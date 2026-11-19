// Command mcc is the My Compute Cloud agent.
//
//	mcc enroll --server URL [--name NAME] TOKEN   register this host with the control plane
//	mcc run                                       connect and run work (normally as a service)
//	mcc policy                                    print the effective local policy
//	mcc logs [-f] [--offset N] TASK               print what a task printed on this host
//	mcc logs --host ADDR --pin PIN [-f] TASK      the same from another host, with MCC_TASK_SECRET
//	mcc logs --server URL [-f] TASK               the same through the control plane, sealed end to end
//	mcc pin                                       print where this host serves output, and its key's pin
//	mcc version
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"github.com/my-compute-cloud/mcc/agent/internal/api"
	"github.com/my-compute-cloud/mcc/agent/internal/cache"
	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/kvm"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/process"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/tart"
	"github.com/my-compute-cloud/mcc/agent/internal/identity"
	"github.com/my-compute-cloud/mcc/agent/internal/runnerbin"
	"github.com/my-compute-cloud/mcc/agent/internal/tasklog"
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
		if len(os.Args) > 2 && os.Args[2] == "check" {
			err = policyCheck(os.Args[3:])
		} else {
			err = showPolicy()
		}
	case "check":
		err = check(os.Args[2:])
	case "logs":
		err = logs(os.Args[2:])
	case "pin":
		err = showPin()
	case "auth":
		err = auth(os.Args[2:])
	case "status":
		err = status(os.Args[2:])
	case "image":
		err = image(os.Args[2:])
	case "pool":
		err = pool(os.Args[2:])
	case "host":
		err = host(os.Args[2:])
	case "task":
		err = task(os.Args[2:])
	case "webhook":
		err = webhook(os.Args[2:])
	case "token":
		err = token(os.Args[2:])
	case "github":
		err = github(os.Args[2:])
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
	fmt.Fprint(os.Stderr, `usage: mcc COMMAND

You, signed in:
  auth login | logout | status | token   sign in through the browser
  status                                 pools, hosts and what is wrong
  image | pool | host                    manage them
  task | webhook                         run work and see how it went
  token                                  API tokens
  github                                 which GitHub accounts send jobs here

This machine, as a host:
  enroll [--pool POOL]                   register it (signed in), or: enroll --server URL TOKEN
  check                                  what is wrong with this host
  policy [check [FILE]]                  what this host allows
  run                                    connect and run work (normally as a service)
  logs [-f] TASK                         what a task printed
  pin                                    where this host serves output, and its key's pin
  version

Most commands take --json.
`)
	os.Exit(2)
}

func enroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "control plane URL")
	poolName := fs.String("pool", "", "the pool to put this host in, when enrolling signed in")
	host, _ := os.Hostname()
	name := fs.String("name", strings.TrimSuffix(host, ".local"), "host name shown in the control plane")
	rest := parse(fs, args)
	if len(rest) > 1 || (len(rest) == 1 && *server == "") {
		return fmt.Errorf("usage: mcc enroll [--pool POOL] [--name NAME] (signed in), or mcc enroll --server URL [--name NAME] TOKEN")
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	if _, err := config.LoadState(dir); err == nil {
		return fmt.Errorf("this host is already enrolled (state in %s)", dir)
	}
	// With no token in hand, whoever is signed in asks for one: it is used at once, here.
	var token string
	if len(rest) == 1 {
		token = rest[0]
	} else {
		c, err := api.New()
		if err != nil {
			return fmt.Errorf("%w, or give a token: mcc enroll --server URL TOKEN", err)
		}
		if *server != "" && strings.TrimSuffix(*server, "/") != strings.TrimSuffix(c.Server, "/") {
			return fmt.Errorf("signed in to %s, not %s", c.Server, *server)
		}
		*server = c.Server
		req := map[string]string{"host_name": *name}
		if *poolName != "" {
			req["pool_id"] = *poolName
		}
		var out struct {
			Token string `json:"token"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.Do(ctx, http.MethodPost, "/api/v1/enrollment-tokens", req, &out); err != nil {
			return fmt.Errorf("asking for an enrollment token: %w", err)
		}
		token = out.Token
	}
	id, err := identity.Generate(dir)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{
		"token":      token,
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
	fmt.Printf("Enrolled as host %s. Start the agent with `mcc run` (or start its service), then `mcc check`.\n", out.HostID)
	if *poolName == "" && len(rest) == 0 {
		fmt.Printf("It is in no pool yet: mcc pool add-host POOL %s\n", *name)
	}
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
	// One agent per state directory. `mcc check` reads the same lock to say whether one runs.
	lock, ok, err := lockAgent(dir)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("an agent is already running with the state in %s", dir)
	}
	defer lock.Close()
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

	// What tasks print is kept here, on the host, and read from here: `mcc logs`, or anything
	// that speaks HTTP to the socket. The socket is the agent's own account's alone.
	var taskOutput *tasklog.Store
	if len(policy.Tasks) > 0 || policy.WorkloadAllowed("script") {
		o := policy.TaskOutput
		taskOutput = tasklog.New(filepath.Join(dir, "task-output"), tasklog.Limits{
			PerTask: int64(o.MaxMB) << 20,
			Total:   int64(o.TotalMB) << 20,
			KeepFor: time.Duration(o.KeepDays) * 24 * time.Hour,
		})
		taskOutput.Recover()
		go taskOutput.Prune()
		go func() {
			if err := taskOutput.ServeUnix(ctx, logSocket(dir)); err != nil {
				log.Warn("task output cannot be read with `mcc logs`", "err", err)
			}
		}()
	}
	// Only if the owner asked for it does this host accept a connection: one listener, which
	// gives a task's output to whoever shows that task's secret, and nothing else to anyone.
	var outputAddress, outputPin string
	var outputKey *ecdsa.PrivateKey
	if taskOutput != nil {
		// The key is made whether or not the host listens: a caller who cannot reach the host
		// reads through the control plane, sealed to this key.
		key, err := tasklog.LoadOrCreateKey(outputKeyPath(dir))
		if err != nil {
			log.Error("task output can only be read on this host", "err", err)
		} else if pin, err := tasklog.Pin(&key.PublicKey); err == nil {
			outputKey, outputPin = key, pin
		}
	}
	if outputKey != nil && policy.TaskOutput.Listen != "" {
		address, err := serveOutput(ctx, outputKey, policy.TaskOutput, taskOutput, log)
		if err != nil {
			log.Error("task output will not be served to other machines", "listen", policy.TaskOutput.Listen, "err", err)
		} else {
			outputAddress = address
			log.Info("serving task output to callers that show a task's secret", "listen", policy.TaskOutput.Listen, "address", address, "pin", outputPin)
		}
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
		ServerURL:     state.ServerURL,
		HostID:        state.HostID,
		Identity:      id,
		Policy:        policy,
		Executor:      ex,
		HostExecutor:  hostEx,
		Runner:        runnerSource(runners),
		Cache:         jobCache,
		TaskOutput:    taskOutput,
		OutputAddress: outputAddress,
		OutputPin:     outputPin,
		OutputKey:     outputKey,
		Log:           log,
		Resources:     resources(policy),
	})
	log.Info("starting agent", "version", agent.Version, "host", state.HostID, "server", state.ServerURL)
	return a.Run(ctx)
}

// outputKeyPath is where the host's output key is kept: the key callers pin, for the listener
// and for reads through the control plane.
func outputKeyPath(dir string) string { return filepath.Join(dir, "output.key") }

// serveOutput starts the listener for callers on other machines, and returns the address they
// are told.
func serveOutput(ctx context.Context, key *ecdsa.PrivateKey, o config.TaskOutput, store *tasklog.Store, log *slog.Logger) (address string, err error) {
	address = o.Reachable()
	cert, err := tasklog.Certificate(key, address)
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return "", err
	}
	go func() {
		if err := store.ServeTLS(ctx, ln, cert); err != nil {
			log.Error("the task output listener stopped", "err", err)
		}
	}()
	return address, nil
}

// showPin prints where this host serves task output and the pin of its key, for a caller who
// would sooner have the pin from the host's owner than from the control plane.
func showPin() error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	policy, err := config.LoadPolicy(dir)
	if err != nil {
		return err
	}
	key, err := tasklog.LoadOrCreateKey(outputKeyPath(dir))
	if err != nil {
		return err
	}
	pin, err := tasklog.Pin(&key.PublicKey)
	if err != nil {
		return err
	}
	address := policy.TaskOutput.Reachable()
	if address == "" {
		address = "(this host does not listen; output is read here, or through the control plane)"
	}
	fmt.Printf("address  %s\npin      sha256//%s\n", address, pin)
	return nil
}

// logSocket is where a running agent serves what tasks printed.
func logSocket(dir string) string { return filepath.Join(dir, "logs.sock") }

// logs prints a task's output from the agent running on this host. Its exit status is the
// task's, when the task has ended and the output was read to its end.
func logs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "keep printing until the task ends")
	offset := fs.Int64("offset", 0, "start this many bytes into the output")
	host := fs.String("host", "", "read from another host: the address the task's output is at")
	pin := fs.String("pin", "", "the pin of that host's key: needed with --host, and with --server it is checked against the control plane's word")
	server := fs.String("server", "", "read through the control plane at this URL, for a host that cannot be reached")
	_ = fs.Parse(args)
	if fs.NArg() != 1 || (*host != "" && *pin == "") || (*host != "" && *server != "") || (*pin != "" && *host == "" && *server == "") {
		return fmt.Errorf("usage: mcc logs [-f] [--offset N] [--host ADDR --pin PIN | --server URL [--pin PIN]] TASK")
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The secret, for a task that has one, comes from the environment: an argument could be
	// read by every account on the host.
	secret := os.Getenv("MCC_TASK_SECRET")
	var end tasklog.End
	if *server != "" {
		// Through the control plane, sealed between this program and the host. The API token
		// says whose task it is; like the secret, it comes from the environment.
		end, err = tasklog.ReadRelay(ctx, nil, *server, os.Getenv("MCC_API_TOKEN"), fs.Arg(0), secret, *pin, *offset, *follow, os.Stdout)
	} else if *host != "" {
		end, err = tasklog.ReadTLS(ctx, *host, *pin, fs.Arg(0), secret, *offset, *follow, os.Stdout)
	} else {
		end, err = tasklog.ReadUnix(ctx, logSocket(dir), fs.Arg(0), secret, *offset, *follow, os.Stdout)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if *server != "" {
			return fmt.Errorf("reading the task's output through %s: %w", *server, err)
		}
		if *host != "" {
			return fmt.Errorf("reading the task's output from %s: %w", *host, err)
		}
		return fmt.Errorf("reading the task's output (is the agent running on this host?): %w", err)
	}
	if end.Truncated {
		fmt.Fprintln(os.Stderr, "mcc: the task printed more than this host keeps; the rest was dropped")
	}
	switch {
	case end.Running:
		fmt.Fprintln(os.Stderr, "mcc: the task is still running; -f follows it")
	case end.ExitCode != nil && *end.ExitCode != 0:
		fmt.Fprintf(os.Stderr, "mcc: the task exited with code %d\n", *end.ExitCode)
		os.Exit(*end.ExitCode)
	case end.Code != "":
		return fmt.Errorf("the task did not run to an exit: %s", end.Code)
	}
	return nil
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
