package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/api"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// The commands that manage an account: images, pools, hosts, tasks, webhooks and tokens. Each
// is a thin caller of the HTTP API, and prints JSON with --json for programs to read.

// cmd is one invocation: its client, its context, and whether to print JSON.
type cmd struct {
	c    *api.Client
	ctx  context.Context
	json bool
	args []string
}

// parse reads flags that may come before or after the other arguments.
func parse(fs *flag.FlagSet, args []string) []string {
	var rest []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			return rest
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// start prepares a command that needs to be signed in. It adds --json to fs.
func start(fs *flag.FlagSet, args []string) (*cmd, context.CancelFunc, error) {
	asJSON := fs.Bool("json", false, "print JSON")
	rest := parse(fs, args)
	c, err := api.New()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := interruptible()
	return &cmd{c: c, ctx: ctx, json: *asJSON, args: rest}, cancel, nil
}

func printRaw(ctx context.Context, c *api.Client, path string) error {
	var raw json.RawMessage
	if _, err := c.Do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	return printJSON(raw)
}

func printJSON(raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
	return nil
}

// get fetches path into out, or prints it as JSON and reports that it did.
func (k *cmd) get(path string, out any) (printed bool, err error) {
	if k.json {
		return true, printRaw(k.ctx, k.c, path)
	}
	_, err = k.c.Do(k.ctx, http.MethodGet, path, nil, out)
	return false, err
}

// send makes a change. With --json it prints what the API answered.
func (k *cmd) send(method, path string, in, out any) error {
	var raw json.RawMessage
	if _, err := k.c.Do(k.ctx, method, path, in, &raw); err != nil {
		return err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
	}
	if k.json && len(raw) > 0 {
		return printJSON(raw)
	}
	return nil
}

func (k *cmd) need(n int, usage string) error {
	if len(k.args) != n {
		return errors.New("usage: " + usage)
	}
	return nil
}

func table(header string, rows func(w io.Writer)) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, header)
	rows(w)
	_ = w.Flush()
}

func seg(s string) string { return url.PathEscape(s) }

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	d := time.Since(*t).Round(time.Second)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func yes(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ----- images -----

type imageView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Source      string            `json:"source"`
	Platform    string            `json:"platform"`
	HostSources map[string]string `json:"host_sources"`
}

func image(args []string) error {
	const usage = "mcc image list | add NAME SOURCE [--platform darwin|linux] [--linux-host SOURCE] [--mac-host SOURCE] | set-source NAME SOURCE | rm NAME"
	if len(args) == 0 {
		return errors.New("usage: " + usage)
	}
	fs := flag.NewFlagSet("image "+args[0], flag.ExitOnError)
	platform := fs.String("platform", "darwin", "what the image's guests run: darwin or linux")
	linuxHost := fs.String("linux-host", "", "a different source for hosts that run Linux")
	macHost := fs.String("mac-host", "", "a different source for hosts that are Macs")
	k, cancel, err := start(fs, args[1:])
	if err != nil {
		return err
	}
	defer cancel()
	switch args[0] {
	case "list", "ls":
		var images []imageView
		if printed, err := k.get("/api/v1/images", &images); printed || err != nil {
			return err
		}
		if len(images) == 0 {
			fmt.Println("No images. Add one: mcc image add NAME SOURCE")
			return nil
		}
		table("NAME\tGUEST\tSOURCE", func(w io.Writer) {
			for _, i := range images {
				fmt.Fprintf(w, "%s\t%s\t%s\n", i.Name, i.Platform, i.Source)
				for os, src := range i.HostSources {
					fmt.Fprintf(w, "\t\t  on %s hosts: %s\n", os, src)
				}
			}
		})
		return nil
	case "add":
		if err := k.need(2, "mcc image add NAME SOURCE"); err != nil {
			return err
		}
		body := map[string]any{"name": k.args[0], "source": k.args[1], "platform": *platform}
		hs := map[string]string{}
		if *linuxHost != "" {
			hs["linux"] = *linuxHost
		}
		if *macHost != "" {
			hs["darwin"] = *macHost
		}
		if len(hs) > 0 {
			body["host_sources"] = hs
		}
		if err := k.send(http.MethodPost, "/api/v1/images", body, nil); err != nil {
			return err
		}
		if !k.json {
			fmt.Printf("Added image %s.\n", k.args[0])
		}
		return nil
	case "set-source":
		if err := k.need(2, "mcc image set-source NAME SOURCE"); err != nil {
			return err
		}
		if err := k.send(http.MethodPatch, "/api/v1/images/"+seg(k.args[0]), map[string]string{"source": k.args[1]}, nil); err != nil {
			return err
		}
		if !k.json {
			fmt.Printf("Image %s now comes from %s. Hosts that use it will fetch it.\n", k.args[0], k.args[1])
		}
		return nil
	case "rm":
		if err := k.need(1, "mcc image rm NAME"); err != nil {
			return err
		}
		if err := k.send(http.MethodDelete, "/api/v1/images/"+seg(k.args[0]), nil, nil); err != nil {
			return err
		}
		if !k.json {
			fmt.Printf("Removed image %s.\n", k.args[0])
		}
		return nil
	}
	return errors.New("usage: " + usage)
}

