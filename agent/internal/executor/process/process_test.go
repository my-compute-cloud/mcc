package process

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

func newProcess(t *testing.T) *Process {
	t.Helper()
	root := t.TempDir()
	p := New(filepath.Join(root, "work"), filepath.Join(root, "state"), "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.Grace = 200 * time.Millisecond
	if problems := p.Preflight(context.Background()); len(problems) > 0 {
		t.Fatalf("preflight: %v", problems)
	}
	return p
}

func start(t *testing.T, p *Process, id string, opts executor.StartOptions) executor.Handle {
	t.Helper()
	h, err := p.Start(context.Background(), id, protocol.ImageSpec{}, opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return h
}

func TestWorkRunsInItsOwnDirectoryWhichIsDeletedAfterwards(t *testing.T) {
	p := newProcess(t)
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	t.Setenv("MCC_TEST_SECRET", "not for the work")

	h := start(t, p, "a1", executor.StartOptions{SharedDirs: map[string]executor.SharedDir{
		"runner": {Path: shared},
		"cache":  {Path: cache, Writable: true},
	}})
	var out bytes.Buffer
	code, err := p.Exec(context.Background(), h, executor.Command{
		Script: `read -r line; echo "stdin=$line"; pwd; echo "dir=$MCC_JOB_DIR"; echo "extra=$EXTRA"; echo "secret=${MCC_TEST_SECRET:-unset}"; test -x runner/run.sh && echo copied; test -e cache || echo "cache not copied"; touch made; exit 7`,
		Env:    []string{"EXTRA=yes"},
		Stdin:  strings.NewReader("hello\n"),
		Output: &out,
	})
	if err != nil || code != 7 {
		t.Fatalf("exec: code %d, err %v, output %s", code, err, out.String())
	}
	dir := filepath.Join(p.Dir, "job-a1")
	real, _ := filepath.EvalSymlinks(dir)
	for _, want := range []string{"stdin=hello", real, "dir=" + dir, "extra=yes", "secret=unset", "copied", "cache not copied"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "made")); err != nil {
		t.Errorf("the work's file should be in its directory until Stop: %v", err)
	}
	if left, _ := p.List(context.Background()); len(left) != 0 {
		t.Errorf("running work listed as left over: %v", left)
	}

	if err := p.Stop(context.Background(), h); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("directory still there after Stop: %v", err)
	}
	if entries, _ := os.ReadDir(p.StateDir); len(entries) != 0 {
		t.Errorf("state left behind: %v", entries)
	}
}

func TestCancellingKillsWhatTheWorkStarted(t *testing.T) {
	p := newProcess(t)
	h := start(t, p, "a2", executor.StartOptions{})
	marker := filepath.Join(t.TempDir(), "alive")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// A child that ignores the polite signal and keeps touching a file.
		_, _ = p.Exec(ctx, h, executor.Command{Script: `(trap '' TERM; while :; do touch "` + marker + `"; sleep 0.05; done) & wait`})
		close(done)
	}()
	waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Exec did not return after its context ended")
	}
	if err := p.Stop(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a process the work started is still running")
	}
}

func TestWorkLeftByACrashIsFoundAndCleanedUp(t *testing.T) {
	p := newProcess(t)
	h := start(t, p, "a3", executor.StartOptions{})
	marker := filepath.Join(t.TempDir(), "alive")
	go func() {
		_, _ = p.Exec(context.Background(), h, executor.Command{Script: `while :; do touch "` + marker + `"; sleep 0.05; done`})
	}()
	waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil })

	// A new agent, after the first one died without cleaning up, knows only what was written down.
	q := New(p.Dir, p.StateDir, "", p.Log)
	q.Grace = 200 * time.Millisecond
	left, err := q.List(context.Background())
	if err != nil || len(left) != 1 || left[0].AssignmentID != "a3" {
		t.Fatalf("List = %v, %v", left, err)
	}
	if err := q.Stop(context.Background(), left[0]); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the leftover work is still running")
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "job-a3")); !os.IsNotExist(err) {
		t.Errorf("the leftover directory is still there: %v", err)
	}
}

