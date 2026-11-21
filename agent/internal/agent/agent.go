// Package agent is the MCC agent: it holds one outbound connection to the control plane of each
// cloud the machine is enrolled in, reports capacity, prepares images, and runs each assignment
// through an executor: in its own VM, or on the host itself where the owner allows that. Local
// policy is enforced here, and the control plane cannot override it.
package agent

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/identity"
	"github.com/my-compute-cloud/mcc/agent/internal/power"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
	"github.com/my-compute-cloud/mcc/agent/internal/tasklog"
)

// Version is set at build time with -ldflags: a release's number, or for a build from
// source the number and the commit, such as 0.1.0+e2c4478.
var Version = "dev"

// CacheSource hands out the host directory a job may keep downloads in. The directory depends
// on which repository or organization the job's runner registers with.
type CacheSource interface {
	Acquire(githubURL string) (dir string, release func(), err error)
	Prune()
}

// RunnerSource supplies the host directory holding a current Actions runner for a guest
// platform (ImageSpec.Platform), or "" if there is none.
type RunnerSource interface {
	Current(platform string) string
}

// Cloud is one enrollment of this machine: a host in one cloud, with its own key and the
// owner's say on what that cloud may do here.
type Cloud struct {
	// Name is what the owner calls it on this machine. The control plane never sees it.
	Name      string
	ServerURL string
	HostID    string
	Identity  *identity.Identity
	// Policy is the machine's policy with this cloud's own say in place of the machine's
	// (config.LoadCloudPolicy).
	Policy config.Policy
	// Cache is this cloud's alone: a writable share is the one thing a job can leave behind
	// for a later job, so no cloud's jobs find another's. Nil gives its jobs no cache.
	Cache CacheSource
}

type Options struct {
	// Clouds are the clouds this machine is enrolled in. With none, ServerURL, HostID,
	// Identity and Cache describe the one enrollment, which Policy governs.
	Clouds    []Cloud
	ServerURL string
	HostID    string
	Identity  *identity.Identity
	// Policy is the machine's policy: what is true of the machine whichever cloud asks.
	Policy   config.Policy
	Executor executor.Executor
	// HostExecutor runs work on the host itself. Nil unless the owner's policy allows that.
	HostExecutor executor.Executor
	Runner       RunnerSource
	// Cache is nil when the owner has not set a cache directory.
	Cache CacheSource
	// HostNix is the host's Nix store, shared read-only with jobs. Nil unless the owner
	// turned that on and the host has one.
	HostNix *HostNix
	// TaskOutput keeps what tasks print. Nil keeps nothing.
	TaskOutput *tasklog.Store
	// OutputAddress and OutputPin are where callers on other machines read a task's output
	// from this host, and the key they will find there. Empty unless the owner turned that on.
	OutputAddress, OutputPin string
	// OutputKey is the key of this host's output listener. With it, a caller can also read a
	// task's output through the control plane, sealed so that only the caller can read it.
	OutputKey *ecdsa.PrivateKey
	Log       *slog.Logger
	Resources executor.Resources
	// Now is the clock used for active hours (tests override it).
	Now func() time.Time
	// MinBackoff and MaxBackoff bound reconnect delays.
	MinBackoff, MaxBackoff time.Duration
}

type assignment struct {
	id string
	// platform is the guest platform of the assignment's image.
	platform string
	// host is true for work that runs on the host itself; it has no image and no platform.
	host bool
	// task is the name of the task it runs, if it runs one.
	task string
	// script is true for a caller's script, which runs in a VM.
	script bool
	state  protocol.AgentAssignmentState
	reason string
	// code and exitCode say how it ended, for the control plane: see protocol.Status.
	code     string
	exitCode int
	cancel   context.CancelFunc
	// stopReason is set when the control plane asked us to stop.
	stopReason string
	// reported is false while a terminal state has not reached the control plane yet.
	reported bool
}

