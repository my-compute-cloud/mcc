// Package agent is the MCC agent: it holds one outbound connection to the control plane,
// reports capacity, prepares images, and runs each assignment through an executor: in its own
// VM, or on the host itself where the owner allows that. Local policy is enforced here, and
// the control plane cannot override it.
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

// Version is set at build time with -ldflags.
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

type Options struct {
	ServerURL string
	HostID    string
	Identity  *identity.Identity
	Policy    config.Policy
	Executor  executor.Executor
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

type Agent struct {
	o     Options
	log   *slog.Logger
	awake *power.Awake

	mu          sync.Mutex
	assignments map[string]*assignment
	images      map[string]protocol.ImageSpec // by name, as configured by the control plane
	pooled      map[string]bool               // image name -> a pool of this host is set to it
	ready       map[string]bool               // image name -> prepared
	preparing   map[string]bool
	refreshing  int             // images being refreshed now
	draining    map[string]bool // the reasons no new work is taken; see drain.go
	runCtx      context.Context // Run's context, for work started outside a message
	upgrade     *Upgrade        // how this agent switches versions, once WatchUpgrade runs
	switches    chan switchTo   // a build its owner asked it to become
	local       bool            // true for a build its owner put in the service's place
	conn        *websocket.Conn
	connCtx     context.Context
	wg          sync.WaitGroup
	isolationOK bool
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
	return &Agent{
		o:           o,
		log:         o.Log,
		awake:       &power.Awake{Log: o.Log},
		assignments: map[string]*assignment{},
		images:      map[string]protocol.ImageSpec{},
		ready:       map[string]bool{},
		preparing:   map[string]bool{},
		draining:    map[string]bool{},
		switches:    make(chan switchTo, 1),
		local:       os.Getenv(localEnv) != "",
		isolationOK: o.Executor.NetworkIsolationAvailable(),
	}
}

// Run cleans up leftovers from a previous run, then keeps a connection to the control plane
// until ctx ends. On return all VMs it started have been stopped.
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

	backoff := a.o.MinBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			break
		}
		var rej *rejectedError
		if errors.As(err, &rej) && rej.upgrade {
			a.log.Error("control plane requires a newer agent", "reason", rej.reason)
			backoff = a.o.MaxBackoff
		} else if time.Since(started) > 30*time.Second {
			backoff = a.o.MinBackoff // the connection was healthy for a while
		}
		delay := backoff/2 + rand.N(backoff/2+1)
		a.log.Warn("disconnected from control plane", "err", err, "retry_in", delay.Round(time.Millisecond))
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
		backoff = min(backoff*2, a.o.MaxBackoff)
	}
	a.shutdown()
	return nil
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
	for _, as := range a.assignments {
		if as.cancel != nil {
			as.stopReason = "agent shutting down"
			as.cancel()
		}
	}
	a.mu.Unlock()
	a.wg.Wait()
}

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
func (a *Agent) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(dialCtx, wsURL(a.o.ServerURL, a.o.HostID), &websocket.DialOptions{
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
		HostID:       a.o.HostID,
		AgentVersion: Version,
		Versions:     protocol.Versions,
		Platform:     protocol.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Signature:    a.o.Identity.SignHello(ch.Nonce, a.o.HostID),
		Assignments:  a.reports(),
		// The policy is read when the agent starts and a change to it restarts the agent, so
		// what each connection opens with is the policy in force.
		Policy: policyReport(a.o.Policy),
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
	a.log.Info("connected to control plane", "server", a.o.ServerURL, "tenant", w.TenantID, "protocol", w.Version)

	a.mu.Lock()
	a.conn, a.connCtx = conn, sctx
	// The terminal states we just sent in hello have been delivered.
	for id, as := range a.assignments {
		if isTerminal(as.state) {
			delete(a.assignments, id)
		}
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.conn = nil
		a.mu.Unlock()
	}()

	a.applyConfig(ctx, w.Pools, w.Images)
	a.sendCapacity()

	interval := time.Duration(w.HeartbeatIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 20 * time.Second
	}
	go a.heartbeat(sctx, interval)

	for {
		raw, typ, err := read(sctx, conn)
		if err != nil {
			return err
		}
		switch typ {
		case protocol.TypeStart:
			var m protocol.Start
			if err := json.Unmarshal(raw, &m); err == nil {
				a.handleStart(ctx, m)
			}
		case protocol.TypeStop:
			var m protocol.Stop
			if err := json.Unmarshal(raw, &m); err == nil {
				a.handleStop(m)
			}
		case protocol.TypeAttach:
			var m protocol.Attach
			if err := json.Unmarshal(raw, &m); err == nil {
				// Tied to this connection: a stream ends when the connection that carries it does.
				a.handleAttach(sctx, m)
			}
		case protocol.TypeDetach:
			var m protocol.Detach
			if err := json.Unmarshal(raw, &m); err == nil {
				a.handleDetach(m)
			}
		case protocol.TypeConfig:
			var m protocol.Config
			if err := json.Unmarshal(raw, &m); err == nil {
				a.applyConfig(ctx, m.Pools, m.Images)
				a.sendCapacity()
			}
		default:
			a.log.Warn("ignoring unexpected message", "type", typ)
		}
	}
}

func (a *Agent) heartbeat(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	lastActive := a.o.Policy.Active(a.o.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.send(&protocol.Heartbeat{Type: protocol.TypeHeartbeat})
			// Re-announce capacity when active hours start or end.
			if active := a.o.Policy.Active(a.o.Now()); active != lastActive {
				lastActive = active
				a.sendCapacity()
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
func (a *Agent) send(m any) bool {
	a.mu.Lock()
	conn, ctx := a.conn, a.connCtx
	a.mu.Unlock()
	if conn == nil {
		return false
	}
	return write(ctx, conn, m) == nil
}

func isTerminal(s protocol.AgentAssignmentState) bool {
	return s == protocol.AgentAssignmentStateFinished || s == protocol.AgentAssignmentStateFailed
}

func (a *Agent) reports() []protocol.AssignmentReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]protocol.AssignmentReport, 0, len(a.assignments))
	for _, as := range a.assignments {
		out = append(out, protocol.AssignmentReport{AssignmentID: as.id, State: as.state, Reason: as.reason, Code: as.code, ExitCode: as.exitCode})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssignmentID < out[j].AssignmentID })
	return out
}

