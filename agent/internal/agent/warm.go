package agent

// Warm VMs: VMs booted before there is a job for them, so that a job starts without waiting
// for a boot. A pool asks each of its hosts for some (PoolConfig.warm_vms), and the host's
// owner can say no (policy warm_vms).
//
// A VM's shares are fixed when it boots, and what a job is given depends on which repository
// the job is from: its cache above all. So a warm VM is booted tied to no repository. It is
// given an empty directory of its own, its slot, as a writable share, and when a job arrives
// the agent puts in the slot what that job needs: a clone of the job's cache, and a copy of
// the host's Nix database. The guest sees what the host puts there at once (executor.LateShares).
// The runner is registered only then, by the control plane, for the job's repository.
//
// Everything that is the same for every job is given at boot: the runner, already copied into
// the guest, and the host's Nix store. What differs from one cloud to another (whether VMs are
// cut off from the host's network, whether they have the host's Nix store) is part of what a
// warm VM is booted with, its spec, and a job takes only a VM of its own spec.
//
// A VM booted for a job that found no warm VM is made the same way, with a slot, and filled at
// once: there is one way a job's VM is given its cache on such an executor, whichever VM it is.
//
// Warm VMs count against the host's VMs: Apple allows two macOS guests at once whatever they are
// doing. A job that finds every VM in use by warm VMs not of its spec has one of them stopped
// to make room.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// vmSpec is what a VM for a GitHub job is booted with, before anyone knows whose job it is.
type vmSpec struct {
	image   protocol.ImageSpec
	isolate bool
	// runner is the host directory of the runner shared into the VM, or "".
	runner string
	// nixStore is the host's Nix store shared into the VM, or "".
	nixStore string
}

// key is the same for two specs exactly when a VM booted for one would do for the other. An
// image is known by its source: two clouds may call one image by different names.
func (s vmSpec) key() string {
	return strings.Join([]string{s.image.Source, guestPlatform(s.image), strconv.FormatBool(s.isolate), s.runner, s.nixStore}, "\x00")
}

// vmSpec is the spec of this cloud's VMs of an image.
func (c *cloud) vmSpec(img protocol.ImageSpec) vmSpec {
	s := vmSpec{image: img, isolate: c.o.Policy.NetworkIsolation() && c.isolationOK}
	if c.o.Policy.SharedRunnerEnabled() && c.o.Runner != nil {
		s.runner = c.o.Runner.Current(guestPlatform(img))
	}
	if c.o.HostNix != nil {
		s.nixStore = c.o.HostNix.Store()
	}
	return s
}

// usesSlot reports whether a GitHub job of this image runs in a VM with a slot: where the
// executor's guests see a slot filled after boot, and the agent has somewhere to keep slots.
func (c *cloud) usesSlot(img protocol.ImageSpec) bool {
	l, ok := c.o.Executor.(executor.LateShares)
	return ok && c.a.warm != nil && l.LateShares(img)
}

// baseVersion is the version of an image a VM booted now would be made from, or "" when the
// executor cannot say.
func (a *Agent) baseVersion(ctx context.Context, img protocol.ImageSpec) string {
	if v, ok := a.o.Executor.(executor.BaseVersions); ok {
		return v.BaseVersion(ctx, img)
	}
	return ""
}

type warmVM struct {
	spec    vmSpec
	key     string
	version string
	id      string
	slot    string
	h       executor.Handle
	booted  time.Time
	cancel  context.CancelFunc
	// done is closed when the VM has booted and is ready, or failed to; err says which.
	done chan struct{}
	err  error
}

func (vm *warmVM) ready() bool {
	select {
	case <-vm.done:
		return vm.err == nil
	default:
		return false
	}
}

// warmPool is the machine's warm VMs, whichever cloud's job takes them.
type warmPool struct {
	a   *Agent
	log *slog.Logger
	// root holds the slots, one directory per VM. It is on the volume of the job caches, which
	// a slot is given a clone of: a clone cannot cross volumes.
	root string

	mu sync.Mutex
	// vms are booting or ready, and not yet given to a job.
	vms []*warmVM
	// stopping are being stopped. They still hold a VM until they are gone.
	stopping   int
	stoppingOn map[string]int
	// failed is when a spec last failed to boot, so that a broken image is not booted again
	// and again.
	failed map[string]time.Time
}

// warmRetry is how long a spec that failed to boot is left before it is tried again.
const warmRetry = 5 * time.Minute

func newWarmPool(a *Agent, root string) *warmPool {
	// Slots a stopped agent left behind; their VMs are deleted by cleanupLeftovers.
	_ = os.RemoveAll(root)
	return &warmPool{a: a, log: a.log, root: root, stoppingOn: map[string]int{}, failed: map[string]time.Time{}}
}