// Agent is the one agent of a machine. What is true of the machine is here: how much runs at
// once, whether it is draining, which version it is. What belongs to one cloud is in a cloud.
type Agent struct {
	o      Options
	log    *slog.Logger
	awake  *power.Awake
	clouds []*cloud

	// admit is held while a start is checked against what is running and then counted, so that
	// two clouds asking for the last slot at once do not both get it.
	admit sync.Mutex

	// mu guards what follows, and every cloud's state.
	mu          sync.Mutex
	preparing   map[string]string // image source -> the name of an image being fetched from it
	refreshing  int               // images being refreshed now
	draining    map[string]bool   // the reasons no new work is taken; see drain.go
	runCtx      context.Context   // Run's context, for work started outside a message
	upgrade     *Upgrade          // how this agent switches versions, once WatchUpgrade runs
	switches    chan switchTo     // a build its owner asked it to become
	local       bool              // true for a build its owner put in the service's place
	wg          sync.WaitGroup
	isolationOK bool
}

// cloud is the agent's side of one enrollment: a connection to that cloud's control plane, the
// work that cloud gave this machine, and the images that cloud's pools name. A cloud's control
// plane is told about, and can ask for, nothing of another cloud's.
type cloud struct {
	a    *Agent
	name string
	// o is the agent's options as they apply to this cloud: its server, host id, key, policy
	// and cache, and the machine's everything else.
	o   Options
	log *slog.Logger
	// These are the agent's own.
	mu          *sync.Mutex
	wg          *sync.WaitGroup
	awake       *power.Awake
	isolationOK bool

	assignments map[string]*assignment
	images      map[string]protocol.ImageSpec // by name, as configured by the control plane
	pooled      map[string]bool               // image name -> a pool of this host is set to it
	ready       map[string]bool               // image name -> prepared
	conn        *websocket.Conn
	connCtx     context.Context
	streams     streams
}

func New(o Options) *Agent {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MinBackoff == 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = time.Minute
	}
	a := &Agent{
		o:           o,
		log:         o.Log,
		awake:       &power.Awake{Log: o.Log},
		preparing:   map[string]string{},
		draining:    map[string]bool{},
		switches:    make(chan switchTo, 1),
		local:       os.Getenv(localEnv) != "",
		isolationOK: o.Executor.NetworkIsolationAvailable(),
	}
	enrolled := o.Clouds
	if len(enrolled) == 0 {
		enrolled = []Cloud{{Name: config.DefaultCloud, ServerURL: o.ServerURL, HostID: o.HostID, Identity: o.Identity, Policy: o.Policy, Cache: o.Cache}}
	}
	for _, e := range enrolled {
		co := o
		co.Clouds = nil
		co.ServerURL, co.HostID, co.Identity, co.Policy, co.Cache = e.ServerURL, e.HostID, e.Identity, e.Policy, e.Cache
		// What the owner turned on for one cloud is not there for another.
		if len(o.Clouds) > 0 {
			if !e.Policy.HostTasks.Enabled {
				co.HostExecutor = nil
			}
			if !e.Policy.ShareNixStore {
				co.HostNix = nil
			}
		}
		log := o.Log
		if len(enrolled) > 1 {
			log = log.With("cloud", e.Name)
		}
		a.clouds = append(a.clouds, &cloud{
			a: a, name: e.Name, o: co, log: log,
			mu: &a.mu, wg: &a.wg, awake: a.awake, isolationOK: a.isolationOK,
			assignments: map[string]*assignment{},
			images:      map[string]protocol.ImageSpec{},
			ready:       map[string]bool{},
		})
	}
	return a
}

