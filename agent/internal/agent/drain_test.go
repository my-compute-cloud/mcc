package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
	"github.com/my-compute-cloud/mcc/agent/internal/identity"
)

// startOwned starts an agent the test can also talk to directly, as its owner would.
func startOwned(t *testing.T, f *fake.Fake) (*Agent, *controlPlane, context.Context) {
	id := identity.FromSeed(make([]byte, 32))
	cp := newControlPlane(t, id)
	a := New(Options{
		ServerURL: cp.srv.URL, HostID: "host-1", Identity: id, Policy: config.DefaultPolicy(), Executor: f,
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
	<-cp.hellos
	cp.next(func(m map[string]any) bool {
		imgs, _ := m["images_ready"].([]any)
		return m["type"] == "capacity" && len(imgs) == 1
	})
	return a, cp, ctx
}

func capacityWith(accepting bool, draining string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		d, _ := m["draining"].(string)
		return m["type"] == "capacity" && m["accepting"] == accepting && d == draining
	}
}

func TestDrainingTakesNothingNewAndLetsRunningWorkFinish(t *testing.T) {
	f := fake.New()
	f.Block = true
	a, cp, ctx := startOwned(t, f)

	cp.send(startMsg("d1"))
	cp.next(status("d1", "running"))
	if a.Idle() {
		t.Fatal("idle with work running")
	}

	a.Drain(DrainOwner)
	cp.next(capacityWith(false, "owner"))
	// A start that was already on its way is refused in the way the control plane retries.
	cp.send(startMsg("d2"))
	m := cp.next(status("d2", "failed"))
	if m["code"] != "refused_capacity" {
		t.Fatalf("a start while draining was refused as %v: %v", m["code"], m["reason"])
	}
	if started, _ := f.Snapshot(); len(started) != 1 {
		t.Fatalf("draining started work: %v", started)
	}
	// What was running is left alone.
	wait, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if a.WaitIdle(wait) == nil {
		t.Fatal("drained while work was still running")
	}
	if act := a.Activity(); act.Draining != "owner" || act.Idle || len(act.Work) != 1 || act.Work[0].ID != "d1" {
		t.Fatalf("activity %+v", act)
	}

	cp.send(map[string]any{"type": "stop", "assignment_id": "d1"})
	cp.next(status("d1", "finished"))
	if err := a.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}

	a.Resume(DrainOwner)
	cp.next(capacityWith(true, ""))
	cp.send(startMsg("d3"))
	cp.next(status("d3", "running"))
}

