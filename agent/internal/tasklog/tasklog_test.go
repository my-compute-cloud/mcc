package tasklog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func intp(n int) *int { return &n }

func TestOutputIsKeptAndReadFromAnOffset(t *testing.T) {
	s := New(t.TempDir(), Limits{})
	l, err := s.Create(Meta{ID: "t1", Task: "build"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte("hello "))
	_, _ = l.Write([]byte("world\n"))
	l.Close(intp(3), "")

	var b bytes.Buffer
	m, err := s.Copy(&b, "t1", 6, false, nil, nil)
	if err != nil || b.String() != "world\n" {
		t.Fatalf("Copy = %q, %v", b.String(), err)
	}
	if m.Running() || m.ExitCode == nil || *m.ExitCode != 3 {
		t.Errorf("record %+v", m)
	}
	if _, err := s.Copy(&b, "t2", 0, false, nil, nil); err != ErrNotFound {
		t.Errorf("another task's output: %v", err)
	}
	if _, err := s.Create(Meta{ID: "../t1"}); err == nil {
		t.Error("an id that is a path was accepted")
	}
}

func TestFollowingSeesWhatIsWrittenUntilTheTaskEnds(t *testing.T) {
	s := New(t.TempDir(), Limits{})
	l, _ := s.Create(Meta{ID: "t1"})
	_, _ = l.Write([]byte("one\n"))

	pr, pw := newPipe()
	type result struct {
		m   Meta
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, err := s.Copy(pw, "t1", 0, true, nil, nil)
		done <- result{m, err}
	}()
	pr.expect(t, "one\n")
	_, _ = l.Write([]byte("two\n"))
	pr.expect(t, "one\ntwo\n")
	select {
	case <-done:
		t.Fatal("following ended while the task was running")
	case <-time.After(50 * time.Millisecond):
	}
	_, _ = l.Write([]byte("three\n"))
	l.Close(intp(0), "")
	r := <-done
	if r.err != nil || r.m.Running() || pr.String() != "one\ntwo\nthree\n" {
		t.Fatalf("after the end: %q %+v %v", pr.String(), r.m, r.err)
	}

	// Without follow, a reader of a running task gets what there is and is told it is running.
	l2, _ := s.Create(Meta{ID: "t2"})
	_, _ = l2.Write([]byte("so far"))
	var b bytes.Buffer
	m, err := s.Copy(&b, "t2", 0, false, nil, nil)
	if err != nil || b.String() != "so far" || !m.Running() {
		t.Fatalf("Copy = %q %+v %v", b.String(), m, err)
	}
	l2.Close(nil, "stopped")
}

func TestATaskCannotFillTheDisk(t *testing.T) {
	s := New(t.TempDir(), Limits{PerTask: 10})
	l, _ := s.Create(Meta{ID: "t1"})
	if n, err := l.Write([]byte("12345678")); n != 8 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	// The task is told its write succeeded: it must not fail because output was dropped.
	if n, err := l.Write([]byte("9abcdef")); n != 7 || err != nil {
		t.Fatalf("Write past the limit = %d, %v", n, err)
	}
	_, _ = l.Write([]byte("more"))
	l.Close(intp(0), "")
	var b bytes.Buffer
	m, _ := s.Copy(&b, "t1", 0, false, nil, nil)
	if b.String() != "123456789a" || !m.Truncated {
		t.Fatalf("kept %q, truncated %v", b.String(), m.Truncated)
	}
}

func TestOldOutputIsPrunedByAgeThenSizeAndNeverWhileRunning(t *testing.T) {
	s := New(t.TempDir(), Limits{Total: 25, KeepFor: time.Hour})
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	write := func(id string, end bool) {
		l, err := s.Create(Meta{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = l.Write([]byte("0123456789"))
		if end {
			l.Close(intp(0), "")
		}
	}
	write("ancient", true)
	now = now.Add(2 * time.Hour)
	write("older", true)
	now = now.Add(time.Minute)
	write("newer", true)
	now = now.Add(time.Minute)
	write("running", false)
	now = now.Add(time.Minute)
	write("newest", true)

	s.Prune()
	var left []string
	for _, m := range s.list() {
		left = append(left, m.ID)
	}
	// "ancient" is past its age. Of the rest, 40 bytes must become 25 or fewer: the oldest ended
	// runs go, and the running one stays whatever its age.
	if got := strings.Join(left, ","); got != "newest,running" {
		t.Fatalf("left after pruning: %s", got)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "older.log")); !os.IsNotExist(err) {
		t.Error("a pruned run's output file is still there")
	}
}

func TestRunsLeftOpenByAStoppedAgentAreClosed(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, Limits{})
	l, _ := s.Create(Meta{ID: "t1"})
	_, _ = l.Write([]byte("partial"))

	// A new agent starts with the same directory.
	s2 := New(dir, Limits{})
	s2.Recover()
	m, err := s2.Meta("t1")
	if err != nil || m.Running() || m.Code != "agent_stopped" {
		t.Fatalf("record %+v, %v", m, err)
	}
	var b bytes.Buffer
	if _, err := s2.Copy(&b, "t1", 0, true, nil, nil); err != nil || b.String() != "partial" {
		t.Fatalf("Copy = %q, %v", b.String(), err)
	}
}

func TestOnlyTheSecretOpensATasksOutputOffTheOwnersSocket(t *testing.T) {
	s := New(t.TempDir(), Limits{})
	l, _ := s.Create(Meta{ID: "t1", SecretHash: hashOf("s3cret")})
	_, _ = l.Write([]byte("private\n"))
	l.Close(intp(2), "")
	open, _ := s.Create(Meta{ID: "t2"}) // its caller chose no secret
	_, _ = open.Write([]byte("owner only\n"))
	open.Close(intp(0), "")

	public := httptest.NewServer(s.Handler(false))
	defer public.Close()
	owner := httptest.NewServer(s.Handler(true))
	defer owner.Close()
	ctx := context.Background()
	read := func(base, id, secret string) (string, End, error) {
		var b bytes.Buffer
		end, err := Read(ctx, public.Client(), base, id, secret, 0, true, &b)
		return b.String(), end, err
	}

	out, end, err := read(public.URL, "t1", "s3cret")
	if err != nil || out != "private\n" || end.ExitCode == nil || *end.ExitCode != 2 {
		t.Fatalf("with the secret: %q %+v %v", out, end, err)
	}
	for _, c := range []struct{ name, id, secret string }{
		{"no secret", "t1", ""},
		{"a wrong secret", "t1", "guess"},
		{"the hash itself", "t1", hashOf("s3cret")},
		{"a task with no secret", "t2", ""},
		{"a task with no secret, and a guess", "t2", "guess"},
		{"a task that never ran here", "t9", "s3cret"},
	} {
		out, _, err := read(public.URL, c.id, c.secret)
		if err == nil || out != "" {
			t.Errorf("%s: read %q, %v", c.name, out, err)
		}
	}
	// A task that is not here and a wrong secret get the same answer.
	_, _, missing := read(public.URL, "t9", "s3cret")
	_, _, wrong := read(public.URL, "t1", "guess")
	if missing.Error() != wrong.Error() {
		t.Errorf("answers differ: %q and %q", missing, wrong)
	}

	// The owner's own listener needs no secret.
	if out, _, err := read(owner.URL, "t2", ""); err != nil || out != "owner only\n" {
		t.Errorf("owner: %q, %v", out, err)
	}
	if out, _, err := read(owner.URL, "t1", ""); err != nil || out != "private\n" {
		t.Errorf("owner, a task with a secret: %q, %v", out, err)
	}
}

func TestOutputIsReadOverTheAgentsSocket(t *testing.T) {
	// A short path: a Unix socket's is limited to about a hundred bytes.
	dir, err := os.MkdirTemp("/tmp", "mcc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s := New(filepath.Join(dir, "out"), Limits{PerTask: 12})
	socket := filepath.Join(dir, "logs.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.ServeUnix(ctx, socket) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, err := os.Stat(socket); err == nil {
			if st.Mode().Perm() != 0o600 {
				t.Fatalf("socket mode %v", st.Mode().Perm())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the socket was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}

	l, _ := s.Create(Meta{ID: "t1"})
	_, _ = l.Write([]byte("line 1\n"))
	var first bytes.Buffer
	end, err := ReadUnix(ctx, socket, "t1", "", 0, false, &first)
	if err != nil || first.String() != "line 1\n" || !end.Running {
		t.Fatalf("while running: %q %+v %v", first.String(), end, err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = l.Write([]byte("line 2 is too long\n"))
		l.Close(intp(7), "")
	}()
	// Picking up where the first read stopped.
	var rest bytes.Buffer
	end, err = ReadUnix(ctx, socket, "t1", "", end.Read, true, &rest)
	if err != nil || rest.String() != "line " || end.ExitCode == nil || *end.ExitCode != 7 || !end.Truncated || end.Running {
		t.Fatalf("following: %q %+v %v", rest.String(), end, err)
	}
	if _, err := ReadUnix(ctx, socket, "t1", "", 999, false, &rest); err == nil {
		t.Error("an offset past the end was accepted")
	}
	if _, err := ReadUnix(ctx, socket, "nope", "", 0, false, &rest); err == nil || !strings.Contains(err.Error(), "no output") {
		t.Errorf("a task that never ran: %v", err)
	}
}

// pipe is a writer whose contents a test can wait on.
type pipe struct {
	ch  chan struct{}
	buf *lockedBuffer
}

func newPipe() (*pipe, *pipe) {
	p := &pipe{ch: make(chan struct{}, 100), buf: &lockedBuffer{}}
	return p, p
}

func (p *pipe) Write(b []byte) (int, error) {
	n, err := p.buf.Write(b)
	select {
	case p.ch <- struct{}{}:
	default:
	}
	return n, err
}

func (p *pipe) String() string { return p.buf.String() }

func (p *pipe) expect(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for p.String() != want {
		select {
		case <-p.ch:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("read %q, want %q", p.String(), want)
		}
	}
}

func TestARunsOutputCannotBeTakenOverByAnotherWithItsID(t *testing.T) {
	s := New(t.TempDir(), Limits{})
	l, err := s.Create(Meta{ID: "t1", Cloud: "ours", Task: "build", SecretHash: hashOf("ours")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte("private\n"))
	// While it runs, nobody starts another under its id, with a secret of their own.
	for _, cloud := range []string{"ours", "theirs"} {
		if _, err := s.Create(Meta{ID: "t1", Cloud: cloud, SecretHash: hashOf("theirs")}); err == nil {
			t.Fatalf("a run still going was replaced by one from %s", cloud)
		}
	}
	if m, _ := s.Meta("t1"); !m.Allows("ours") || m.Allows("theirs") {
		t.Fatal("the run's secret changed")
	}
	l.Close(intp(0), "")
	// Once it has ended, another cloud still cannot have its id; its own cloud may run it again.
	if _, err := s.Create(Meta{ID: "t1", Cloud: "theirs"}); err == nil {
		t.Fatal("another cloud replaced a run that had ended")
	}
	var b bytes.Buffer
	if _, err := s.Copy(&b, "t1", 0, false, nil, nil); err != nil || b.String() != "private\n" {
		t.Fatalf("the output is %q, %v", b.String(), err)
	}
	again, err := s.Create(Meta{ID: "t1", Cloud: "ours"})
	if err != nil {
		t.Fatalf("its own cloud could not run it again: %v", err)
	}
	again.Close(intp(0), "")
}