// Run cleans up leftovers from a previous run, then keeps a connection to each cloud's control
// plane until ctx ends. On return all VMs it started have been stopped.
func (a *Agent) Run(ctx context.Context) error {
	if a.o.Policy.NetworkIsolation() && !a.isolationOK {
		why := "this host cannot isolate VMs from its network"
		if e, ok := a.o.Executor.(interface{ NetworkIsolationMissing() string }); ok {
			why = e.NetworkIsolationMissing()
		}
		if a.o.Policy.RequireNetworkIsolation {
			a.log.Error("network isolation is required by policy but unavailable; VMs will be refused: " + why)
		} else {
			a.log.Warn("network isolation unavailable: " + why)
		}
	}
	a.mu.Lock()
	a.runCtx = ctx
	a.mu.Unlock()
	a.cleanupLeftovers(ctx)
	go a.refreshLoop(ctx)

	// One cloud that cannot be reached, or that has revoked this host, costs the others nothing.
	var connections sync.WaitGroup
	for _, c := range a.clouds {
		connections.Add(1)
		go func() {
			defer connections.Done()
			c.keepConnected(ctx)
		}()
	}
	connections.Wait()
	a.shutdown()
	return nil
}

// keepConnected holds this cloud's connection, dialling again whenever it is lost.
func (c *cloud) keepConnected(ctx context.Context) {
	backoff := c.o.MinBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			break
		}
		var rej *rejectedError
		if errors.As(err, &rej) && rej.upgrade {
			c.log.Error("control plane requires a newer agent", "reason", rej.reason)
			backoff = c.o.MaxBackoff
		} else if time.Since(started) > 30*time.Second {
			backoff = c.o.MinBackoff // the connection was healthy for a while
		}
		delay := backoff/2 + rand.N(backoff/2+1)
		c.log.Warn("disconnected from control plane", "err", err, "retry_in", delay.Round(time.Millisecond))
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
		backoff = min(backoff*2, c.o.MaxBackoff)
	}
}

func (a *Agent) cleanupLeftovers(ctx context.Context) {
	for _, ex := range []executor.Executor{a.o.Executor, a.o.HostExecutor} {
		if ex == nil {
			continue
		}
		left, err := ex.List(ctx)
		if err != nil {
			a.log.Warn("could not list work left from a previous run", "err", err)
			continue
		}
		for _, h := range left {
			a.log.Info("cleaning up work left from a previous run", "name", h.Name)
			if err := ex.Stop(ctx, h); err != nil {
				a.log.Warn("cleaning up leftover work failed", "name", h.Name, "err", err)
			}
		}
	}
}

func (a *Agent) shutdown() {
	a.mu.Lock()
	for _, c := range a.clouds {
		for _, as := range c.assignments {
			if as.cancel != nil {
				as.stopReason = "agent shutting down"
				as.cancel()
			}
		}
	}
	a.mu.Unlock()
	a.wg.Wait()
}

// Draining is the agent's: a machine drains for every cloud at once.
func (c *cloud) Draining() string { return c.a.Draining() }

type rejectedError struct {
	reason  string
	upgrade bool
}

func (e *rejectedError) Error() string { return "rejected: " + e.reason }

