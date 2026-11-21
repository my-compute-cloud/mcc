package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
	"github.com/my-compute-cloud/mcc/agent/internal/identity"
)

// startClouds runs one agent enrolled in several clouds, each with a control plane and a key
// of its own, and returns the control planes in the order given.
func startClouds(t *testing.T, machine config.Policy, f *fake.Fake, clouds ...Cloud) (*Agent, []*controlPlane) {
	t.Helper()
	var cps []*controlPlane
	for i := range clouds {
		seed := make([]byte, 32)
		seed[0] = byte(i + 1)
		id := identity.FromSeed(seed)
		cp := newControlPlane(t, id)
		cps = append(cps, cp)
		clouds[i].Identity, clouds[i].HostID = id, "host-"+clouds[i].Name
		if clouds[i].ServerURL == "" {
			clouds[i].ServerURL = cp.srv.URL
		}
	}
	a := New(Options{
		Clouds: clouds, Policy: machine, Executor: f,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = a.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return a, cps
}

// forCloud is the machine's policy as it governs a cloud with this policy of its own.
func forCloud(t *testing.T, machine config.Policy, c config.CloudPolicy) config.Policy {
	t.Helper()
	p, err := machine.ForCloud(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func free(n int) func(map[string]any) bool {
	return func(m map[string]any) bool {
		return m["type"] == "capacity" && m["slots_free"] == float64(n)
	}
}

func TestCloudsShareTheMachineAndEachKeepsToItsOwnCap(t *testing.T) {
	f := fake.New()
	f.Block = true
	machine := config.DefaultPolicy()
	machine.MaxVMs = 2
	_, cps := startClouds(t, machine, f,
		Cloud{Name: "default", Policy: machine},
		Cloud{Name: "acme", Policy: forCloud(t, machine, config.CloudPolicy{MaxVMs: 1})})
	mine, acme := cps[0], cps[1]
	if h := <-mine.hellos; h.HostID != "host-default" || h.Policy.MaxVms != 2 {
		t.Fatalf("hello %+v", h)
	}
	// Each cloud knows this machine as a host of its own, and is told the policy that governs
	// its own work here.
	if h := <-acme.hellos; h.HostID != "host-acme" || h.Policy.MaxVms != 1 {
		t.Fatalf("hello %+v", h)
	}
	if c := acme.next(free(1)); c["slots_total"] != float64(1) {
		t.Fatalf("a cloud is told its own cap: %v", c)
	}
	if c := mine.next(free(2)); c["slots_total"] != float64(2) {
		t.Fatalf("capacity %v", c)
	}

	// What one cloud takes, the other is told it no longer has.
	mine.send(startMsg("m1"))
	mine.next(status("m1", "running"))
	mine.send(startMsg("m2"))
	mine.next(status("m2", "running"))
	acme.next(free(0))

	// A start that was already on its way is refused for want of room, which the control
	// plane takes elsewhere.
	acme.send(startMsg("a1"))
	if m := acme.next(status("a1", "failed")); m["reason"] != "refused: no free slot" || m["code"] != codeRefusedCapacity {
		t.Fatalf("the machine was full: %v", m)
	}

	// And what one gives back, the other is told it may have.
	mine.send(map[string]any{"type": "stop", "assignment_id": "m1"})
	mine.next(status("m1", "finished"))
	acme.next(free(1))
	acme.send(startMsg("a2"))
	acme.next(status("a2", "running"))
	mine.next(free(0))

	// A cloud keeps to its own cap though the machine has room.
	mine.send(map[string]any{"type": "stop", "assignment_id": "m2"})
	mine.next(status("m2", "finished"))
	acme.send(startMsg("a3"))
	if m := acme.next(status("a3", "failed")); m["reason"] != "refused: no free slot" {
		t.Fatalf("a cloud went over its cap: %v", m)
	}
	if f.Running() != 1 {
		t.Fatalf("running: %d", f.Running())
	}
}

func TestACloudIsNotGivenWhatTheOwnerGaveAnother(t *testing.T) {
	f := fake.New()
	machine := config.DefaultPolicy()
	machine.AllowedWorkloads = []string{"github-runner", "script"}
	machine.AllowedImages = []string{"ghcr.io/cirruslabs/*"}
	_, cps := startClouds(t, machine, f,
		Cloud{Name: "default", Policy: machine},
		Cloud{Name: "acme", Policy: forCloud(t, machine, config.CloudPolicy{ExcludedImages: []string{image.Source}})})
	mine, acme := cps[0], cps[1]
	<-mine.hellos
	<-acme.hellos
	if c := mine.next(isType("capacity")); c["scripts"] != true {
		t.Fatalf("the first cloud may send scripts: %v", c)
	}
	if c := acme.next(isType("capacity")); c["scripts"] == true {
		t.Fatalf("a cloud was offered scripts its owner gave only to another: %v", c)
	}
	// Each cloud's images are judged by its own policy.
	acme.send(startMsg("a1"))
	if m := acme.next(status("a1", "failed")); m["code"] != codeRefusedPolicy {
		t.Fatalf("an image the cloud's policy excludes: %v", m)
	}
	mine.send(startMsg("m1"))
	mine.next(status("m1", "finished"))
}

func TestACloudThatCannotBeReachedCostsTheOthersNothing(t *testing.T) {
	f := fake.New()
	machine := config.DefaultPolicy()
	// A control plane that refuses every connection.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	a, cps := startClouds(t, machine, f,
		Cloud{Name: "default", Policy: machine},
		Cloud{Name: "gone", Policy: machine, ServerURL: down.URL})
	mine := cps[0]
	<-mine.hellos
	mine.send(startMsg("m1"))
	mine.next(status("m1", "running"))
	mine.next(status("m1", "finished"))

	act := a.Activity()
	if act.Connected || len(act.Clouds) != 2 || !act.Clouds[0].Connected || act.Clouds[1].Connected {
		t.Fatalf("which clouds are connected: %+v", act)
	}
}

func TestAnImageTwoCloudsNameIsFetchedOnceAndThereForBoth(t *testing.T) {
	f := fake.New()
	machine := config.DefaultPolicy()
	_, cps := startClouds(t, machine, f,
		Cloud{Name: "default", Policy: machine},
		Cloud{Name: "acme", Policy: forCloud(t, machine, config.CloudPolicy{})})
	ready := func(m map[string]any) bool {
		imgs, _ := m["images_ready"].([]any)
		return m["type"] == "capacity" && len(imgs) == 1
	}
	for _, cp := range cps {
		<-cp.hellos
		cp.next(ready)
	}
}