func TestAnAssignmentIDCannotLeaveTheWorkDirectory(t *testing.T) {
	p := newProcess(t)
	for _, id := range []string{"../x", "a/b", "", "-rf"} {
		if _, err := p.Start(context.Background(), id, protocol.ImageSpec{}, executor.StartOptions{}); err == nil {
			t.Errorf("Start(%q) should fail", id)
		}
	}
	// Cleaning up after a start that was refused touches nothing: the state file such an id
	// names would be some other file.
	policy := filepath.Join(filepath.Dir(p.StateDir), "policy.json")
	if err := os.MkdirAll(filepath.Dir(policy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background(), executor.Handle{AssignmentID: "../policy"}); err != nil {
		t.Errorf("Stop of a refused id: %v", err)
	}
	if _, err := os.Stat(policy); err != nil {
		t.Errorf("Stop deleted a file outside the state directory: %v", err)
	}
	// Nor can a state file send Stop to a directory that is not the work's.
	victim := t.TempDir()
	if err := p.writeState(state{AssignmentID: "evil", Dir: victim}); err != nil {
		t.Fatal(err)
	}
	_ = p.Stop(context.Background(), executor.Handle{AssignmentID: "evil"})
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("Stop deleted a directory outside the work directory: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAKeptDirectoryOutlivesTheWorkAndIsNeverSharedAtOnce(t *testing.T) {
	p := newProcess(t)
	opts := executor.StartOptions{KeptDirs: []executor.KeptDir{{Link: "runner/_work", Key: "github.com/acme/app", Wipe: []string{"_actions"}}}}
	run := func(h executor.Handle, script string) string {
		var out bytes.Buffer
		if code, err := p.Exec(context.Background(), h, executor.Command{Script: script, Output: &out}); err != nil || code != 0 {
			t.Fatalf("exec: %d %v %s", code, err, out.String())
		}
		return out.String()
	}

	first := start(t, p, "k1", opts)
	run(first, `mkdir -p runner/_work/_actions && touch runner/_work/checkout runner/_work/_actions/cached`)
	// A second job of the same repository while the first still runs gets a directory of its own.
	second := start(t, p, "k2", opts)
	if out := run(second, `ls runner/_work`); strings.TrimSpace(out) != "" {
		t.Errorf("two jobs at once shared a kept directory: %q", out)
	}
	for _, h := range []executor.Handle{first, second} {
		if err := p.Stop(context.Background(), h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "job-k1")); !os.IsNotExist(err) {
		t.Errorf("the job's own directory was kept: %v", err)
	}

	// The next job finds what the first left, without what must not carry over.
	third := start(t, p, "k3", opts)
	if out := run(third, `ls -A runner/_work`); strings.TrimSpace(out) != "checkout" {
		t.Errorf("kept directory holds %q, want only the checkout", out)
	}
	// Another repository's job gets another directory.
	other := start(t, p, "k4", executor.StartOptions{KeptDirs: []executor.KeptDir{{Link: "runner/_work", Key: "github.com/acme/other"}}})
	if out := run(other, `ls -A runner/_work`); strings.TrimSpace(out) != "" {
		t.Errorf("another repository's job saw %q", out)
	}

	for _, bad := range []executor.KeptDir{{Link: "../out", Key: "k"}, {Link: "w", Key: "///"}, {Link: "w", Key: "k", Wipe: []string{"../.."}}} {
		if _, err := p.Start(context.Background(), "bad", protocol.ImageSpec{}, executor.StartOptions{KeptDirs: []executor.KeptDir{bad}}); err == nil {
			t.Errorf("Start with kept directory %+v should fail", bad)
		}
		_ = p.Stop(context.Background(), executor.Handle{AssignmentID: "bad"})
	}
}