// wsURL names the host in the query string. The control plane uses it only to route the
// connection to this host's tenant; the signed hello that follows is what authenticates it.
func wsURL(server, hostID string) string {
	u := strings.TrimSuffix(server, "/") + "/agent/connect?host_id=" + url.QueryEscape(hostID)
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	if strings.HasPrefix(u, "http://") {
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// session runs one connection from dial to close.
func (c *cloud) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(dialCtx, wsURL(c.o.ServerURL, c.o.HostID), &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": []string{"mcc-agent/" + Version}},
	})
	cancel()
	if err != nil {
		return err
	}
	conn.SetReadLimit(1 << 20)
	defer conn.CloseNow()

	sctx, scancel := context.WithCancel(ctx)
	defer scancel()

	// challenge -> hello -> welcome
	var ch protocol.Challenge
	if err := readTyped(sctx, conn, protocol.TypeChallenge, &ch); err != nil {
		return err
	}
	hello := protocol.Hello{
		Type:         protocol.TypeHello,
		HostID:       c.o.HostID,
		AgentVersion: Version,
		Versions:     protocol.Versions,
		Platform:     protocol.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Signature:    c.o.Identity.SignHello(ch.Nonce, c.o.HostID),
		Assignments:  c.reports(),
		// The policy is read when the agent starts and a change to it restarts the agent, so
		// what each connection opens with is the policy in force. Each cloud is told the policy
		// that governs its own work here, and nothing of what another cloud may do.
		Policy: policyReport(c.o.Policy, c.isolationOK),
	}
	if err := write(sctx, conn, &hello); err != nil {
		return err
	}
	raw, typ, err := read(sctx, conn)
	if err != nil {
		return err
	}
	switch typ {
	case protocol.TypeReject:
		var r protocol.Reject
		_ = json.Unmarshal(raw, &r)
		return &rejectedError{reason: r.Reason, upgrade: r.UpgradeRequired}
	case protocol.TypeWelcome:
	default:
		return fmt.Errorf("expected welcome, got %s", typ)
	}
	var w protocol.Welcome
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	c.log.Info("connected to control plane", "server", c.o.ServerURL, "tenant", w.TenantID, "protocol", w.Version)

	c.mu.Lock()
	c.conn, c.connCtx = conn, sctx
	// The terminal states we just sent in hello have been delivered.
	for id, as := range c.assignments {
		if isTerminal(as.state) {
			delete(c.assignments, id)
		}
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()

	c.applyConfig(ctx, w.Pools, w.Images)
	c.sendCapacity()

	interval := time.Duration(w.HeartbeatIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 20 * time.Second
	}
	go c.heartbeat(sctx, interval)

	for {
		raw, typ, err := read(sctx, conn)
		if err != nil {
			return err
		}
		switch typ {
		case protocol.TypeStart:
			var m protocol.Start
			if err := json.Unmarshal(raw, &m); err == nil {
				c.handleStart(ctx, m)
			}
		case protocol.TypeStop:
			var m protocol.Stop
			if err := json.Unmarshal(raw, &m); err == nil {
				c.handleStop(m)
			}
		case protocol.TypeAttach:
			var m protocol.Attach
			if err := json.Unmarshal(raw, &m); err == nil {
				// Tied to this connection: a stream ends when the connection that carries it does.
				c.handleAttach(sctx, m)
			}
		case protocol.TypeDetach:
			var m protocol.Detach
			if err := json.Unmarshal(raw, &m); err == nil {
				c.handleDetach(m)
			}
		case protocol.TypeConfig:
			var m protocol.Config
			if err := json.Unmarshal(raw, &m); err == nil {
				c.applyConfig(ctx, m.Pools, m.Images)
				c.sendCapacity()
			}
		default:
			c.log.Warn("ignoring unexpected message", "type", typ)
		}
	}
}

func (c *cloud) heartbeat(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	lastActive := c.o.Policy.Active(c.o.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.send(&protocol.Heartbeat{Type: protocol.TypeHeartbeat})
			// Re-announce capacity when active hours start or end.
			if active := c.o.Policy.Active(c.o.Now()); active != lastActive {
				lastActive = active
				c.sendCapacity()
			}
		}
	}
}

// ----- messages -----

func envelope(m any) {
	switch v := m.(type) {
	case *protocol.Hello:
		v.V, v.ID = 1, newID()
	case *protocol.Capacity:
		v.V, v.ID = 1, newID()
	case *protocol.Status:
		v.V, v.ID = 1, newID()
	case *protocol.Heartbeat:
		v.V, v.ID = 1, newID()
	case *protocol.Output:
		v.V, v.ID = 1, newID()
	}
}

func write(ctx context.Context, conn *websocket.Conn, m any) error {
	envelope(m)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func read(ctx context.Context, conn *websocket.Conn) ([]byte, string, error) {
	_, b, err := conn.Read(ctx)
	if err != nil {
		return nil, "", err
	}
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, "", fmt.Errorf("invalid message: %w", err)
	}
	return b, env.Type, nil
}

