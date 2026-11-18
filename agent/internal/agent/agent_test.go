package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
	"github.com/my-compute-cloud/mcc/agent/internal/identity"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

var image = protocol.ImageSpec{Name: "sequoia", Source: "ghcr.io/cirruslabs/macos-sequoia-base:latest", Platform: "darwin"}

// controlPlane is a scripted control plane: it authenticates the agent and records messages.
type controlPlane struct {
	t      *testing.T
	pub    ed25519.PublicKey
	srv    *httptest.Server
	mu     sync.Mutex
	conn   *websocket.Conn
	msgs   chan map[string]any
	hellos chan protocol.Hello
}

func newControlPlane(t *testing.T, id *identity.Identity) *controlPlane {
	pubB, _ := base64.StdEncoding.DecodeString(id.PublicKey())
	cp := &controlPlane{t: t, pub: pubB, msgs: make(chan map[string]any, 100), hellos: make(chan protocol.Hello, 10)}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		nonce := "nonce-" + newID()
		cp.write(c, map[string]any{"type": "challenge", "nonce": nonce, "versions": []int{1}})
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		var h protocol.Hello
		_ = json.Unmarshal(b, &h)
		sig, _ := base64.StdEncoding.DecodeString(h.Signature)
		if !ed25519.Verify(cp.pub, []byte("mcc-hello\n"+nonce+"\n"+h.HostID), sig) {
			cp.write(c, map[string]any{"type": "reject", "reason": "authentication failed", "upgrade_required": false})
			c.Close(4003, "auth")
			return
		}
		cp.write(c, map[string]any{
			"type": "welcome", "version": 1, "tenant_id": "t1", "heartbeat_interval_seconds": 1,
			"pools":  []protocol.PoolConfig{{Name: "mac", Label: "mcc-macos", Image: image.Name}},
			"images": []protocol.ImageSpec{image},
		})
		cp.mu.Lock()
		cp.conn = c
		cp.mu.Unlock()
		cp.hellos <- h
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if m["type"] != "heartbeat" {
				cp.msgs <- m
			}
		}
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func (cp *controlPlane) write(c *websocket.Conn, m map[string]any) {
	m["v"], m["id"] = 1, newID()
	b, _ := json.Marshal(m)
	_ = c.Write(context.Background(), websocket.MessageText, b)
}

func (cp *controlPlane) send(m map[string]any) {
	cp.mu.Lock()
	c := cp.conn
	cp.mu.Unlock()
	cp.write(c, m)
}

func (cp *controlPlane) dropConnection() {
	cp.mu.Lock()
	c := cp.conn
	cp.conn = nil
	cp.mu.Unlock()
	c.CloseNow()
}

// next returns the next message matching pred, discarding others.
func (cp *controlPlane) next(pred func(map[string]any) bool) map[string]any {
	cp.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-cp.msgs:
			if pred(m) {
				return m
			}
		case <-timeout:
			cp.t.Fatal("timed out waiting for message")
		}
	}
}

func status(id, state string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		return m["type"] == "status" && m["assignment_id"] == id && m["state"] == state
	}
}

func isType(t string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["type"] == t }
}

