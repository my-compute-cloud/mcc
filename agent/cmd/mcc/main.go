// Command mcc is the My Compute Cloud agent.
//
//	mcc enroll --server URL [--name NAME] TOKEN   register this host with the control plane
//	mcc run                                       connect and run work (normally as a launchd agent)
//	mcc policy                                    print the effective local policy
//	mcc version
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/agent"
	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
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
	fmt.Printf("Enrolled as host %s. Start the agent with `mcc run` (or enable the launchd service).\n", out.HostID)
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

	var ex executor.Executor
	switch os.Getenv("MCC_EXECUTOR") {
	case "fake":
		f := fake.New()
		f.JobDuration = 200 * time.Millisecond
		ex = f
	case "", "tart":
		ex = tart.New(log)
	default:
		return fmt.Errorf("unknown MCC_EXECUTOR %q", os.Getenv("MCC_EXECUTOR"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var runner *runnerbin.Manager
	if policy.SharedRunnerEnabled() && os.Getenv("MCC_EXECUTOR") != "fake" {
		runner = runnerbin.New(filepath.Join(dir, "runner"), runnerPlatform())
		go keepRunnerCurrent(ctx, runner, log)
	}

	a := agent.New(agent.Options{
		ServerURL: state.ServerURL,
		HostID:    state.HostID,
		Identity:  id,
		Policy:    policy,
		Executor:  ex,
		Runner:    runnerSource(runner),
		Log:       log,
		Resources: resources(policy),
	})
	log.Info("starting agent", "version", agent.Version, "host", state.HostID, "server", state.ServerURL)
	return a.Run(ctx)
}

func runnerSource(m *runnerbin.Manager) agent.RunnerSource {
	if m == nil {
		return nil
	}
	return m
}

func keepRunnerCurrent(ctx context.Context, m *runnerbin.Manager, log *slog.Logger) {
	for {
		dir, err := m.Update(ctx)
		if err != nil {
			log.Warn("updating Actions runner failed; VMs will use the image's runner if none is cached", "err", err)
		} else {
			log.Info("Actions runner ready", "dir", dir)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(12 * time.Hour):
		}
	}
}

// runnerPlatform is the Actions runner build for macOS guests on this host.
func runnerPlatform() string {
	if runtime.GOARCH == "amd64" {
		return "osx-x64"
	}
	return "osx-arm64"
}

// resources applies the policy, defaulting to half the host per VM.
func resources(p config.Policy) executor.Resources {
	r := executor.Resources{CPUs: p.CPUs, MemoryMB: p.MemoryMB}
	if r.CPUs == 0 {
		r.CPUs = max(1, runtime.NumCPU()/2)
	}
	if r.MemoryMB == 0 {
		if total := hostMemoryMB(); total > 0 {
			r.MemoryMB = total / 2
		}
	}
	return r
}

func hostMemoryMB() int {
	if runtime.GOOS != "darwin" {
		return 0
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
	b, _ := json.MarshalIndent(struct {
		config.Policy
		File                  string             `json:"file"`
		EffectiveResources    executor.Resources `json:"effective_resources"`
		NetworkIsolationOn    bool               `json:"network_isolation_effective"`
		StayAwakeEffective    bool               `json:"stay_awake_effective"`
		SharedRunnerEffective bool               `json:"shared_runner_effective"`
	}{p, config.PolicyPath(dir), resources(p), p.NetworkIsolation() && tart.New(slog.Default()).NetworkIsolationAvailable(), p.StayAwakeEnabled(), p.SharedRunnerEnabled()}, "", "  ")
	fmt.Println(string(b))
	return nil
}