func readTyped(ctx context.Context, conn *websocket.Conn, want string, into any) error {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, typ, err := read(rctx, conn)
	if err != nil {
		return err
	}
	if typ != want {
		return fmt.Errorf("expected %s, got %s", want, typ)
	}
	return json.Unmarshal(b, into)
}

// send writes to the current connection, if any. Messages while disconnected are dropped; the
// next hello carries the state of every assignment instead.
func (c *cloud) send(m any) bool {
	c.mu.Lock()
	conn, ctx := c.conn, c.connCtx
	c.mu.Unlock()
	if conn == nil {
		return false
	}
	return write(ctx, conn, m) == nil
}

func isTerminal(s protocol.AgentAssignmentState) bool {
	return s == protocol.AgentAssignmentStateFinished || s == protocol.AgentAssignmentStateFailed
}

func (c *cloud) reports() []protocol.AssignmentReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]protocol.AssignmentReport, 0, len(c.assignments))
	for _, as := range c.assignments {
		out = append(out, protocol.AssignmentReport{AssignmentID: as.id, State: as.state, Reason: as.reason, Code: as.code, ExitCode: as.exitCode})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssignmentID < out[j].AssignmentID })
	return out
}

func (c *cloud) setState(id string, s protocol.AgentAssignmentState, reason string) {
	c.mu.Lock()
	as := c.assignments[id]
	if as == nil {
		c.mu.Unlock()
		return
	}
	as.state, as.reason = s, reason
	st := &protocol.Status{Type: protocol.TypeStatus, AssignmentID: id, State: s, Reason: reason}
	if isTerminal(s) {
		st.Code, st.ExitCode = as.code, as.exitCode
	}
	c.mu.Unlock()
	delivered := c.send(st)
	if isTerminal(s) {
		c.mu.Lock()
		if delivered {
			delete(c.assignments, id)
		} else {
			as.cancel = nil // keep it until a hello reports it
		}
		c.mu.Unlock()
		// What this cloud gave back, another may now have.
		c.a.announce()
	}
}

// ----- capacity and images -----

// What runs at once has two limits: the machine's, counted across every cloud, and this
// cloud's own share of it. A cloud is told the smaller of what each leaves.

func isVM(as *assignment) bool   { return !as.host }
func onHost(as *assignment) bool { return as.host }

// countLocked counts this cloud's work that has not ended and that pick chooses.
func (c *cloud) countLocked(pick func(*assignment) bool) int {
	n := 0
	for _, as := range c.assignments {
		if !isTerminal(as.state) && pick(as) {
			n++
		}
	}
	return n
}

func (c *cloud) count(pick func(*assignment) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.countLocked(pick)
}

// count is the same across every cloud.
func (a *Agent) count(pick func(*assignment) bool) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.clouds {
		n += c.countLocked(pick)
	}
	return n
}

// slotsTotal is the most VMs of any kind this cloud may have here at once: its own cap, and
// never more than the machine's. A guest platform may have a lower limit of its own
// (guestLimit).
func (c *cloud) slotsTotal() int {
	return min(c.o.Policy.MaxVMs, c.a.o.Policy.MaxVMs)
}

// freeSlots is how many more VMs this cloud may start now.
func (c *cloud) freeSlots() int {
	return max(0, min(c.slotsTotal()-c.count(isVM), c.a.o.Policy.MaxVMs-c.a.count(isVM)))
}

// guestPlatform is an image's guest platform; images from before the field are macOS.
func guestPlatform(image protocol.ImageSpec) string {
	if image.Platform == "" {
		return "darwin"
	}
	return image.Platform
}

// guestLimit is the most VMs of a guest platform this host may run at once, or 0 if only
// slotsTotal limits them. Apple allows two macOS guests per host.
func (c *cloud) guestLimit(platform string) int {
	return c.o.Executor.MaxVMs(protocol.ImageSpec{Platform: platform})
}

