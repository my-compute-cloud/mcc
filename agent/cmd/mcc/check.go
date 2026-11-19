package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/agent"
	"github.com/my-compute-cloud/mcc/agent/internal/api"
	"github.com/my-compute-cloud/mcc/agent/internal/config"
)

// finding is one thing `mcc check` looked at. It has the shape of a problem in the control
// plane's status, so that a program reads both the same way.
type finding struct {
	Code string `json:"code"`
	// ok, warning or error.
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

type report struct {
	OK       bool      `json:"ok"`
	Findings []finding `json:"findings"`
}

func (r *report) ok(code, format string, a ...any) {
	r.Findings = append(r.Findings, finding{Code: code, Severity: "ok", Message: fmt.Sprintf(format, a...)})
}

func (r *report) warn(code, message, fix string) {
	r.Findings = append(r.Findings, finding{Code: code, Severity: "warning", Message: message, Fix: fix})
}

func (r *report) bad(code, message, fix string) {
	r.Findings = append(r.Findings, finding{Code: code, Severity: "error", Message: message, Fix: fix})
}

// check is `mcc check`: what is wrong with this host, from what only the host knows and, when
// someone is signed in, what the control plane knows about it.
func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	r := inspect(ctx, dir)
	r.OK = true
	for _, f := range r.Findings {
		if f.Severity == "error" {
			r.OK = false
		}
	}
	if *asJSON {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, f := range r.Findings {
			mark := map[string]string{"ok": "ok", "warning": " !", "error": " x"}[f.Severity]
			fmt.Printf("%s  %s\n", mark, f.Message)
			if f.Fix != "" {
				fmt.Printf("      %s\n", f.Fix)
			}
		}
		if r.OK {
			fmt.Println("\nThis host is ready for work.")
		}
	}
	if !r.OK {
		os.Exit(1)
	}
	return nil
}