// start boots a VM of a spec with an empty slot, under the given id. The caller stops it and
// removes the slot.
func (w *warmPool) start(ctx context.Context, spec vmSpec, id, slot string) (executor.Handle, error) {
	if err := os.MkdirAll(w.root, 0o700); err != nil {
		return executor.Handle{}, err
	}
	if err := os.Mkdir(slot, 0o755); err != nil {
		return executor.Handle{}, err
	}
	opts := executor.StartOptions{
		Resources:      w.a.o.Resources,
		IsolateNetwork: spec.isolate,
		SharedDirs:     map[string]executor.SharedDir{slotShare: {Path: slot, Writable: true}},
	}
	if spec.runner != "" {
		opts.SharedDirs[runnerShare] = executor.SharedDir{Path: spec.runner}
	}
	if spec.nixStore != "" {
		opts.SharedDirs[nixStoreShare] = executor.SharedDir{Path: spec.nixStore}
	}
	return w.a.o.Executor.Start(ctx, id, spec.image, opts)
}

// discard stops a VM and removes its slot.
func (w *warmPool) discard(h executor.Handle, slot string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if h.Name != "" {
		if err := w.a.o.Executor.Stop(ctx, h); err != nil {
			w.log.Error("deleting VM failed", "vm", h.Name, "err", err)
		}
	}
	if slot != "" {
		_ = os.RemoveAll(slot)
	}
}

// boot readies one warm VM. It runs on its own; vm.done says when it is over.
func (w *warmPool) boot(ctx context.Context, vm *warmVM) {
	started := time.Now()
	h, err := w.start(ctx, vm.spec, vm.id, vm.slot)
	vm.h = h
	if err == nil && vm.spec.runner != "" {
		var out lastLines
		code, xerr := w.a.o.Executor.Exec(ctx, h, executor.Command{Script: warmUpScript, Output: lineWriter{&out}})
		if xerr == nil && code != 0 {
			xerr = fmt.Errorf("copying the runner into the VM exited with code %d: %s", code, out.String())
		}
		err = xerr
	}
	vm.err = err
	vm.booted = time.Now()
	close(vm.done)
	if err != nil {
		w.mu.Lock()
		gone := w.remove(vm)
		if gone && ctx.Err() == nil {
			w.failed[vm.key] = time.Now()
		}
		w.mu.Unlock()
		if ctx.Err() == nil {
			w.log.Warn("booting a warm VM failed", "image", vm.spec.image.Name, "err", err)
		}
		if gone {
			w.discard(h, vm.slot)
		}
		return
	}
	w.log.Info("VM warm", "image", vm.spec.image.Name, "vm", h.Name, "boot", time.Since(started).Round(time.Millisecond))
	w.a.announce()
}

// remove takes a VM out of vms, and says whether it was there. w.mu is held.
func (w *warmPool) remove(vm *warmVM) bool {
	for i, v := range w.vms {
		if v == vm {
			w.vms = append(w.vms[:i], w.vms[i+1:]...)
			return true
		}
	}
	return false
}

// take gives a job a warm VM of its spec, waiting for one still booting, or returns nil when
// there is none to take. A VM from an older version of the image than a new one would have is
// not given out.
func (w *warmPool) take(ctx context.Context, spec vmSpec, version string) *warmVM {
	key := spec.key()
	w.mu.Lock()
	var pick *warmVM
	for _, vm := range w.vms {
		if vm.key != key || vm.version != version {
			continue
		}
		if vm.ready() {
			pick = vm
			break
		}
		if pick == nil {
			pick = vm
		}
	}
	if pick != nil {
		w.remove(pick)
	}
	w.mu.Unlock()
	if pick == nil {
		return nil
	}
	select {
	case <-pick.done:
	case <-ctx.Done():
		pick.cancel()
		<-pick.done
	}
	if pick.err != nil || ctx.Err() != nil {
		w.discard(pick.h, pick.slot)
		return nil
	}
	// Its boot is over, and the VM does not end with the context it was booted under.
	pick.cancel()
	// The control plane no longer finds a warm VM of this image here.
	go w.a.announce()
	return pick
}

// vmsOn counts the warm VMs of a platform, and all of them; those being stopped too. w.mu is
// held.
func (w *warmPool) vmsOn(platform string) (on, all int) {
	for _, vm := range w.vms {
		if guestPlatform(vm.spec.image) == platform {
			on++
		}
	}
	return on + w.stoppingOn[platform], len(w.vms) + w.stopping
}