// ----- pools -----

type poolHostView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Ready     bool   `json:"ready"`
	NotReady  string `json:"not_ready"`
	// WarmVMs is this host's share of the pool's warm VMs.
	WarmVMs int `json:"warm_vms"`
}

type poolView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label"`
	// Also lists what a workflow may add to runs-on to choose among the pool's hosts.
	Also      []string `json:"also"`
	Isolation string   `json:"isolation"`
	// WarmVMs is how many VMs of its image its hosts keep booted and waiting, in all. The
	// control plane divides them among the hosts.
	WarmVMs int `json:"warm_vms"`
	Image   *struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"image"`
	Flagged *string        `json:"flagged_reason"`
	Hosts   []poolHostView `json:"hosts"`
	Queued  int            `json:"queued"`
	Running int            `json:"running"`
}

func (p poolView) runs() string {
	if p.Isolation == "host" {
		return "on the host itself"
	}
	runs := "VMs"
	if p.Image != nil {
		runs = "VMs from " + p.Image.Name
	}
	if p.WarmVMs > 0 {
		runs += fmt.Sprintf(", %d warm in the pool", p.WarmVMs)
	}
	return runs
}

func pool(args []string) error {
	const usage = "mcc pool list | show POOL | create NAME --label LABEL (--image IMAGE [--warm N] | --isolation host) | rename POOL NAME | set-image POOL IMAGE | set-warm POOL N | add-host POOL HOST | remove-host POOL HOST | clear-flag POOL | rm POOL"
	if len(args) == 0 {
		return errors.New("usage: " + usage)
	}
	fs := flag.NewFlagSet("pool "+args[0], flag.ExitOnError)
	label := fs.String("label", "", "what workflows say in runs-on to use this pool")
	img := fs.String("image", "", "the image its VMs boot from")
	isolation := fs.String("isolation", "vm", "vm, or host to run its work on the host itself with no VM")
	warm := fs.Int("warm", 0, "how many VMs of its image its hosts keep booted and waiting in all, so a job need not wait for a boot")
	k, cancel, err := start(fs, args[1:])
	if err != nil {
		return err
	}
	defer cancel()
	done := func(format string, a ...any) error {
		if !k.json {
			fmt.Printf(format+"\n", a...)
		}
		return nil
	}
	switch args[0] {
	case "list", "ls":
		var st statusView
		if k.json {
			return printRaw(k.ctx, k.c, "/api/v1/pools")
		}
		if _, err := k.c.Do(k.ctx, http.MethodGet, "/api/v1/status", nil, &st); err != nil {
			return err
		}
		printPools(st.Pools)
		return nil
	case "show":
		if err := k.need(1, "mcc pool show POOL"); err != nil {
			return err
		}
		var p poolView
		if printed, err := k.get("/api/v1/pools/"+seg(k.args[0]), &p); printed || err != nil {
			return err
		}
		printPools([]poolView{p})
		return nil
	case "create":
		if len(k.args) != 1 || *label == "" || (*isolation != "host" && *img == "") {
			return errors.New("usage: mcc pool create NAME --label LABEL (--image IMAGE [--warm N] | --isolation host)")
		}
		body := map[string]any{"name": k.args[0], "label": *label, "isolation": *isolation}
		if *isolation != "host" {
			body["image_id"] = *img
		}
		// Sent only when asked for, so that the control plane says why a host pool cannot have it.
		if *warm != 0 {
			body["warm_vms"] = *warm
		}
		if err := k.send(http.MethodPost, "/api/v1/pools", body, nil); err != nil {
			return err
		}
		return done("Created pool %s. Workflows use it with runs-on: %s. It has no hosts yet: mcc pool add-host %s HOST, or mcc enroll --pool %s on a new host.", k.args[0], *label, k.args[0], k.args[0])
	case "rename":
		if err := k.need(2, "mcc pool rename POOL NAME"); err != nil {
			return err
		}
		if err := k.send(http.MethodPatch, "/api/v1/pools/"+seg(k.args[0]), map[string]string{"name": k.args[1]}, nil); err != nil {
			return err
		}
		return done("Renamed %s to %s.", k.args[0], k.args[1])
	case "set-image":
		if err := k.need(2, "mcc pool set-image POOL IMAGE"); err != nil {
			return err
		}
		if err := k.send(http.MethodPut, "/api/v1/pools/"+seg(k.args[0])+"/image", map[string]string{"image_id": k.args[1]}, nil); err != nil {
			return err
		}
		return done("Pool %s now uses image %s.", k.args[0], k.args[1])
	case "set-warm":
		if err := k.need(2, "mcc pool set-warm POOL N"); err != nil {
			return err
		}
		n, err := strconv.Atoi(k.args[1])
		if err != nil {
			return fmt.Errorf("the number of warm VMs must be a whole number, not %q", k.args[1])
		}
		if err := k.send(http.MethodPut, "/api/v1/pools/"+seg(k.args[0])+"/warm", map[string]int{"warm_vms": n}, nil); err != nil {
			return err
		}
		if n == 0 {
			return done("Pool %s keeps no VMs warm. Its hosts stop the ones waiting now.", k.args[0])
		}
		return done("Pool %s keeps %d VM%s of its image booted and waiting, divided among its hosts as their owners allow.", k.args[0], n, yes(n == 1, "", "s"))
	case "add-host", "remove-host":
		if err := k.need(2, "mcc pool "+args[0]+" POOL HOST"); err != nil {
			return err
		}
		method := http.MethodPut
		if args[0] == "remove-host" {
			method = http.MethodDelete
		}
		if err := k.send(method, "/api/v1/pools/"+seg(k.args[0])+"/hosts/"+seg(k.args[1]), nil, nil); err != nil {
			return err
		}
		return done("%s %s %s.", yes(args[0] == "add-host", "Added", "Removed"), k.args[1], yes(args[0] == "add-host", "to "+k.args[0], "from "+k.args[0]))
	case "clear-flag":
		if err := k.need(1, "mcc pool clear-flag POOL"); err != nil {
			return err
		}
		if err := k.send(http.MethodPost, "/api/v1/pools/"+seg(k.args[0])+"/clear-flag", nil, nil); err != nil {
			return err
		}
		return done("Pool %s takes work again.", k.args[0])
	case "rm":
		if err := k.need(1, "mcc pool rm POOL"); err != nil {
			return err
		}
		if err := k.send(http.MethodDelete, "/api/v1/pools/"+seg(k.args[0]), nil, nil); err != nil {
			return err
		}
		return done("Removed pool %s.", k.args[0])
	}
	return errors.New("usage: " + usage)
}

func printPools(pools []poolView) {
	if len(pools) == 0 {
		fmt.Println("No pools. Create one: mcc pool create NAME --label LABEL --image IMAGE")
		return
	}
	table("POOL\tRUNS-ON\tMAY ADD\tRUNS\tHOSTS\tQUEUED\tRUNNING\tSTATE", func(w io.Writer) {
		for _, p := range pools {
			ready := 0
			for _, h := range p.Hosts {
				if h.Ready {
					ready++
				}
			}
			state := "ok"
			if p.Flagged != nil && *p.Flagged != "" {
				state = "set aside: " + *p.Flagged
			} else if ready == 0 {
				state = "no host ready"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d of %d ready\t%d\t%d\t%s\n", p.Name, p.Label, or(strings.Join(p.Also, ", "), "-"), p.runs(), ready, len(p.Hosts), p.Queued, p.Running, state)
		}
	})
}

// ----- hosts -----

type slots struct {
	Total int `json:"total"`
	Free  int `json:"free"`
}

type hostView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Connected   bool       `json:"connected"`
	Accepting   bool       `json:"accepting"`
	LastSeenAt  *time.Time `json:"last_seen_at"`
	Version     string     `json:"agent_version"`
	OS          string     `json:"os"`
	Arch        string     `json:"arch"`
	VMSlots     slots      `json:"vm_slots"`
	HostSlots   slots      `json:"host_slots"`
	Pools       []string   `json:"pools"`
	ImagesReady []string   `json:"images_ready"`
	ImagesWarm  []string   `json:"images_warm"`
	// WarmVMs is the most warm VMs its owner lets it keep, over all its pools.
	WarmVMs int      `json:"warm_vms"`
	Tasks   []string `json:"tasks"`
	Scripts bool     `json:"scripts"`
	// Policy is what the host said it runs under when its agent last connected. Nil from an
	// agent that does not say.
	Policy *protocol.HostPolicy `json:"policy"`
}

// describeReportedPolicy says in words what a host told the control plane of its policy.
func describeReportedPolicy(p protocol.HostPolicy) []string {
	images := "any image of the account"
	if len(p.AllowedImages) > 0 {
		images = "matching " + strings.Join(p.AllowedImages, ", ")
	}
	if len(p.ExcludedImages) > 0 {
		images += ", except " + strings.Join(p.ExcludedImages, ", ")
	}
	out := []string{
		"Images        " + images,
		"Runners for   " + or(strings.Join(p.AllowedGithubUrls, ", "), "any repository or organization"),
		"Work          " + or(strings.Join(p.AllowedWorkloads, ", "), "github-runner") + yes(p.HostTasks, ", and tasks on the host itself", ""),
		"Hours         " + or(p.ActiveHours, "always"),
	}
	cache := "none"
	if p.Cache != "" {
		cache = "one per " + yes(p.Cache == "host", "host", "repository or organization")
	}
	out = append(out, "Job cache     "+cache+yes(p.ShareNixStore, "; its Nix store is shared with jobs, read-only", ""))
	out = append(out, "Network       "+yes(p.IsolateNetwork, "VMs reach the internet only", "VMs share the host's network")+yes(p.RequireNetworkIsolation, ", or do not run", ""))
	return out
}

func (h hostView) state() string {
	switch {
	case !h.Connected:
		return "offline, seen " + ago(h.LastSeenAt)
	case !h.Accepting:
		return "connected, taking no work now"
	}
	return "connected"
}

func host(args []string) error {
	const usage = "mcc host list | show HOST | revoke HOST"
	if len(args) == 0 {
		return errors.New("usage: " + usage)
	}
	k, cancel, err := start(flag.NewFlagSet("host "+args[0], flag.ExitOnError), args[1:])
	if err != nil {
		return err
	}
	defer cancel()
	switch args[0] {
	case "list", "ls":
		if k.json {
			return printRaw(k.ctx, k.c, "/api/v1/hosts")
		}
		var st statusView
		if _, err := k.c.Do(k.ctx, http.MethodGet, "/api/v1/status", nil, &st); err != nil {
			return err
		}
		printHosts(st.Hosts)
		return nil
	case "show":
		if err := k.need(1, "mcc host show HOST"); err != nil {
			return err
		}
		if k.json {
			return printRaw(k.ctx, k.c, "/api/v1/hosts/"+seg(k.args[0]))
		}
		var st statusView
		if _, err := k.c.Do(k.ctx, http.MethodGet, "/api/v1/status", nil, &st); err != nil {
			return err
		}
		for _, h := range st.Hosts {
			if h.Name != k.args[0] && h.ID != k.args[0] {
				continue
			}
			fmt.Printf("%s (%s)\n", h.Name, h.ID)
			fmt.Printf("  State         %s\n", h.state())
			fmt.Printf("  System        %s/%s, agent %s\n", h.OS, h.Arch, or(h.Version, "unknown"))
			fmt.Printf("  VMs           %d free of %d\n", h.VMSlots.Free, h.VMSlots.Total)
			fmt.Printf("  On the host   %s\n", yes(h.HostSlots.Total > 0, fmt.Sprintf("%d free of %d", h.HostSlots.Free, h.HostSlots.Total), "not allowed by its policy"))
			fmt.Printf("  Pools         %s\n", or(strings.Join(h.Pools, ", "), "none"))
			fmt.Printf("  Images ready  %s\n", or(strings.Join(h.ImagesReady, ", "), "none"))
			warm := "none allowed by its policy"
			if h.WarmVMs > 0 {
				warm = fmt.Sprintf("up to %d; waiting now: %s", h.WarmVMs, or(strings.Join(h.ImagesWarm, ", "), "none"))
			}
			fmt.Printf("  Warm VMs      %s\n", warm)
			for _, p := range st.Pools {
				for _, ph := range p.Hosts {
					if ph.ID == h.ID && ph.WarmVMs > 0 {
						fmt.Printf("    %d for pool %s\n", ph.WarmVMs, p.Name)
					}
				}
			}
			fmt.Printf("  Tasks         %s\n", or(strings.Join(h.Tasks, ", "), "none declared"))
			fmt.Printf("  Scripts       %s\n", yes(h.Scripts, "allowed in its VMs", "not allowed"))
			if h.Policy == nil {
				fmt.Println("  Policy        not reported; its agent is older than the report, or has never connected")
			} else {
				fmt.Println("  Policy, as it reported when its agent last started:")
				for _, l := range describeReportedPolicy(*h.Policy) {
					fmt.Println("    " + l)
				}
			}
			for _, p := range st.Problems {
				if p.Subject.Kind == "host" && p.Subject.ID == h.ID {
					fmt.Printf("  %s: %s\n    %s\n", strings.ToUpper(p.Severity), p.Message, p.Fix)
				}
			}
			return nil
		}
		return fmt.Errorf("no host named %s", k.args[0])
	case "revoke":
		if err := k.need(1, "mcc host revoke HOST"); err != nil {
			return err
		}
		if err := k.send(http.MethodDelete, "/api/v1/hosts/"+seg(k.args[0]), nil, nil); err != nil {
			return err
		}
		if !k.json {
			fmt.Printf("Revoked %s. Its key no longer works; to use the machine again, enroll it again.\n", k.args[0])
		}
		return nil
	}
	return errors.New("usage: " + usage)
}

func printHosts(hosts []hostView) {
	if len(hosts) == 0 {
		fmt.Println("No hosts. On the machine that should run work: mcc enroll --pool POOL")
		return
	}
	table("HOST\tSTATE\tSYSTEM\tVMS FREE\tON HOST FREE\tPOOLS", func(w io.Writer) {
		for _, h := range hosts {
			onHost := "-"
			if h.HostSlots.Total > 0 {
				onHost = fmt.Sprintf("%d of %d", h.HostSlots.Free, h.HostSlots.Total)
			}
			fmt.Fprintf(w, "%s\t%s\t%s/%s\t%d of %d\t%s\t%s\n", h.Name, h.state(), h.OS, h.Arch, h.VMSlots.Free, h.VMSlots.Total, onHost, or(strings.Join(h.Pools, ", "), "none"))
		}
	})
}

// ----- status -----

type problemView struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Subject  struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"subject"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

type statusView struct {
	OK     bool       `json:"ok"`
	Pools  []poolView `json:"pools"`
	Hosts  []hostView `json:"hosts"`
	GitHub []struct {
		Account   string `json:"account"`
		Suspended bool   `json:"suspended"`
	} `json:"github"`
	Problems []problemView `json:"problems"`
}

func printProblems(problems []problemView) {
	for _, p := range problems {
		mark := "!"
		if p.Severity == "error" {
			mark = "x"
		}
		fmt.Printf("  %s %s\n      %s\n", mark, p.Message, p.Fix)
	}
}

// status is `mcc status`: the account as the control plane sees it.
func status(args []string) error {
	k, cancel, err := start(flag.NewFlagSet("status", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	defer cancel()
	var st statusView
	if printed, err := k.get("/api/v1/status", &st); printed || err != nil {
		return err
	}
	printPools(st.Pools)
	fmt.Println()
	printHosts(st.Hosts)
	fmt.Println()
	var accounts []string
	for _, g := range st.GitHub {
		accounts = append(accounts, g.Account+yes(g.Suspended, " (suspended)", ""))
	}
	fmt.Printf("GitHub: %s\n", or(strings.Join(accounts, ", "), "no account connected"))
	if len(st.Problems) == 0 {
		fmt.Println("\nNothing is wrong.")
		return nil
	}
	fmt.Println("\nProblems:")
	printProblems(st.Problems)
	return nil
}

// ----- tasks -----

type taskView struct {
	ID       string            `json:"id"`
	Pool     string            `json:"pool"`
	Task     *string           `json:"task"`
	Script   string            `json:"script"`
	Params   map[string]string `json:"params"`
	State    string            `json:"state"`
	ExitCode *int              `json:"exit_code"`
	Code     *string           `json:"code"`
	Reason   *string           `json:"reason"`
	HostID   *string           `json:"host_id"`
	Created  time.Time         `json:"created_at"`
}

func (t taskView) what() string {
	if t.Task != nil {
		return *t.Task
	}
	return "(script)"
}

func (t taskView) ended() bool {
	return t.State == "succeeded" || t.State == "failed" || t.State == "cancelled"
}

type params map[string]string

func (p params) String() string { return "" }
func (p params) Set(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if !ok || key == "" {
		return errors.New("a parameter is KEY=VALUE")
	}
	p[key] = value
	return nil
}

func task(args []string) error {
	const usage = "mcc task list | show TASK | cancel TASK | run --pool POOL (NAME [--param KEY=VALUE]... | --script FILE) [--timeout SECONDS] [--wait]"
	if len(args) == 0 {
		return errors.New("usage: " + usage)
	}
	fs := flag.NewFlagSet("task "+args[0], flag.ExitOnError)
	poolName := fs.String("pool", "", "the pool to run it in")
	script := fs.String("script", "", "a file holding a shell script to run in a VM, or - for standard input")
	timeout := fs.Int("timeout", 0, "seconds it may run; the host's own limit still applies")
	wait := fs.Bool("wait", false, "wait for it to end, and exit as it did")
	p := params{}
	fs.Var(p, "param", "a parameter for a declared task, KEY=VALUE; may be repeated")
	k, cancel, err := start(fs, args[1:])
	if err != nil {
		return err
	}
	defer cancel()
	show := func(t taskView) {
		fmt.Printf("%s  %s in %s: %s", t.ID, t.what(), t.Pool, t.State)
		if t.ExitCode != nil {
			fmt.Printf(", exit code %d", *t.ExitCode)
		}
		if t.Code != nil && *t.Code != "" {
			fmt.Printf(" (%s)", *t.Code)
		}
		fmt.Println()
		if t.Reason != nil && *t.Reason != "" {
			fmt.Printf("  %s\n", *t.Reason)
		}
	}
	switch args[0] {
	case "list", "ls":
		var tasks []taskView
		if printed, err := k.get("/api/v1/tasks", &tasks); printed || err != nil {
			return err
		}
		if len(tasks) == 0 {
			fmt.Println("No tasks have been asked for.")
			return nil
		}
		table("ID\tTASK\tPOOL\tSTATE\tEXIT\tASKED", func(w io.Writer) {
			for _, t := range tasks {
				exit := "-"
				if t.ExitCode != nil {
					exit = fmt.Sprint(*t.ExitCode)
				} else if t.Code != nil && *t.Code != "" {
					exit = *t.Code
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.what(), t.Pool, t.State, exit, ago(&t.Created))
			}
		})
		return nil
	case "show", "cancel":
		if err := k.need(1, "mcc task "+args[0]+" TASK"); err != nil {
			return err
		}
		var t taskView
		if args[0] == "cancel" {
			if err := k.send(http.MethodDelete, "/api/v1/tasks/"+seg(k.args[0]), nil, &t); err != nil {
				return err
			}
		} else if printed, err := k.get("/api/v1/tasks/"+seg(k.args[0]), &t); printed || err != nil {
			return err
		}
		if !k.json {
			show(t)
		}
		return nil
	case "run":
		body := map[string]any{"pool": *poolName}
		switch {
		case *poolName == "":
			return errors.New("usage: " + usage)
		case *script != "":
			if len(k.args) != 0 || len(p) != 0 {
				return errors.New("a script takes no name and no parameters; write the values into it")
			}
			var b []byte
			if *script == "-" {
				b, err = io.ReadAll(os.Stdin)
			} else {
				b, err = os.ReadFile(*script)
			}
			if err != nil {
				return err
			}
			body["script"] = string(b)
		case len(k.args) == 1:
			body["task"] = k.args[0]
			if len(p) > 0 {
				body["params"] = p
			}
		default:
			return errors.New("usage: " + usage)
		}
		if *timeout > 0 {
			body["timeout_seconds"] = *timeout
		}
		// The secret that opens the task's output on its host is the caller's; only its hash is sent.
		if secret := os.Getenv("MCC_TASK_SECRET"); secret != "" {
			sum := sha256.Sum256([]byte(secret))
			body["output_secret_hash"] = hex.EncodeToString(sum[:])
		}
		var t taskView
		quiet := *k
		quiet.json = k.json && !*wait
		if err := quiet.send(http.MethodPost, "/api/v1/tasks", body, &t); err != nil {
			return err
		}
		if !*wait {
			if !k.json {
				show(t)
			}
			return nil
		}
		fmt.Fprintf(os.Stderr, "Task %s is %s; waiting for it to end...\n", t.ID, t.State)
		for !t.ended() {
			select {
			case <-k.ctx.Done():
				return fmt.Errorf("stopped waiting; the task goes on: mcc task show %s", t.ID)
			case <-time.After(2 * time.Second):
			}
			if _, err := k.c.Do(k.ctx, http.MethodGet, "/api/v1/tasks/"+t.ID, nil, &t); err != nil && k.ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "(%v; asking again)\n", err)
			}
		}
		if k.json {
			if err := printRaw(k.ctx, k.c, "/api/v1/tasks/"+t.ID); err != nil {
				return err
			}
		} else {
			show(t)
		}
		if t.State != "succeeded" {
			code := 1
			if t.ExitCode != nil && *t.ExitCode > 0 && *t.ExitCode < 256 {
				code = *t.ExitCode
			}
			cancel()
			os.Exit(code)
		}
		return nil
	}
	return errors.New("usage: " + usage)
}

// ----- webhooks -----

func webhook(args []string) error {
	const usage = "mcc webhook list | create NAME --pool POOL (--task TASK [--param KEY=VALUE]... | --script FILE) | rm ID"
	if len(args) == 0 {
		return errors.New("usage: " + usage)
	}
	fs := flag.NewFlagSet("webhook "+args[0], flag.ExitOnError)
	poolName := fs.String("pool", "", "the pool its task runs in")
	taskName := fs.String("task", "", "the declared task a delivery starts")
	script := fs.String("script", "", "a file holding a shell script a delivery runs in a VM")
	p := params{}
	fs.Var(p, "param", "a parameter every delivery starts the task with, KEY=VALUE; may be repeated")
	k, cancel, err := start(fs, args[1:])
	if err != nil {
		return err
	}
	defer cancel()
	type hookView struct {
		ID           string     `json:"id"`
		Name         string     `json:"name"`
		Pool         *string    `json:"pool"`
		Task         *string    `json:"task"`
		URL          string     `json:"url"`
		Secret       string     `json:"secret"`
		LastDelivery *time.Time `json:"last_delivery_at"`
	}
	switch args[0] {
	case "list", "ls":
		var hooks []hookView
		if printed, err := k.get("/api/v1/webhooks", &hooks); printed || err != nil {
			return err
		}
		if len(hooks) == 0 {
			fmt.Println("No webhooks.")
			return nil
		}
		table("ID\tNAME\tPOOL\tSTARTS\tLAST DELIVERY\tURL", func(w io.Writer) {
			for _, h := range hooks {
				starts, pl := "(script)", "(gone)"
				if h.Task != nil {
					starts = *h.Task
				}
				if h.Pool != nil {
					pl = *h.Pool
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", h.ID, h.Name, pl, starts, ago(h.LastDelivery), h.URL)
			}
		})
		return nil
	case "create":
		if len(k.args) != 1 || *poolName == "" || (*taskName == "") == (*script == "") {
			return errors.New("usage: mcc webhook create NAME --pool POOL (--task TASK [--param KEY=VALUE]... | --script FILE)")
		}
		body := map[string]any{"name": k.args[0], "pool": *poolName}
		if *script != "" {
			b, err := os.ReadFile(*script)
			if err != nil {
				return err
			}
			body["script"] = string(b)
		} else {
			body["task"] = *taskName
			if len(p) > 0 {
				body["params"] = p
			}
		}
		var h hookView
		if err := k.send(http.MethodPost, "/api/v1/webhooks", body, &h); err != nil {
			return err
		}
		if !k.json {
			fmt.Printf("Created webhook %s.\n  URL     %s\n  Secret  %s\nThe secret is shown this once. A delivery is signed with it; see the docs for how.\n", h.Name, h.URL, h.Secret)
		}
		return nil
	case "rm":
		if err := k.need(1, "mcc webhook rm ID"); err != nil {
			return err
		}
		if err := k.send(http.MethodDelete, "/api/v1/webhooks/"+seg(k.args[0]), nil, nil); err != nil {
			return err
		}
		if !k.json {
			fmt.Println("Removed the webhook.")
		}
		return nil
	}
	return errors.New("usage: " + usage)
}

// ----- tokens -----

type scopeList []string

func (s *scopeList) String() string     { return strings.Join(*s, ",") }
func (s *scopeList) Set(v string) error { *s = append(*s, strings.Split(v, ",")...); return nil }

func token(args []string) error {
	const usage = "mcc token list | create NAME [--scope read|tasks:run|scripts:run|logs:read]... [--days N] | revoke ID"
	if len(args) == 0 {
		return errors.New("usage: " + usage)
	}
	fs := flag.NewFlagSet("token "+args[0], flag.ExitOnError)
	var scopes scopeList
	fs.Var(&scopes, "scope", "limit the token to this; may be repeated. Without any, it may do everything")
	days := fs.Int("days", 0, "days until it expires; 0 for never")
	k, cancel, err := start(fs, args[1:])
	if err != nil {
		return err
	}
	defer cancel()
	switch args[0] {
	case "list", "ls":
		var tokens []struct {
			ID        string     `json:"id"`
			Name      string     `json:"name"`
			Kind      string     `json:"kind"`
			Scopes    []string   `json:"scopes"`
			ExpiresAt *time.Time `json:"expires_at"`
			LastUsed  *time.Time `json:"last_used_at"`
		}
		if printed, err := k.get("/api/v1/api-tokens", &tokens); printed || err != nil {
			return err
		}
		table("ID\tNAME\tMADE BY\tMAY DO\tLAST USED\tEXPIRES", func(w io.Writer) {
			for _, t := range tokens {
				may, exp := "everything", "never"
				if t.Scopes != nil {
					may = strings.Join(t.Scopes, ", ")
				}
				if t.ExpiresAt != nil {
					exp = t.ExpiresAt.Local().Format("2 Jan 2006")
				}
				made := map[string]string{"cli": "mcc auth login", "mcp": "an MCP client", "api": "hand"}[t.Kind]
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, or(made, t.Kind), may, ago(t.LastUsed), exp)
			}
		})
		return nil
	case "create":
		if err := k.need(1, "mcc token create NAME [--scope SCOPE]... [--days N]"); err != nil {
			return err
		}
		body := map[string]any{"name": k.args[0]}
		if len(scopes) > 0 {
			body["scopes"] = []string(scopes)
		}
		if *days > 0 {
			body["expires_in_days"] = *days
		}
		var out struct {
			Token string `json:"api_token"`
		}
		if err := k.send(http.MethodPost, "/api/v1/api-tokens", body, &out); err != nil {
			return err
		}
		if !k.json {
			fmt.Fprintln(os.Stderr, "The token is shown this once:")
			fmt.Println(out.Token)
		}
		return nil
	case "revoke":
		if err := k.need(1, "mcc token revoke ID"); err != nil {
			return err
		}
		if err := k.send(http.MethodDelete, "/api/v1/api-tokens/"+seg(k.args[0]), nil, nil); err != nil {
			return err
		}
		if !k.json {
			fmt.Println("Revoked the token.")
		}
		return nil
	}
	return errors.New("usage: " + usage)
}

// ----- GitHub -----

const githubInstallURL = "https://github.com/apps/my-compute-cloud/installations/new"

func github(args []string) error {
	k, cancel, err := start(flag.NewFlagSet("github", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	defer cancel()
	var st statusView
	if k.json {
		return printRaw(k.ctx, k.c, "/api/v1/installations")
	}
	if _, err := k.c.Do(k.ctx, http.MethodGet, "/api/v1/status", nil, &st); err != nil {
		return err
	}
	if len(st.GitHub) == 0 {
		fmt.Println("No GitHub account is connected, so no GitHub Actions job will arrive.")
	} else {
		fmt.Println("Jobs arrive from:")
		for _, g := range st.GitHub {
			fmt.Printf("  %s%s\n", g.Account, yes(g.Suspended, " (the App is suspended there)", ""))
		}
	}
	fmt.Printf("\nTo connect an account or more repositories, install the GitHub App there:\n  %s\nThen sign in at the dashboard once, which connects it to this account.\n", githubInstallURL)
	return nil
}