func (a *Agent) setState(id string, s protocol.AgentAssignmentState, reason string) {
	a.mu.Lock()
	as := a.assignments[id]
	if as == nil {
		a.mu.Unlock()
		return
	}
	as.state, as.reason = s, reason
	st := &protocol.Status{Type: protocol.TypeStatus, AssignmentID: id, State: s, Reason: reason}
	if isTerminal(s) {
		st.Code, st.ExitCode = as.code, as.exitCode
	}
	a.mu.Unlock()
	delivered := a.send(st)
	if isTerminal(s) {
		a.mu.Lock()
		if delivered {
			delete(a.assignments, id)
		} else {
			as.cancel = nil // keep it until a hello reports it
		}
		a.mu.Unlock()
		a.sendCapacity()
	}
}

// ----- capacity and images -----

// slotsTotal is the owner's cap on VMs of any kind. A guest platform may have a lower limit
// of its own (guestLimit).
func (a *Agent) slotsTotal() int {
	return a.o.Policy.MaxVMs
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
func (a *Agent) guestLimit(platform string) int {
	return a.o.Executor.MaxVMs(protocol.ImageSpec{Platform: platform})
}

// activeOn counts running VMs of one guest platform.
func (a *Agent) activeOn(platform string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, as := range a.assignments {
		if !isTerminal(as.state) && as.platform == platform {
			n++
		}
	}
	return n
}

// activeCount counts running VMs; hostActive counts work running on the host itself. The two
// have separate limits.
func (a *Agent) activeCount() int { return a.active(false) }
func (a *Agent) hostActive() int  { return a.active(true) }

func (a *Agent) active(host bool) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, as := range a.assignments {
		if !isTerminal(as.state) && as.host == host {
			n++
		}
	}
	return n
}

// hostSlots is how much work this host runs directly at once: none unless the owner allows it,
// and none where policy demands network isolation, which such work cannot have.
func (a *Agent) hostSlots() int {
	if a.o.HostExecutor == nil || a.o.Policy.RequireNetworkIsolation {
		return 0
	}
	return a.o.Policy.HostTasks.Slots()
}

func (a *Agent) accepting() bool {
	if a.Draining() != "" {
		return false
	}
	if !a.o.Policy.Active(a.o.Now()) {
		return false
	}
	return !(a.o.Policy.RequireNetworkIsolation && !a.isolationOK)
}

// freeHostSlots is zero until there is something the host can run: a runner to give a GitHub
// job (unlike a VM, the host has no image with one in it to fall back on), or a task its owner
// has declared.
func (a *Agent) freeHostSlots() int {
	if a.hostRunner() == "" && len(a.o.Policy.Tasks) == 0 {
		return 0
	}
	return max(0, a.hostSlots()-a.hostActive())
}

// activeTask counts runs of one task.
func (a *Agent) activeTask(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, as := range a.assignments {
		if !isTerminal(as.state) && as.task == name {
			n++
		}
	}
	return n
}