// room says whether one more VM of a platform fits beside the jobs' VMs and the warm ones.
// w.mu is held.
func (w *warmPool) room(platform string, extra int) bool {
	on, all := w.vmsOn(platform)
	jobs := w.a.count(isVM)
	jobsOn := w.a.count(func(as *assignment) bool { return !as.host && as.platform == platform })
	if jobs+all+extra > w.a.o.Policy.MaxVMs {
		return false
	}
	lim := w.a.o.Executor.MaxVMs(protocol.ImageSpec{Platform: platform})
	return lim <= 0 || jobsOn+on+extra <= lim
}

// makeRoom stops warm VMs until a job of a platform, already counted among the jobs, has a VM
// to itself. It waits for each to be gone: the hypervisor counts it until then.
func (w *warmPool) makeRoom(platform string) {
	for {
		w.mu.Lock()
		if w.room(platform, 0) || len(w.vms) == 0 {
			w.mu.Unlock()
			return
		}
		// One of the job's platform frees what both limits count; a booting one is the
		// furthest from being of use.
		victim := w.vms[0]
		for _, vm := range w.vms {
			if guestPlatform(vm.spec.image) == platform && (guestPlatform(victim.spec.image) != platform || !vm.ready()) {
				victim = vm
			}
		}
		w.remove(victim)
		w.mu.Unlock()
		w.log.Info("stopping a warm VM to make room for a job", "image", victim.spec.image.Name)
		w.stop(victim)
	}
}

// stop ends a warm VM no job has taken, and waits until it is gone.
func (w *warmPool) stop(vm *warmVM) {
	platform := guestPlatform(vm.spec.image)
	w.mu.Lock()
	w.stopping++
	w.stoppingOn[platform]++
	w.mu.Unlock()
	vm.cancel()
	<-vm.done
	w.discard(vm.h, vm.slot)
	w.mu.Lock()
	w.stopping--
	w.stoppingOn[platform]--
	w.mu.Unlock()
}

// want is how many warm VMs of one spec the machine is asked for.
type want struct {
	spec    vmSpec
	version string
	n       int
}

// wanted adds up what each cloud's pools ask for, of what this host can and may keep warm.
func (a *Agent) wanted(ctx context.Context) map[string]*want {
	out := map[string]*want{}
	if a.warmMax() == 0 || a.Draining() != "" {
		return out
	}
	for _, c := range a.clouds {
		if !c.accepting() {
			continue
		}
		c.mu.Lock()
		asks := map[string]int{}
		for name, n := range c.warm {
			if c.ready[name] {
				asks[name] = n
			}
		}
		images := make(map[string]protocol.ImageSpec, len(asks))
		for name := range asks {
			images[name] = c.images[name]
		}
		c.mu.Unlock()
		for name, n := range asks {
			img := images[name]
			if n <= 0 || img.Source == "" || !c.o.Policy.ImageAllowed(img.Source) || !c.usesSlot(img) {
				continue
			}
			s := c.vmSpec(img)
			k := s.key()
			if out[k] == nil {
				out[k] = &want{spec: s, version: a.baseVersion(ctx, img)}
			}
			out[k].n += n
		}
	}
	// What the pools ask is shared out by each cloud's control plane, which knows the owner's
	// limit. Several clouds do not know of each other, so the limit is kept here too.
	left := a.warmMax()
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k].n = min(out[k].n, left)
		left -= out[k].n
	}
	return out
}

// warmMax is the most warm VMs this machine keeps: what its owner allows, and none where its
// executor cannot fill a VM's slot after boot.
func (a *Agent) warmMax() int {
	if a.warm == nil {
		return 0
	}
	l, ok := a.o.Executor.(executor.LateShares)
	if !ok {
		return 0
	}
	for _, p := range a.o.Executor.GuestPlatforms() {
		if l.LateShares(protocol.ImageSpec{Platform: p}) {
			return a.o.Policy.WarmVMsMax()
		}
	}
	return 0
}

// refill brings the warm VMs to what is wanted: it stops those no longer wanted, or of an older
// version of their image, and boots what is missing where there is room.
func (w *warmPool) refill() {
	w.a.mu.Lock()
	ctx := w.a.runCtx
	w.a.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return
	}
	wanted := w.a.wanted(ctx)

	w.mu.Lock()
	var surplus []*warmVM
	have := map[string]int{}
	for _, vm := range append([]*warmVM(nil), w.vms...) {
		d := wanted[vm.key]
		if d == nil || vm.version != d.version || have[vm.key] >= d.n {
			w.remove(vm)
			surplus = append(surplus, vm)
			continue
		}
		have[vm.key]++
	}
	keys := make([]string, 0, len(wanted))
	for k := range wanted {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := wanted[k]
		if t, ok := w.failed[k]; ok && time.Since(t) < warmRetry {
			continue
		}
		for have[k] < d.n && w.room(guestPlatform(d.spec.image), 1) {
			id := "warm-" + newID()[:12]
			bctx, cancel := context.WithCancel(ctx)
			vm := &warmVM{spec: d.spec, key: k, version: d.version, id: id, slot: filepath.Join(w.root, id), cancel: cancel, done: make(chan struct{})}
			w.vms = append(w.vms, vm)
			have[k]++
			go w.boot(bctx, vm)
		}
	}
	w.mu.Unlock()

	for _, vm := range surplus {
		go func() {
			w.stop(vm)
			// What it held may be wanted for another spec.
			w.refill()
		}()
	}
	if len(surplus) > 0 {
		w.a.announce()
	}
}

