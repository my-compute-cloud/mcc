package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/cache"
	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/kvm"
	"github.com/my-compute-cloud/mcc/agent/internal/identity"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
	"github.com/my-compute-cloud/mcc/agent/internal/runnerbin"
)

type fixedRunner string

func (r fixedRunner) Current(string) string { return string(r) }

// TestRealKVMJob runs the whole agent against a scripted control plane on a Linux host with
// KVM, with real VMs, so it only runs when asked:
//
//	MCC_KVM_IMAGE=<registry image or directory> [MCC_KVM_DIR=<state directory>] \
//	  go test -run RealKVMJob -v -timeout 1h ./internal/agent
//
// The control plane sends a start with a runner configuration that is no registration. The
// agent prepares the image, starts a VM with the real Actions runner and a cache shared in,
// runs the entry command, and, the runner never being given a job, stops and deletes the VM
// at the idle timeout.
func TestRealKVMJob(t *testing.T) {
	source := os.Getenv("MCC_KVM_IMAGE")
	if source == "" {
		t.Skip("MCC_KVM_IMAGE is not set")
	}
	dir := os.Getenv("MCC_KVM_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	old := image
	image = protocol.ImageSpec{Name: "ubuntu", Source: source, Platform: "linux"}
	defer func() { image = old }()

	k := kvm.New(filepath.Join(dir, "kvm"), log)
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	runner, err := runnerbin.New(filepath.Join(dir, "runner-linux"), "linux-"+arch).Update(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	policy := config.DefaultPolicy()
	policy.CacheDir = cacheDir

	id := identity.FromSeed(make([]byte, 32))
	cp := newControlPlane(t, id)
	a := New(Options{
		ServerURL: cp.srv.URL, HostID: "host-1", Identity: id, Policy: policy, Executor: k,
		Runner: fixedRunner(runner), Cache: cache.New(cacheDir, "runner", 0, log), Log: log,
		Resources:  executor.Resources{CPUs: 4, MemoryMB: 4096},
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() {
		_ = a.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	wait := func(what string, pred func(map[string]any) bool) map[string]any {
		t.Helper()
		timeout := time.After(15 * time.Minute)
		for {
			select {
			case m := <-cp.msgs:
				if pred(m) {
					return m
				}
			case <-timeout:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	<-cp.hellos
	c := wait("the image to be ready", func(m map[string]any) bool {
		imgs, _ := m["images_ready"].([]any)
		return m["type"] == "capacity" && len(imgs) == 1
	})
	t.Logf("capacity: %v", c)

	cp.send(map[string]any{
		"type": "start", "assignment_id": "real1", "image": image,
		"workload": map[string]any{
			"kind": "github-runner", "runner_name": "mcc-real1",
			"jit_config": jitFor("https://github.com/my-compute-cloud/none"), "idle_timeout_seconds": 75,
		},
	})
	wait("starting", status("real1", "starting"))
	wait("running", status("real1", "running"))
	if vms, _ := k.List(ctx); len(vms) != 1 || vms[0].Name != "mcc-real1" {
		t.Errorf("VMs while the job runs: %v", vms)
	}
	end := wait("the end of the assignment", func(m map[string]any) bool {
		return m["type"] == "status" && m["assignment_id"] == "real1" && (m["state"] == "finished" || m["state"] == "failed")
	})
	if end["state"] != "finished" || !strings.Contains(end["reason"].(string), "idle timeout") {
		t.Errorf("the assignment ended as %v (%v), not by its idle timeout", end["state"], end["reason"])
	}
	if vms, _ := k.List(ctx); len(vms) != 0 {
		t.Errorf("VMs left after the job: %v", vms)
	}
	if entries, _ := os.ReadDir(filepath.Join(cacheDir, "data")); len(entries) != 1 {
		t.Errorf("the job's cache directory was not made: %v", entries)
	}
}
