//go:build darwin

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/cache"
	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/tart"
	"github.com/my-compute-cloud/mcc/agent/internal/identity"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// TestRealTartWarmVM boots a real warm VM and gives it a job's cache and Nix database, then
// runs the macOS entry command in it with the runner swapped for checks of what the job finds.
// It takes a few minutes and one of the host's two macOS VMs, so it only runs when asked:
//
//	MCC_TART_IMAGE=<registry image> [MCC_TART_XCODE=/Applications/Xcode.app] \
//	  MCC_TART_RUNNER=<runner directory> MCC_TART_DIR=<directory> \
//	  go test -run RealTartWarmVM -v -timeout 30m ./internal/agent
//
// The image must be prepared already, with what the host's agent adds to it: MCC_TART_XCODE is
// the policy's xcode. The test does not prepare it, because preparing an image deletes its other
// bases, and on a host that runs an agent one of those is the agent's.
//
// MCC_TART_DIR holds the caches and slots, and must be somewhere Tart may share from. With the
// host's Nix store present it is shared in too, and the check reads a path of the host's
// through it.
func TestRealTartWarmVM(t *testing.T) {
	source, runner, dir := os.Getenv("MCC_TART_IMAGE"), os.Getenv("MCC_TART_RUNNER"), os.Getenv("MCC_TART_DIR")
	if source == "" || runner == "" || dir == "" {
		t.Skip("MCC_TART_IMAGE, MCC_TART_RUNNER and MCC_TART_DIR are not all set")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	old := image
	image = protocol.ImageSpec{Name: "tahoe", Source: source, Platform: "darwin"}
	defer func() { image = old }()

	ex := tart.New(log)
	ex.Dir = filepath.Join(dir, "tart")
	ex.Xcode = os.Getenv("MCC_TART_XCODE")
	// Before the agent starts: it prepares a pool's image when it connects.
	if !ex.Ready(ctx, image) {
		t.Fatal("the image is not prepared with what MCC_TART_XCODE says the host adds; run the agent, or name its Xcode")
	}
	cacheRoot := filepath.Join(dir, "cache")
	jobs := cache.New(cacheRoot, "", 0, log)
	policy := config.DefaultPolicy()
	policy.ShareNixStore = true
	hostNix, err := NewHostNix(filepath.Join(dir, "nix"))
	if err != nil {
		t.Logf("without the host's Nix store: %v", err)
		hostNix = nil
	}

	unit := filepath.Join(cacheRoot, "data", "github.com+acme+one")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "seed"), []byte("from an earlier job"), 0o644); err != nil {
		t.Fatal(err)
	}

	id := identity.FromSeed(make([]byte, 32))
	cp := newControlPlane(t, id)
	a := New(Options{
		ServerURL: cp.srv.URL, HostID: "host-1", Identity: id, Policy: policy, Executor: ex,
		Runner: fixedRunner(runner), Cache: jobs, HostNix: hostNix, SlotDir: filepath.Join(cacheRoot, "slots"),
		Log: log, MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() {
		_ = a.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	<-cp.hellos

	asked := time.Now()
	cp.send(map[string]any{
		"type":   "config",
		"pools":  []protocol.PoolConfig{{Name: "mac", Label: "mcc-macos", Image: image.Name, WarmVms: 1}},
		"images": []protocol.ImageSpec{image},
	})
	deadline := time.After(10 * time.Minute)
	for warm := false; !warm; {
		select {
		case m := <-cp.msgs:
			names, _ := m["images_warm"].([]any)
			warm = m["type"] == "capacity" && slices.Contains(names, any(image.Name))
		case <-deadline:
			t.Fatal("no warm VM")
		}
	}
	t.Logf("warm after %s", time.Since(asked).Round(time.Millisecond))

	// What executeInSlot does for a job, with checks in place of the runner.
	c := a.clouds[0]
	spec := c.vmSpec(image)
	took := time.Now()
	vm := a.warm.take(ctx, spec, a.baseVersion(ctx, image))
	if vm == nil {
		t.Fatal("the warm VM was not given out")
	}
	defer a.warm.discard(vm.h, vm.slot)
	giveBack, err := jobs.Lend("https://github.com/acme/one", filepath.Join(vm.slot, slotCache))
	if err != nil {
		t.Fatal(err)
	}
	if hostNix != nil {
		if err := hostNix.SnapshotInto(ctx, filepath.Join(vm.slot, slotNixState)); err != nil {
			t.Fatal(err)
		}
	}
	filled := time.Since(took)

	// A path of the host's, which the guest must find in the host's store through the slot's
	// copy of the database.
	hostPath := ""
	if hostNix != nil {
		if p, err := filepath.EvalSymlinks("/run/current-system/sw/bin/sqlite3"); err == nil {
			parts := strings.SplitN(strings.TrimPrefix(p, "/nix/store/"), "/", 2)
			hostPath = "/nix/store/" + parts[0]
		}
	}
	checks := `echo "runner-copied-while-warm=$(test -e ` + runnerCopied + ` && echo yes || echo no)"
echo "cache=$(cat "$MCC_CACHE_DIR/seed" 2>&1)"
echo "built by this job" > "$MCC_CACHE_DIR/built"
echo "nix-state=$NIX_STATE"
if [ -n "` + hostPath + `" ]; then
  if nix --extra-experimental-features 'nix-command read-only-local-store' path-info \
      --store "local?store=/nix/store&real=$SHARES_URL/` + nixStoreShare + `&state=$NIX_STATE_URL&read-only=true" ` + hostPath + ` >/dev/null 2>&1; then
    echo "host-path=found"
  else
    echo "host-path=missing"
  fi
fi
`
	script := strings.Replace(githubRunnerScript, `./run.sh --jitconfig "$JIT" || rc=$?`, checks, 1)
	if script == githubRunnerScript {
		t.Fatal("the entry command no longer runs the runner where this test expects")
	}
	var out bytes.Buffer
	jit := base64.StdEncoding.EncodeToString([]byte("not a registration"))
	ran := time.Now()
	code, err := ex.Exec(ctx, vm.h, executor.Command{Script: script, Stdin: strings.NewReader(jit + "\n"), Output: &out})
	t.Logf("slot filled in %s; entry command up to the runner took %s", filled.Round(time.Millisecond), time.Since(ran).Round(time.Millisecond))
	t.Logf("guest said:\n%s", out.String())
	if err != nil || code != 0 {
		t.Fatalf("entry command: code %d, %v", code, err)
	}
	said := out.String()
	for _, want := range []string{"runner-copied-while-warm=yes", "cache=from an earlier job", "nix-state=/Volumes/My Shared Files/mcc-slot/nix-state"} {
		if !strings.Contains(said, want) {
			t.Errorf("guest did not say %q", want)
		}
	}
	if hostPath != "" && !strings.Contains(said, "host-path=found") {
		t.Errorf("the guest did not find %s in the host's store", hostPath)
	}

	a.warm.discard(vm.h, "")
	giveBack()
	if b, err := os.ReadFile(filepath.Join(unit, "built")); err != nil || string(b) != "built by this job\n" {
		t.Errorf("what the job wrote to its cache was not kept: %q, %v", b, err)
	}
}
