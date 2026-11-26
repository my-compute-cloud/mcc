//go:build darwin

package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/cache"
	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

var otherImage = protocol.ImageSpec{Name: "tahoe", Source: "ghcr.io/cirruslabs/macos-tahoe-base:latest", Platform: "darwin"}

type warmHost struct {
	a         *Agent
	cp        *controlPlane
	f         *fake.Fake
	cacheRoot string
}

// startWarmHost is an agent whose executor's guests see a slot filled after boot, as Tart's
// macOS guests do, with a shared runner and a job cache.
func startWarmHost(t *testing.T, policy config.Policy) warmHost {
	t.Helper()
	f := fake.New()
	f.Late = true
	root := t.TempDir()
	runner := t.TempDir()
	jobs := cache.New(filepath.Join(root, "cache"), "", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cp, _, _, a := startTheAgent(t, policy, f, func(o *Options) {
		o.Cache = jobs
		o.Runner = oneRunner(runner)
		o.SlotDir = filepath.Join(root, "cache", "slots")
	})
	hello := <-cp.hellos
	cp.next(isType("capacity"))
	if want := policy.WarmVMsMax(); hello.Policy.WarmVms != want {
		t.Fatalf("the host said it keeps up to %d warm VMs, not %d", hello.Policy.WarmVms, want)
	}
	return warmHost{a: a, cp: cp, f: f, cacheRoot: filepath.Join(root, "cache")}
}

// askWarm is the control plane asking each host of the pool for n warm VMs of image, beside a
// pool of otherImage that asks for none.
func (w warmHost) askWarm(n int) {
	w.cp.send(map[string]any{
		"type": "config",
		"pools": []protocol.PoolConfig{
			{Name: "mac", Label: "mcc-macos", Image: image.Name, WarmVms: n},
			{Name: "other", Label: "mcc-tahoe", Image: otherImage.Name},
		},
		"images": []protocol.ImageSpec{image, otherImage},
	})
}

// warmFor waits for a capacity report that has a warm VM of image, or none.
func (w warmHost) warmFor(t *testing.T, want bool) {
	t.Helper()
	w.cp.next(func(m map[string]any) bool {
		if m["type"] != "capacity" {
			return false
		}
		names, _ := m["images_warm"].([]any)
		return slices.Contains(names, any(image.Name)) == want
	})
}

func warmIDs(f *fake.Fake) []string {
	started, _ := f.Snapshot()
	var out []string
	for _, id := range started {
		if strings.HasPrefix(id, "warm-") {
			out = append(out, id)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAWarmVMIsTiedToNoRepositoryUntilAJobIsGivenIt(t *testing.T) {
	w := startWarmHost(t, config.DefaultPolicy())
	unit := filepath.Join(w.cacheRoot, "data", "github.com+acme+one")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "earlier"), []byte("from an earlier job"), 0o644); err != nil {
		t.Fatal(err)
	}

	w.askWarm(1)
	w.warmFor(t, true)
	ids := warmIDs(w.f)
	if len(ids) != 1 {
		t.Fatalf("warm VMs booted: %v", ids)
	}
	warm := ids[0]
	_, _, opts := w.f.Given(warm)
	slot := opts.SharedDirs[slotShare]
	if !slot.Writable || opts.SharedDirs[runnerShare].Path == "" {
		t.Fatalf("a warm VM's shares: %+v", opts.SharedDirs)
	}
	if _, ok := opts.SharedDirs[cacheShare]; ok {
		t.Fatal("a warm VM was given a cache before it had a job")
	}
	if entries, _ := os.ReadDir(slot.Path); len(entries) != 0 {
		t.Fatalf("a warm VM's slot holds %v before it has a job", entries)
	}
	// The runner is copied in while the VM waits, not while a job does.
	if cmds := w.f.Commands(warm); len(cmds) != 1 || cmds[0] != warmUpScript {
		t.Fatalf("commands run in a warm VM: %q", cmds)
	}

	var sawCache bool
	var mu sync.Mutex
	w.f.During = func(h executor.Handle, script string) {
		if script == warmUpScript {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		b, err := os.ReadFile(filepath.Join(slot.Path, slotCache, "earlier"))
		sawCache = err == nil && string(b) == "from an earlier job"
		_ = os.WriteFile(filepath.Join(slot.Path, slotCache, "built"), []byte("by this job"), 0o644)
	}
	m := startMsg("a1")
	m["workload"].(map[string]any)["jit_config"] = jitFor("https://github.com/acme/one")
	w.cp.send(m)
	w.cp.next(status("a1", "finished"))

	if slices.Contains(func() []string { s, _ := w.f.Snapshot(); return s }(), "a1") {
		t.Fatal("the job had a VM booted for it although one was warm")
	}
	if w.f.Stdin[warm] != jitFor("https://github.com/acme/one")+"\n" {
		t.Fatal("the job's runner configuration did not reach the warm VM")
	}
	mu.Lock()
	if !sawCache {
		t.Fatal("the job did not find its repository's cache in the slot")
	}
	mu.Unlock()
	if b, err := os.ReadFile(filepath.Join(unit, "built")); err != nil || string(b) != "by this job" {
		t.Fatalf("what the job added to its cache was not kept: %q, %v", b, err)
	}
	if _, err := os.Stat(slot.Path); err == nil {
		t.Fatal("the slot was left behind")
	}
	_, stopped := w.f.Snapshot()
	if !slices.Contains(stopped, warm) {
		t.Fatal("the job's VM was not deleted")
	}
	// Another takes its place.
	waitFor(t, "another warm VM", func() bool { return len(warmIDs(w.f)) == 2 })
}

func TestAJobWithNoWarmVMOfItsImageStopsOneToMakeRoom(t *testing.T) {
	w := startWarmHost(t, config.DefaultPolicy())
	w.askWarm(2)
	waitFor(t, "two warm VMs", func() bool { return len(warmIDs(w.f)) == 2 && w.f.Running() == 2 })

	m := startMsg("b1")
	m["image"] = otherImage
	w.cp.send(m)
	w.cp.next(status("b1", "finished"))

	started, stopped := w.f.Snapshot()
	if !slices.Contains(started, "b1") {
		t.Fatalf("the job's VM was not booted: %v", started)
	}
	warm := warmIDs(w.f)
	if !slices.Contains(stopped, warm[0]) && !slices.Contains(stopped, warm[1]) {
		t.Fatalf("no warm VM was stopped to make room: stopped %v", stopped)
	}
	// Booted with a slot like any VM of the image's kind, and filled for the job.
	_, _, opts := w.f.Given("b1")
	if _, ok := opts.SharedDirs[slotShare]; !ok {
		t.Fatalf("a VM booted for its job: %+v", opts.SharedDirs)
	}
	waitFor(t, "the warm VMs back", func() bool { return w.f.Running() == 2 })
}

func TestAWarmVMOfAnOlderImageIsNotGivenAJob(t *testing.T) {
	w := startWarmHost(t, config.DefaultPolicy())
	w.f.SetVersion("v1")
	w.askWarm(1)
	w.warmFor(t, true)
	old := warmIDs(w.f)[0]

	w.f.SetVersion("v2")
	w.cp.send(startMsg("a1"))
	w.cp.next(status("a1", "finished"))
	if started, _ := w.f.Snapshot(); !slices.Contains(started, "a1") {
		t.Fatal("the job was given a VM of the older image")
	}
	waitFor(t, "the older warm VM stopped and a new one booted", func() bool {
		_, stopped := w.f.Snapshot()
		return slices.Contains(stopped, old) && len(warmIDs(w.f)) == 2
	})
}

func TestTheOwnerCanSayNoToWarmVMs(t *testing.T) {
	policy := config.DefaultPolicy()
	none := 0
	policy.WarmVMs = &none
	w := startWarmHost(t, policy)
	w.askWarm(1)
	w.cp.send(startMsg("a1"))
	w.cp.next(status("a1", "finished"))
	time.Sleep(50 * time.Millisecond)
	if ids := warmIDs(w.f); len(ids) != 0 {
		t.Fatalf("warm VMs booted against the owner's policy: %v", ids)
	}
}

func TestDrainingStopsTheWarmVMs(t *testing.T) {
	w := startWarmHost(t, config.DefaultPolicy())
	w.askWarm(1)
	w.warmFor(t, true)
	a := w.a
	a.Drain(DrainOwner)
	waitFor(t, "the warm VM stopped", func() bool { return w.f.Running() == 0 })
	a.Resume(DrainOwner)
	waitFor(t, "a warm VM again", func() bool { return w.f.Running() == 1 })
}

func TestTheHostKeepsNoMoreWarmVMsThanItsOwnerAllows(t *testing.T) {
	policy := config.DefaultPolicy()
	one := 1
	policy.WarmVMs = &one
	w := startWarmHost(t, policy)
	w.askWarm(2)
	w.warmFor(t, true)
	time.Sleep(50 * time.Millisecond)
	if ids := warmIDs(w.f); len(ids) != 1 {
		t.Fatalf("warm VMs booted: %v", ids)
	}
}

func TestAHostThatCannotFillASlotLaterSaysItKeepsNoWarmVMs(t *testing.T) {
	cp, _, _ := startAgent(t, config.DefaultPolicy(), fake.New())
	if got := (<-cp.hellos).Policy.WarmVms; got != 0 {
		t.Fatalf("the host said it keeps up to %d warm VMs", got)
	}
}
