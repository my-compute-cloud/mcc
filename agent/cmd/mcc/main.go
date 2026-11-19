// Command mcc is the My Compute Cloud agent.
//
//	mcc enroll --server URL [--name NAME] TOKEN   register this host with the control plane
//	mcc run                                       connect and run work (normally as a service)
//	mcc policy                                    print the effective local policy
//	mcc version
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/agent"
	"github.com/my-compute-cloud/mcc/agent/internal/cache"
	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/kvm"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/process"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/tart"
	"github.com/my-compute-cloud/mcc/agent/internal/identity"
	"github.com/my-compute-cloud/mcc/agent/internal/runnerbin"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "enroll":
		err = enroll(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "policy":
		err = showPolicy()
	case "version":
		fmt.Println(agent.Version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcc:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mcc enroll --server URL [--name NAME] TOKEN | mcc run | mcc policy | mcc version")
	os.Exit(2)
}

func enroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "control plane URL")
	host, _ := os.Hostname()
	name := fs.String("name", strings.TrimSuffix(host, ".local"), "host name shown in the control plane")
	_ = fs.Parse(args)
	if *server == "" || fs.NArg() != 1 {
		return fmt.Errorf("usage: mcc enroll --server URL [--name NAME] TOKEN")
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	if _, err := config.LoadState(dir); err == nil {
		return fmt.Errorf("this host is already enrolled (state in %s)", dir)
	}
	id, err := identity.Generate(dir)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{
		"token":      fs.Arg(0),
		"name":       *name,
		"public_key": id.PublicKey(),
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
	})
	res, err := http.Post(strings.TrimSuffix(*server, "/")+"/api/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		_ = os.Remove(filepath.Join(dir, "identity.key"))
		return err
	}
	defer res.Body.Close()
	var out struct {
		HostID   string `json:"host_id"`
		TenantID string `json:"tenant_id"`
		Error    string `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusCreated {
		_ = os.Remove(filepath.Join(dir, "identity.key"))
		return fmt.Errorf("enrollment failed: %s %s", res.Status, out.Error)
	}
	if err := config.SaveState(dir, config.State{ServerURL: *server, HostID: out.HostID, TenantID: out.TenantID}); err != nil {
		return err
	}
	fmt.Printf("Enrolled as host %s. Start the agent with `mcc run` (or enable its service).\n", out.HostID)
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	verbose := fs.Bool("v", false, "debug logging")
	_ = fs.Parse(args)
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	state, err := config.LoadState(dir)
	if err != nil {
		return err
	}
	if s := os.Getenv("MCC_SERVER_URL"); s != "" {
		state.ServerURL = s
	}
	id, err := identity.Load(dir)
	if err != nil {
		return err
	}
	policy, err := config.LoadPolicy(dir)
	if err != nil {
		return err
	}

	ex, err := newExecutor(dir, policy, log)
	if err != nil {
		return err
	}
	if k, ok := ex.(*kvm.KVM); ok {
		for _, problem := range k.Preflight() {
			log.Warn("this host is not ready to run VMs", "problem", problem)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One Actions runner per guest platform this host can run: macOS and Linux guests each
	// need their own build.
	// Work on the host itself, where the owner allows it. It has no image to bring a runner,
	// so GitHub jobs there need the runner built for this host's own system.
	var hostEx executor.Executor
	if policy.HostTasks.Enabled {
		work := policy.HostTasks.Dir
		if work == "" {
			work = filepath.Join(dir, "work")
		}
		px := process.New(work, filepath.Join(dir, "host-jobs"), policy.HostTasks.User, log)
		for _, problem := range px.Preflight(ctx) {
			log.Warn("this host is not ready to run work without a VM", "problem", problem)
		}
		hostEx = px
		log.Info("this host runs work without a VM", "dir", work, "user", policy.HostTasks.User, "at_once", policy.HostTasks.Slots())
	}

	var runners runnerSet
	var platforms []string
	if policy.SharedRunnerEnabled() {
		platforms = ex.GuestPlatforms()
	}
	if policy.HostTasks.Enabled && policy.HostTasks.GitHubRunner && !slices.Contains(platforms, runtime.GOOS) {
		platforms = append(platforms, runtime.GOOS)
	}
	if len(platforms) > 0 && os.Getenv("MCC_EXECUTOR") != "fake" {
		runners = runnerSet{}
		for _, platform := range platforms {
			switch platform {
			case "darwin":
				runners[platform] = runnerbin.New(filepath.Join(dir, "runner"), "osx-"+runnerArch())
			case "linux":
				runners[platform] = runnerbin.New(filepath.Join(dir, "runner-linux"), "linux-"+runnerArch())
			}
		}
		for platform, m := range runners {
			go keepRunnerCurrent(ctx, platform, m, log)
		}
	}

	var jobCache agent.CacheSource
	if policy.CacheDir != "" {
		c := cache.New(policy.CacheDir, policy.CacheScope, policy.CacheMaxGB, log)
		go c.Prune()
		jobCache = c
	}

	a := agent.New(agent.Options{
		ServerURL:    state.ServerURL,
		HostID:       state.HostID,
		Identity:     id,
		Policy:       policy,
		Executor:     ex,
		HostExecutor: hostEx,
		Runner:       runnerSource(runners),
		Cache:        jobCache,
		Log:          log,
		Resources:    resources(policy),
	})
	log.Info("starting agent", "version", agent.Version, "host", state.HostID, "server", state.ServerURL)
	return a.Run(ctx)
}

// newExecutor is how this host runs VMs: Tart on a Mac, QEMU with KVM on Linux. MCC_EXECUTOR
// names another, which is for tests.
func newExecutor(dir string, policy config.Policy, log *slog.Logger) (executor.Executor, error) {
	name := os.Getenv("MCC_EXECUTOR")
	if name == "" {
		name = map[string]string{"darwin": "tart", "linux": "kvm"}[runtime.GOOS]
	}
	switch name {
	case "fake":
		f := fake.New()
		f.JobDuration = 200 * time.Millisecond
		return f, nil
	case "tart":
		t := tart.New(log)
		t.Xcode = policy.Xcode
		t.Preload = policy.PreloadImages
		return t, nil
	case "kvm":
		if policy.Xcode != "" {
			log.Warn("the xcode policy only applies to Mac hosts and is ignored")
		}
		k := kvm.New(filepath.Join(dir, "kvm"), log)
		k.Preload = policy.PreloadImages
		return k, nil
	case "":
		return nil, fmt.Errorf("mcc does not run VMs on %s", runtime.GOOS)
	}
	return nil, fmt.Errorf("unknown MCC_EXECUTOR %q", name)
}

// runnerSet holds the runner for each guest platform.
type runnerSet map[string]*runnerbin.Manager

func (r runnerSet) Current(platform string) string {
	if m := r[platform]; m != nil {
		return m.Current()
	}
	return ""
}

func runnerSource(r runnerSet) agent.RunnerSource {
	if r == nil {
		return nil
	}
	return r
}

func keepRunnerCurrent(ctx context.Context, platform string, m *runnerbin.Manager, log *slog.Logger) {
	for {
		dir, err := m.Update(ctx)
		if err != nil {
			log.Warn("updating Actions runner failed; VMs will use the image's runner if none is cached", "guests", platform, "err", err)
		} else {
			log.Info("Actions runner ready", "guests", platform, "dir", dir)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(12 * time.Hour):
		}
	}
}

// runnerArch is the architecture in the Actions runner's build names. Guests have the host's.
func runnerArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return "arm64"
}

// resources applies the policy. By default each VM gets an equal share of the host, as if
// max_vms of them ran at once, and never more than half: a host with max_vms 2 or less gives
// each VM half, as it always has.
func resources(p config.Policy) executor.Resources {
	r := executor.Resources{CPUs: p.CPUs, MemoryMB: p.MemoryMB, DiskGB: p.DiskGB}
	share := max(2, p.MaxVMs)
	if r.CPUs == 0 {
		r.CPUs = max(1, runtime.NumCPU()/share)
	}
	if r.MemoryMB == 0 {
		if total := hostMemoryMB(); total > 0 {
			r.MemoryMB = total / share
		}
	}
	return r
}

func hostMemoryMB() int {
	if runtime.GOOS != "darwin" {
		return kvm.HostMemoryMB()
	}
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	b, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return int(b / (1024 * 1024))
}

func showPolicy() error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	p, err := config.LoadPolicy(dir)
	if err != nil {
		return err
	}
	ex, err := newExecutor(dir, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(struct {
		config.Policy
		File                  string             `json:"file"`
		EffectiveResources    executor.Resources `json:"effective_resources"`
		NetworkIsolationOn    bool               `json:"network_isolation_effective"`
		StayAwakeEffective    bool               `json:"stay_awake_effective"`
		SharedRunnerEffective bool               `json:"shared_runner_effective"`
	}{p, config.PolicyPath(dir), resources(p), p.NetworkIsolation() && ex.NetworkIsolationAvailable(), p.StayAwakeEnabled(), p.SharedRunnerEnabled()}, "", "  ")
	fmt.Println(string(b))
	return nil
}