// declaredTasks are the tasks the owner's policy declares, for the control plane to choose
// hosts by. None on a host that runs no work itself.
func (a *Agent) declaredTasks() []protocol.TaskCapacity {
	if a.hostSlots() == 0 {
		return nil
	}
	var out []protocol.TaskCapacity
	for name, t := range a.o.Policy.Tasks {
		out = append(out, protocol.TaskCapacity{Name: name, TimeoutSeconds: int(t.Timeout() / time.Second), MaxConcurrent: t.MaxConcurrent})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// outputKey is the public half of the host's output key, as callers are given it.
func (a *Agent) outputKey() string {
	if a.o.OutputKey == nil || a.o.TaskOutput == nil {
		return ""
	}
	k, err := tasklog.PublicKey(a.o.OutputKey)
	if err != nil {
		return ""
	}
	return k
}

// hostRunner is the directory of the Actions runner built for this host's own system.
func (a *Agent) hostRunner() string {
	if a.o.Runner == nil {
		return ""
	}
	return a.o.Runner.Current(runtime.GOOS)
}

func (a *Agent) sendCapacity() {
	total := a.slotsTotal()
	free := max(0, total-a.activeCount())
	a.mu.Lock()
	ready := make([]string, 0, len(a.ready))
	for name, ok := range a.ready {
		if ok {
			ready = append(ready, name)
		}
	}
	a.mu.Unlock()
	sort.Strings(ready)
	var guests []protocol.GuestCapacity
	for _, p := range a.o.Executor.GuestPlatforms() {
		guests = append(guests, protocol.GuestCapacity{Platform: p, Limit: a.guestLimit(p)})
	}
	a.send(&protocol.Capacity{
		Type: protocol.TypeCapacity, SlotsTotal: total, SlotsFree: free, ImagesReady: ready, Accepting: a.accepting(), Guests: guests,
		HostSlotsTotal: a.hostSlots(), HostSlotsFree: a.freeHostSlots(), Tasks: a.declaredTasks(),
		OutputAddress: a.o.OutputAddress, OutputPin: a.o.OutputPin, OutputKey: a.outputKey(),
		Scripts:  a.o.Policy.WorkloadAllowed("script"),
		Draining: a.Draining(),
	})
}

// applyConfig records the images the control plane may ask for, and prepares those a pool of
// this host is set to and policy allows. The others are the account's other images, which a
// job may name in place of its pool's; one of those is fetched when a job first asks for it.
func (a *Agent) applyConfig(ctx context.Context, pools []protocol.PoolConfig, images []protocol.ImageSpec) {
	a.mu.Lock()
	a.images = map[string]protocol.ImageSpec{}
	for _, img := range images {
		a.images[img.Name] = img
	}
	a.pooled = map[string]bool{}
	for _, p := range pools {
		if p.Image != "" {
			a.pooled[p.Image] = true
		}
	}
	for name := range a.ready {
		if _, ok := a.images[name]; !ok {
			delete(a.ready, name)
		}
	}
	pooled := a.pooled
	a.mu.Unlock()
	for _, img := range images {
		if pooled[img.Name] && !a.o.Policy.ImageAllowed(img.Source) {
			a.log.Warn("image not allowed by local policy; not preparing it", "image", img.Name, "source", img.Source)
		}
	}
	a.prepareMissing(ctx)
}

// prepareMissing starts fetching each image the control plane wants, the owner allows and the
// host does not have. While draining it starts nothing: a download begun now would only hold
// up what the agent is draining for.
func (a *Agent) prepareMissing(ctx context.Context) {
	a.mu.Lock()
	images := make([]protocol.ImageSpec, 0, len(a.images))
	for _, img := range a.images {
		if a.pooled[img.Name] {
			images = append(images, img)
		}
	}
	a.mu.Unlock()
	for _, img := range images {
		if !a.o.Policy.ImageAllowed(img.Source) {
			continue
		}
		if a.o.Executor.Ready(ctx, img) {
			a.mu.Lock()
			a.ready[img.Name] = true
			a.mu.Unlock()
			continue
		}
		if a.Draining() != "" {
			continue
		}
		a.prepareAsync(ctx, img)
	}
}

func (a *Agent) prepareAsync(ctx context.Context, img protocol.ImageSpec) {
	a.mu.Lock()
	if a.preparing[img.Name] {
		a.mu.Unlock()
		return
	}
	a.preparing[img.Name] = true
	a.mu.Unlock()
	go func() {
		err := a.o.Executor.Prepare(ctx, img)
		a.mu.Lock()
		delete(a.preparing, img.Name)
		if err == nil {
			a.ready[img.Name] = true
		}
		a.mu.Unlock()
		if err != nil {
			a.log.Error("preparing image failed", "image", img.Name, "err", err)
			return
		}
		a.log.Info("image ready", "image", img.Name)
		a.sendCapacity()
	}()
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
		for name, img := range a.images {
			if a.ready[name] {
				imgs = append(imgs, img)
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