// activeOn counts running VMs of one guest platform, whichever cloud they are for: the limit
// is the machine's.
func (c *cloud) activeOn(platform string) int {
	return c.a.count(func(as *assignment) bool { return as.platform == platform })
}

// hostSlots is how much work this host runs directly at once: none unless the owner allows it,
// and none where policy demands network isolation, which such work cannot have.
func (c *cloud) hostSlots() int {
	if c.o.HostExecutor == nil || c.o.Policy.RequireNetworkIsolation {
		return 0
	}
	return c.o.Policy.HostTasks.Slots()
}

func (c *cloud) accepting() bool {
	if c.Draining() != "" {
		return false
	}
	if !c.o.Policy.Active(c.o.Now()) {
		return false
	}
	return !(c.o.Policy.RequireNetworkIsolation && !c.isolationOK)
}

// hostRoom is how much more work this cloud may run on the host itself now: what its own share
// leaves, and never more than the machine has left.
func (c *cloud) hostRoom() int {
	return max(0, min(c.hostSlots()-c.count(onHost), c.a.o.Policy.HostTasks.Slots()-c.a.count(onHost)))
}

// freeHostSlots is zero until there is something the host can run: a runner to give a GitHub
// job (unlike a VM, the host has no image with one in it to fall back on), or a task its owner
// has declared.
func (c *cloud) freeHostSlots() int {
	if c.hostRunner() == "" && len(c.o.Policy.Tasks) == 0 {
		return 0
	}
	return c.hostRoom()
}

// activeTask counts this cloud's runs of one task.
func (c *cloud) activeTask(name string) int {
	return c.count(func(as *assignment) bool { return as.task == name })
}