// lockAgent takes the lock an agent holds on its state directory for as long as it runs, so
// that there is one agent per directory and anything can tell whether one is running. The
// file must stay open; ok is false if another process has the lock.
func lockAgent(dir string) (f *os.File, ok bool, err error) {
	f, err = os.OpenFile(filepath.Join(dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

// agentRunning reports whether an agent is running with this state directory.
func agentRunning(dir string) bool {
	f, ok, err := lockAgent(dir)
	if err != nil {
		return false
	}
	if ok {
		_ = f.Close() // nobody had it
		return false
	}
	return true
}

// serviceLog is where a service started the documented way writes, if that file is there.
func serviceLog() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, "Library", "Logs", "mcc-agent.log")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func lastLines(path string, n int) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func freeGB(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize) >> 30, true
}

func inspect(ctx context.Context, dir string) *report {
	r := &report{}

	// What the host's owner allows. Everything else is judged against it.
	policy, perr := config.LoadPolicy(dir)
	if perr != nil {
		r.bad("policy_invalid", fmt.Sprintf("The policy in %s cannot be used: %v.", config.PolicyPath(dir), perr),
			"Fix the file; `mcc policy check` says what it permits once it is valid. The agent does not start with a policy it cannot read.")
	} else {
		r.ok("policy", "The policy is valid (%s). %s", config.PolicyPath(dir), strings.Join(describePolicy(policy), " "))
	}

	state, serr := config.LoadState(dir)
	if serr != nil {
		r.bad("not_enrolled", "This host is not enrolled.", "Sign in with `mcc auth login`, then `mcc enroll --pool POOL`.")
	} else {
		r.ok("enrolled", "Enrolled with %s as host %s.", state.ServerURL, state.HostID)
	}

	if perr == nil {
		if _, err := newExecutor(dir, policy, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
			r.bad("cannot_run_vms", fmt.Sprintf("This host cannot run VMs: %v.", err),
				map[string]string{"darwin": "Install Tart (https://tart.run) and make sure the agent's service has it on its PATH.", "linux": "Install QEMU and give the agent's account access to /dev/kvm."}[runtime.GOOS])
		} else {
			r.ok("vms", "It can run VMs.")
		}
	}

	if gb, ok := freeGB(dir); ok {
		if gb < 40 {
			r.warn("low_disk", fmt.Sprintf("Only %d GB is free where the agent keeps its state; a macOS image alone is tens of gigabytes.", gb), "Free some space, or prune old images with `tart prune`.")
		} else {
			r.ok("disk", "%d GB is free.", gb)
		}
	}

	if serr == nil {
		if agentRunning(dir) {
			r.ok("agent_running", "The agent is running.")
			// What it has in hand, which is what an upgrade or a shutdown would have to wait for.
			if act, err := askAgent(ctx, dir, http.MethodGet, "/v1/status"); err == nil {
				switch act.Draining {
				case agent.DrainUpgrade:
					r.warn("draining_for_upgrade", fmt.Sprintf("A newer agent is installed. This one takes no new work and switches to it once idle; %s.", describeWork(act)),
						"Nothing to do: it switches by itself. upgrade_wait_minutes in the policy limits how long work may hold it back.")
				case agent.DrainOwner:
					r.warn("draining", fmt.Sprintf("This host was told to drain, so it takes no new work; %s.", describeWork(act)), "`mcc resume` lets it take work again.")
				default:
					r.ok("activity", "Agent %s: %s.", act.Version, describeWork(act))
				}
				if !act.Installed {
					r.warn("local_build", fmt.Sprintf("The agent running is a build put in the service's place: %s.", act.Executable),
						"`mcc use --installed` goes back to the installed agent. So does the next update that changes it.")
				}
			}
		} else {
			fix := "Start it: `mcc run`, or start its service (launchd: `launchctl kickstart -k gui/$(id -u)/org.nixos.mcc-agent`; systemd: `systemctl start mcc-agent`)."
			if p := serviceLog(); p != "" {
				if tail := lastLines(p, 5); len(tail) > 0 {
					fix += " The end of " + p + ":\n        " + strings.Join(tail, "\n        ")
				}
			}
			r.bad("agent_not_running", "The agent is not running, so this host takes no work.", fix)
		}
	}

	// What the control plane knows, if someone is signed in to ask.
	c, err := api.New()
	if errors.Is(err, api.ErrSignedOut) {
		r.warn("not_signed_in", "Not signed in, so only this machine was looked at.", "`mcc auth login`, then `mcc check` again to see what the control plane says about this host.")
		return r
	}
	if err != nil {
		r.warn("credentials", fmt.Sprintf("The saved sign-in cannot be read: %v.", err), "`mcc auth login` again.")
		return r
	}
	if serr != nil {
		return r
	}
	if strings.TrimSuffix(c.Server, "/") != strings.TrimSuffix(state.ServerURL, "/") {
		r.warn("signed_in_elsewhere", fmt.Sprintf("Signed in to %s, but this host is enrolled with %s.", c.Server, state.ServerURL), "`mcc auth login --server "+state.ServerURL+"`.")
		return r
	}
	var st statusView
	if _, err := c.Do(ctx, http.MethodGet, "/api/v1/status", nil, &st); err != nil {
		r.warn("control_plane", fmt.Sprintf("The control plane could not be asked: %v.", err), "")
		return r
	}
	var me *hostView
	for i := range st.Hosts {
		if st.Hosts[i].ID == state.HostID {
			me = &st.Hosts[i]
		}
	}
	if me == nil {
		r.bad("host_unknown", "The account you are signed in to has no such host. It was revoked, or it belongs to another account.",
			"If it was revoked: remove state.json and identity.key from "+dir+", then `mcc enroll --pool POOL` again.")
		return r
	}
	if me.Connected {
		r.ok("connected", "The control plane sees this host as %s, connected.", me.Name)
	} else if agentRunning(dir) {
		r.bad("not_connected", "The agent is running but the control plane does not see it connected.", "It may be unable to reach "+state.ServerURL+"; its log says why.")
	}
	if len(me.Pools) == 0 {
		r.bad("host_in_no_pool", "This host is in no pool, so it is given no work.", "`mcc pool add-host POOL "+me.Name+"`.")
	}

	var images []imageView
	_, _ = c.Do(ctx, http.MethodGet, "/api/v1/images", nil, &images)
	sourceOf := map[string]string{}
	for _, i := range images {
		src := i.Source
		if s := i.HostSources[runtime.GOOS]; s != "" {
			src = s
		}
		sourceOf[i.Name] = src
	}
	for _, p := range st.Pools {
		in := false
		for _, h := range p.Hosts {
			in = in || h.ID == state.HostID
		}
		if !in {
			continue
		}
		switch {
		case p.Isolation == "host" && perr == nil && !policy.HostTasks.Enabled:
			r.bad("policy_refuses_host_work", fmt.Sprintf("Pool %s runs its work on the host itself, which this host's policy does not allow.", p.Name),
				"Set host_tasks in "+config.PolicyPath(dir)+" if that is what you want, or `mcc pool remove-host "+p.Name+" "+me.Name+"`.")
		case p.Isolation == "host":
			r.ok("pool", "Pool %s (runs-on: %s): work runs on this host itself.", p.Name, p.Label)
		case p.Image != nil && perr == nil && !policy.ImageAllowed(sourceOf[p.Image.Name]):
			r.bad("policy_refuses_image", fmt.Sprintf("Pool %s uses image %s (%s), which this host's allowed_images refuses.", p.Name, p.Image.Name, sourceOf[p.Image.Name]),
				"Add a pattern for it to allowed_images in "+config.PolicyPath(dir)+", or use another image.")
		case p.Image != nil && !contains(me.ImagesReady, p.Image.Name):
			r.warn("image_not_ready", fmt.Sprintf("Pool %s: image %s is not ready on this host yet. The first download of a macOS image takes a long time.", p.Name, p.Image.Name),
				"Watch the agent's log; `tart list` shows what has arrived.")
		case p.Image != nil:
			r.ok("pool", "Pool %s (runs-on: %s): image %s is ready.", p.Name, p.Label, p.Image.Name)
		}
	}

	if perr == nil && len(policy.AllowedGitHubURLs) > 0 {
		for _, g := range st.GitHub {
			account := "https://github.com/" + strings.ToLower(g.Account)
			allowed := false
			for _, u := range policy.AllowedGitHubURLs {
				u = strings.TrimSuffix(strings.ToLower(u), "/")
				allowed = allowed || u == account || strings.HasPrefix(u, account+"/")
			}
			if !allowed {
				r.warn("policy_refuses_github", fmt.Sprintf("Jobs arrive from %s, but this host's allowed_github_urls has nothing of theirs, so it will refuse them.", g.Account),
					"Add "+account+" (an organization) or the repositories' URLs (a personal account) to allowed_github_urls in "+config.PolicyPath(dir)+".")
			}
		}
	}
	if len(st.GitHub) == 0 {
		r.warn("no_github_installation", "No GitHub account is connected, so no GitHub Actions job will arrive.", "`mcc github` says how to connect one.")
	}
	return r
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// describePolicy says in words what a policy lets the control plane ask of this host.
func describePolicy(p config.Policy) []string {
	var out []string
	out = append(out, fmt.Sprintf("Up to %d VMs at once.", p.MaxVMs))
	if len(p.AllowedImages) == 0 {
		out = append(out, "Any image.")
	} else {
		out = append(out, "Images matching: "+strings.Join(p.AllowedImages, ", ")+".")
	}
	switch {
	case !p.WorkloadAllowed("github-runner"):
		out = append(out, "No GitHub Actions jobs.")
	case len(p.AllowedGitHubURLs) == 0:
		out = append(out, "GitHub Actions jobs from any account connected to yours.")
	default:
		out = append(out, "GitHub Actions jobs only from: "+strings.Join(p.AllowedGitHubURLs, ", ")+".")
	}
	if p.WorkloadAllowed("script") {
		out = append(out, "Callers' own scripts, inside VMs.")
	} else {
		out = append(out, "No callers' scripts.")
	}
	if !p.HostTasks.Enabled {
		out = append(out, "Nothing runs on the host itself.")
	} else {
		as := "a dedicated account, " + p.HostTasks.User
		if p.HostTasks.User == "" {
			as = "the agent's own account, which can read the agent's key and your files"
		}
		out = append(out, fmt.Sprintf("Up to %d runs on the host itself, as %s.", p.HostTasks.Slots(), as))
		if p.HostTasks.GitHubRunner {
			out = append(out, "GitHub Actions jobs may run on the host itself.")
		}
		var names []string
		for name := range p.Tasks {
			names = append(names, name)
		}
		if len(names) > 0 {
			out = append(out, "Declared tasks: "+strings.Join(names, ", ")+".")
		}
	}
	if p.ActiveHours != "" {
		out = append(out, "Only during "+p.ActiveHours+".")
	}
	if p.LogLevel == "debug" {
		out = append(out, "Debug logging.")
	}
	if w := p.UpgradeWait(); w > 0 {
		out = append(out, fmt.Sprintf("A newer agent waits up to %s for running work before it takes over.", w))
	} else {
		out = append(out, "A newer agent waits for running work for as long as it takes.")
	}
	if p.TaskOutput.Listen != "" {
		out = append(out, "Serves task output to other machines on "+p.TaskOutput.Listen+".")
	}
	if p.TaskOutput.Upload {
		out = append(out, "Uploads task output to the control plane.")
	}
	return out
}

// policyCheck is `mcc policy check [FILE]`: whether a policy is valid, and what it permits.
func policyCheck(args []string) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	path := config.PolicyPath(dir)
	if len(args) == 1 {
		path = args[0]
		// LoadPolicy reads the file this names.
		os.Setenv("MCC_POLICY_FILE", path)
	} else if len(args) > 1 {
		return errors.New("usage: mcc policy check [FILE]")
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if len(args) == 1 {
			return fmt.Errorf("%s does not exist", path)
		}
		fmt.Printf("There is no policy file at %s, so the defaults apply:\n", path)
	} else {
		fmt.Printf("%s\n", path)
	}
	p, err := config.LoadPolicy(dir)
	if err != nil {
		return err
	}
	for _, line := range describePolicy(p) {
		fmt.Println("  " + line)
	}
	return nil
}