// stopAll ends every warm VM, for the agent shutting down.
func (w *warmPool) stopAll() {
	w.mu.Lock()
	vms := w.vms
	w.vms = nil
	w.mu.Unlock()
	var wg sync.WaitGroup
	for _, vm := range vms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.stop(vm)
		}()
	}
	wg.Wait()
}

// readyKeys are the specs a job would find a ready warm VM of now.
func (w *warmPool) readyKeys() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]bool{}
	for _, vm := range w.vms {
		if vm.ready() {
			out[vm.key] = true
		}
	}
	return out
}

// warmImages are this cloud's images a job would find a warm VM of now, for the control plane
// to prefer this host for their jobs.
func (c *cloud) warmImages(ctx context.Context) []string {
	if c.a.warm == nil {
		return nil
	}
	keys := c.a.warm.readyKeys()
	if len(keys) == 0 {
		return nil
	}
	c.mu.Lock()
	images := make([]protocol.ImageSpec, 0, len(c.images))
	for name, img := range c.images {
		if c.ready[name] {
			images = append(images, img)
		}
	}
	c.mu.Unlock()
	var out []string
	for _, img := range images {
		if keys[c.vmSpec(img).key()] {
			out = append(out, img.Name)
		}
	}
	sort.Strings(out)
	return out
}

// executeInSlot runs a GitHub job in a VM with a slot: a warm one if there is one of its spec,
// otherwise one booted for it now. Either way the job's cache and the copy of the host's Nix
// database are put in the slot only now that the job is here.
func (c *cloud) executeInSlot(ctx context.Context, as *assignment, m protocol.Start, lg *slog.Logger) (bool, string) {
	w := c.a.warm
	spec := c.vmSpec(m.Image)
	started := time.Now()
	var h executor.Handle
	var slot string
	if vm := w.take(ctx, spec, c.a.baseVersion(ctx, m.Image)); vm != nil {
		h, slot = vm.h, vm.slot
		lg.Info("VM started", "vm", h.Name, "warm", true, "waited", time.Since(started).Round(time.Millisecond))
	} else {
		w.makeRoom(guestPlatform(m.Image))
		slot = filepath.Join(w.root, m.AssignmentID)
		var err error
		h, err = w.start(ctx, spec, m.AssignmentID, slot)
		if err != nil {
			w.discard(h, slot)
			return false, fmt.Sprintf("starting VM: %v", err)
		}
		lg.Info("VM started", "vm", h.Name, "boot", time.Since(started).Round(time.Millisecond))
	}
	// The VM first, then what goes back into the cache once nothing can write the slot any
	// more, then the slot.
	var giveBack func()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := c.o.Executor.Stop(ctx, h); err != nil {
			lg.Error("deleting VM failed", "vm", h.Name, "err", err)
		} else {
			lg.Info("VM deleted", "vm", h.Name)
		}
		if giveBack != nil {
			giveBack()
			go c.o.Cache.Prune()
		}
		_ = os.RemoveAll(slot)
	}()

	if c.o.Cache != nil {
		if r, err := jitRunner(m.Workload.JITConfig); err != nil {
			lg.Warn("running without a cache", "err", fmt.Errorf("could not read the runner configuration to choose a cache: %w", err))
		} else if back, err := c.o.Cache.Lend(r.GitHubURL, filepath.Join(slot, slotCache)); err != nil {
			// A job that cannot be given its cache runs without one rather than with
			// someone else's.
			lg.Warn("running without a cache", "err", err)
		} else {
			giveBack = back
		}
	}
	if spec.nixStore != "" && c.o.HostNix != nil {
		// A job that cannot have the host's store runs without it, as on any other host.
		if err := c.o.HostNix.SnapshotInto(ctx, filepath.Join(slot, slotNixState)); err != nil {
			lg.Warn("running without the host's Nix store", "err", err)
		}
	}
	return c.runRunner(ctx, as, m, lg, c.o.Executor, h, entryScript(m.Image), nil)
}

// lineWriter keeps what is written to it in a lastLines, line by line.
type lineWriter struct{ l *lastLines }

func (w lineWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(string(b), "\n") {
		w.l.add(line)
	}
	return len(b), nil
}