// declaredTasks are the tasks the owner's policy declares, for the control plane to choose
// hosts by. None on a host that runs no work itself.
func (c *cloud) declaredTasks() []protocol.TaskCapacity {
	if c.hostSlots() == 0 {
		return nil
	}
	var out []protocol.TaskCapacity
	for name, t := range c.o.Policy.Tasks {
		out = append(out, protocol.TaskCapacity{Name: name, TimeoutSeconds: int(t.Timeout() / time.Second), MaxConcurrent: t.MaxConcurrent})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// outputKey is the public half of the host's output key, as callers are given it.
func (c *cloud) outputKey() string {
	if c.o.OutputKey == nil || c.o.TaskOutput == nil {
		return ""
	}
	k, err := tasklog.PublicKey(c.o.OutputKey)
	if err != nil {
		return ""
	}
	return k
}

// hostRunner is the directory of the Actions runner built for this host's own system.
func (c *cloud) hostRunner() string {
	if c.o.Runner == nil {
		return ""
	}
	return c.o.Runner.Current(runtime.GOOS)
}

func (c *cloud) sendCapacity() {
	total, free := c.slotsTotal(), c.freeSlots()
	c.mu.Lock()
	ready := make([]string, 0, len(c.ready))
	for name, ok := range c.ready {
		if ok {
			ready = append(ready, name)
		}
	}
	c.mu.Unlock()
	sort.Strings(ready)
	var guests []protocol.GuestCapacity
	for _, p := range c.o.Executor.GuestPlatforms() {
		guests = append(guests, protocol.GuestCapacity{Platform: p, Limit: c.guestLimit(p)})
	}
	c.send(&protocol.Capacity{
		Type: protocol.TypeCapacity, SlotsTotal: total, SlotsFree: free, ImagesReady: ready, Accepting: c.accepting(), Guests: guests,
		HostSlotsTotal: c.hostSlots(), HostSlotsFree: c.freeHostSlots(), Tasks: c.declaredTasks(),
		OutputAddress: c.o.OutputAddress, OutputPin: c.o.OutputPin, OutputKey: c.outputKey(),
		Scripts:  c.o.Policy.WorkloadAllowed("script"),
		Draining: c.Draining(),
	})
}

// applyConfig records the images the control plane may ask for, and prepares those a pool of
// this host is set to and policy allows. The others are the account's other images, which a
// job may name in place of its pool's; one of those is fetched when a job first asks for it.
func (c *cloud) applyConfig(ctx context.Context, pools []protocol.PoolConfig, images []protocol.ImageSpec) {
	c.mu.Lock()
	c.images = map[string]protocol.ImageSpec{}
	for _, img := range images {
		c.images[img.Name] = img
	}
	c.pooled = map[string]bool{}
	for _, p := range pools {
		if p.Image != "" {
			c.pooled[p.Image] = true
		}
	}
	for name := range c.ready {
		if _, ok := c.images[name]; !ok {
			delete(c.ready, name)
		}
	}
	pooled := c.pooled
	c.mu.Unlock()
	for _, img := range images {
		if pooled[img.Name] && !c.o.Policy.ImageAllowed(img.Source) {
			c.log.Warn("image not allowed by local policy; not preparing it", "image", img.Name, "source", img.Source)
		}
	}
	c.prepareMissing(ctx)
}

// prepareMissing starts fetching each image the control plane wants, the owner allows and the
// host does not have. While draining it starts nothing: a download begun now would only hold
// up what the agent is draining for.
func (c *cloud) prepareMissing(ctx context.Context) {
	c.mu.Lock()
	images := make([]protocol.ImageSpec, 0, len(c.images))
	for _, img := range c.images {
		if c.pooled[img.Name] {
			images = append(images, img)
		}
	}
	c.mu.Unlock()
	for _, img := range images {
		if !c.o.Policy.ImageAllowed(img.Source) {
			continue
		}
		if c.o.Executor.Ready(ctx, img) {
			c.mu.Lock()
			c.ready[img.Name] = true
			c.mu.Unlock()
			continue
		}
		if c.Draining() != "" {
			continue
		}
		c.prepareAsync(ctx, img)
	}
}

// prepareAsync fetches an image unless it is already being fetched. Images are kept by their
// source, so one that two clouds name is fetched once and is there for both.
func (c *cloud) prepareAsync(ctx context.Context, img protocol.ImageSpec) {
	c.mu.Lock()
	if _, busy := c.a.preparing[img.Source]; busy {
		c.mu.Unlock()
		return
	}
	c.a.preparing[img.Source] = img.Name
	c.mu.Unlock()
	go func() {
		err := c.o.Executor.Prepare(ctx, img)
		c.mu.Lock()
		delete(c.a.preparing, img.Source)
		if err == nil {
			c.ready[img.Name] = true
		}
		c.mu.Unlock()
		if err != nil {
			c.log.Error("preparing image failed", "image", img.Name, "err", err)
			return
		}
		c.log.Info("image ready", "image", img.Name)
		c.sendCapacity()
		for _, other := range c.a.clouds {
			if other != c {
				other.prepareMissing(ctx)
				other.sendCapacity()
			}
		}
	}()
}

// announce tells every cloud what it may have now.
func (a *Agent) announce() {
	for _, c := range a.clouds {
		c.sendCapacity()
	}
}

func (a *Agent) refreshLoop(ctx context.Context) {
	every := a.o.Policy.ImageRefresh()
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.mu.Lock()
		var imgs []protocol.ImageSpec
		seen := map[string]bool{}
		for _, c := range a.clouds {
			for name, img := range c.images {
				if c.ready[name] && !seen[img.Source] {
					seen[img.Source] = true
					imgs = append(imgs, img)
				}
			}
		}
		a.mu.Unlock()
		for _, img := range imgs {
			// A refresh can wait; what the agent is draining for should not wait for it.
			a.mu.Lock()
			if a.drainingLocked() != "" {
				a.mu.Unlock()
				continue
			}
			a.refreshing++
			a.mu.Unlock()
			err := a.o.Executor.Refresh(ctx, img)
			a.mu.Lock()
			a.refreshing--
			a.mu.Unlock()
			if err != nil {
				a.log.Warn("refreshing image failed", "image", img.Name, "err", err)
			}
		}
	}
}