func startAgent(t *testing.T, policy config.Policy, f *fake.Fake) (*controlPlane, context.CancelFunc, chan struct{}) {
	id := identity.FromSeed(make([]byte, 32))
	cp := newControlPlane(t, id)
	a := New(Options{
		ServerURL: cp.srv.URL, HostID: "host-1", Identity: id, Policy: policy, Executor: f,
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
	return cp, cancel, done
}

func startMsg(id string) map[string]any {
	return map[string]any{
		"type": "start", "assignment_id": id, "image": image,
		"workload": map[string]any{"kind": "github-runner", "runner_name": "mcc-" + id, "jit_config": "secret-jit"},
	}
}

func TestJobLifecycle(t *testing.T) {
	f := fake.New()
	f.Leave("stale") // from a previous crash
	cp, _, _ := startAgent(t, config.DefaultPolicy(), f)

	h := <-cp.hellos
	if h.HostID != "host-1" || len(h.Assignments) != 0 {
		t.Fatalf("unexpected hello %+v", h)
	}
	if _, stopped := f.Snapshot(); len(stopped) != 1 || stopped[0] != "stale" {
		t.Fatalf("leftover VM not deleted: %v", stopped)
	}
	c := cp.next(func(m map[string]any) bool {
		imgs, _ := m["images_ready"].([]any)
		return m["type"] == "capacity" && len(imgs) == 1
	})
	if c["slots_total"].(float64) != 2 || c["accepting"] != true {
		t.Fatalf("capacity %v", c)
	}

	cp.send(startMsg("a1"))
	cp.next(status("a1", "starting"))
	cp.next(status("a1", "running"))
	cp.next(status("a1", "finished"))
	started, stopped := f.Snapshot()
	if len(started) != 1 || stopped[len(stopped)-1] != "a1" || f.Running() != 0 {
		t.Fatalf("VM not cleaned up: started=%v stopped=%v", started, stopped)
	}
	if f.Stdin["a1"] != "secret-jit\n" {
		t.Fatalf("JIT config not passed on stdin: %q", f.Stdin["a1"])
	}
}

func TestRefusals(t *testing.T) {
	f := fake.New()
	f.Block = true
	policy := config.DefaultPolicy()
	policy.MaxVMs = 1
	cp, _, _ := startAgent(t, policy, f)
	<-cp.hellos

	bad := startMsg("b1")
	bad["image"] = protocol.ImageSpec{Name: "evil", Source: "docker.io/evil:latest", Platform: "darwin"}
	cp.send(bad)
	m := cp.next(status("b1", "failed"))
	if !strings.Contains(m["reason"].(string), "not in this host's pool configuration") {
		t.Fatalf("reason %v", m["reason"])
	}

	cp.send(startMsg("b2"))
	cp.next(status("b2", "running"))
	cp.send(startMsg("b3"))
	m = cp.next(status("b3", "failed"))
	if m["reason"] != "refused: no free slot" {
		t.Fatalf("reason %v", m["reason"])
	}

	// A repeated start has no further effect.
	cp.send(startMsg("b2"))
	cp.next(status("b2", "running"))
	if started, _ := f.Snapshot(); len(started) != 1 {
		t.Fatalf("duplicate start launched a VM: %v", started)
	}

	cp.send(map[string]any{"type": "stop", "assignment_id": "b2", "reason": "no queued job"})
	m = cp.next(status("b2", "finished"))
	if m["reason"] != "stopped by control plane: no queued job" || f.Running() != 0 {
		t.Fatalf("stop: %v running=%d", m, f.Running())
	}
}

func TestPolicyImagesAndHours(t *testing.T) {
	f := fake.New()
	policy := config.DefaultPolicy()
	policy.AllowedImages = []string{"ghcr.io/other/*"}
	cp, _, _ := startAgent(t, policy, f)
	<-cp.hellos
	cp.send(startMsg("c1"))
	m := cp.next(status("c1", "failed"))
	if !strings.Contains(m["reason"].(string), "not allowed by local policy") {
		t.Fatalf("reason %v", m["reason"])
	}
	if f.Ready(context.Background(), image) {
		t.Fatal("disallowed image was prepared")
	}
}

func TestStatusSurvivesDisconnect(t *testing.T) {
	f := fake.New()
	f.JobDuration = 300 * time.Millisecond
	cp, _, _ := startAgent(t, config.DefaultPolicy(), f)
	<-cp.hellos
	cp.send(startMsg("d1"))
	cp.next(status("d1", "running"))
	cp.dropConnection()

	// The job finishes while disconnected; the next hello reports it.
	h := <-cp.hellos
	for len(h.Assignments) == 0 || h.Assignments[0].State != protocol.AgentAssignmentStateFinished {
		if len(h.Assignments) == 1 && h.Assignments[0].State == protocol.AgentAssignmentStateRunning {
			cp.dropConnection()
			h = <-cp.hellos
			continue
		}
		t.Fatalf("hello after reconnect: %+v", h)
	}
	if h.Assignments[0].AssignmentID != "d1" {
		t.Fatalf("hello after reconnect: %+v", h)
	}
	// Once delivered it is forgotten.
	cp.dropConnection()
	if h := <-cp.hellos; len(h.Assignments) != 0 {
		t.Fatalf("finished assignment reported twice: %+v", h)
	}
}

func TestRunnerExitBeforeJobIsAFailure(t *testing.T) {
	f := fake.New()
	f.TakeJob = false
	f.ExitCode = 1
	cp, _, _ := startAgent(t, config.DefaultPolicy(), f)
	<-cp.hellos
	cp.send(startMsg("e1"))
	m := cp.next(status("e1", "failed"))
	if m["reason"] != "runner exited with code 1 before taking a job" {
		t.Fatalf("reason %v", m["reason"])
	}
}

func TestIdleTimeoutStopsUnusedRunner(t *testing.T) {
	f := fake.New()
	f.Block = true
	cp, _, _ := startAgent(t, config.DefaultPolicy(), f)
	<-cp.hellos
	s := startMsg("f1")
	s["workload"].(map[string]any)["idle_timeout_seconds"] = 1
	cp.send(s)
	m := cp.next(status("f1", "finished"))
	if m["reason"] != "idle timeout" {
		t.Fatalf("reason %v", m["reason"])
	}
}

func TestJITGitHubURL(t *testing.T) {
	runner := base64.StdEncoding.EncodeToString([]byte("\uFEFF" + `{"agentName":"x","gitHubUrl":"https://github.com/acme/builds"}`))
	files, _ := json.Marshal(map[string]string{".runner": runner, ".credentials": "e30="})
	u, err := jitGitHubURL(base64.StdEncoding.EncodeToString(files))
	if err != nil || u != "https://github.com/acme/builds" {
		t.Fatalf("got %q %v", u, err)
	}
	if _, err := jitGitHubURL("not base64!"); err == nil {
		t.Fatal("expected error")
	}
}