// installAgent puts a different "agent" at path, as an installer or a system update would.
func installAgent(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func watch(t *testing.T, a *Agent, ctx context.Context, path string, wait time.Duration, fail bool) chan string {
	execd := make(chan string, 4)
	go a.WatchUpgrade(ctx, Upgrade{
		Path: path, Argv: []string{path}, Wait: wait, Every: 10 * time.Millisecond,
		Exec: func(p string, _, _ []string) error {
			execd <- p
			if fail {
				return errors.New("cannot be started")
			}
			select {} // the process would be gone
		},
	})
	time.Sleep(30 * time.Millisecond) // it has noted what is installed now
	return execd
}

func never(t *testing.T, execd chan string, d time.Duration, why string) {
	t.Helper()
	select {
	case <-execd:
		t.Fatal(why)
	case <-time.After(d):
	}
}

func TestANewerAgentTakesOverOnlyOnceThisOneIsIdle(t *testing.T) {
	f := fake.New()
	f.Block = true
	a, cp, ctx := startOwned(t, f)
	path := filepath.Join(t.TempDir(), "mcc-agent")
	installAgent(t, path, "one")
	execd := watch(t, a, ctx, path, 0, true)
	never(t, execd, 50*time.Millisecond, "switched with nothing new installed")

	cp.send(startMsg("u1"))
	cp.next(status("u1", "running"))
	installAgent(t, path, "another")
	cp.next(capacityWith(false, "upgrade"))
	never(t, execd, 150*time.Millisecond, "switched while work was running")

	cp.send(map[string]any{"type": "stop", "assignment_id": "u1"})
	cp.next(status("u1", "finished"))
	select {
	case p := <-execd:
		if p != path {
			t.Fatalf("started %s", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not switch once idle")
	}
	// It could not be started: the agent carries on, and does not try the same one in a loop.
	cp.next(capacityWith(true, ""))
	never(t, execd, 100*time.Millisecond, "tried again to start an agent that cannot be started")
	cp.send(startMsg("u2"))
	cp.next(status("u2", "running"))
}

func TestWorkHoldsANewerAgentBackOnlyAsLongAsThePolicyAllows(t *testing.T) {
	f := fake.New()
	f.Block = true
	a, cp, ctx := startOwned(t, f)
	path := filepath.Join(t.TempDir(), "mcc-agent")
	installAgent(t, path, "one")
	execd := watch(t, a, ctx, path, 200*time.Millisecond, false)

	cp.send(startMsg("w1"))
	cp.next(status("w1", "running"))
	installAgent(t, path, "another")
	never(t, execd, 100*time.Millisecond, "did not wait at all")
	select {
	case <-execd:
	case <-time.After(3 * time.Second):
		t.Fatal("waited past the limit")
	}
	// What was left was stopped first, as a stop of the service would.
	if f.Running() != 0 {
		t.Fatalf("switched with %d VMs still running", f.Running())
	}
}

func TestTheOwnerCanPutAnotherBuildInTheServicesPlaceAndGoBack(t *testing.T) {
	f := fake.New()
	f.Block = true
	a, cp, ctx := startOwned(t, f)
	dir := t.TempDir()
	service, mine := filepath.Join(dir, "mcc-agent"), filepath.Join(dir, "my-build")
	installAgent(t, service, "installed")
	installAgent(t, mine, "mine")
	type started struct {
		path  string
		argv  []string
		local bool
	}
	execd := make(chan started, 4)
	go a.WatchUpgrade(ctx, Upgrade{
		Path: service, Argv: []string{service}, Every: 10 * time.Millisecond,
		Exec: func(p string, argv, env []string) error {
			execd <- started{p, argv, slices.Contains(env, "MCC_LOCAL_BUILD=1")}
			return errors.New("the test goes on in this process")
		},
	})
	time.Sleep(30 * time.Millisecond)

	if err := a.RequestSwitch("my-build", nil); err == nil {
		t.Fatal("took a path that says nothing about where")
	}
	if err := a.RequestSwitch(filepath.Join(dir, "missing"), nil); err == nil {
		t.Fatal("took a program that is not there")
	}

	// It waits for running work, as an upgrade does.
	cp.send(startMsg("s1"))
	cp.next(status("s1", "running"))
	if err := a.RequestSwitch(mine, []string{mine, "run", "-v"}); err != nil {
		t.Fatal(err)
	}
	cp.next(capacityWith(false, "upgrade"))
	select {
	case <-execd:
		t.Fatal("switched while work was running")
	case <-time.After(100 * time.Millisecond):
	}
	cp.send(map[string]any{"type": "stop", "assignment_id": "s1"})
	cp.next(status("s1", "finished"))
	got := <-execd
	if got.path != mine || !slices.Equal(got.argv, []string{mine, "run", "-v"}) || !got.local {
		t.Fatalf("started %+v", got)
	}

	// And back: the installed agent is started as the service starts it, and is not marked.
	cp.next(capacityWith(true, ""))
	if err := a.RequestSwitch("", nil); err != nil {
		t.Fatal(err)
	}
	got = <-execd
	if got.path != service || !slices.Equal(got.argv, []string{service}) || got.local {
		t.Fatalf("went back to %+v", got)
	}
}

func TestAnUpgradeThatIsTakenBackIsNotWaitedFor(t *testing.T) {
	f := fake.New()
	f.Block = true
	a, cp, ctx := startOwned(t, f)
	dir := t.TempDir()
	one, two, path := filepath.Join(dir, "one"), filepath.Join(dir, "two"), filepath.Join(dir, "mcc-agent")
	installAgent(t, one, "one")
	installAgent(t, two, "another")
	if err := os.Symlink(one, path); err != nil {
		t.Fatal(err)
	}
	execd := watch(t, a, ctx, path, 0, true)

	cp.send(startMsg("r1"))
	cp.next(status("r1", "running"))
	relink := func(to string) {
		_ = os.Remove(path)
		if err := os.Symlink(to, path); err != nil {
			t.Fatal(err)
		}
	}
	relink(two) // as a system update points its link at a new version
	cp.next(capacityWith(false, "upgrade"))
	relink(one) // and a rollback points it back
	cp.next(capacityWith(true, ""))
	never(t, execd, 100*time.Millisecond, "switched to the agent that was already running")
	if f.Running() != 1 {
		t.Fatal("the work did not survive")
	}
}
