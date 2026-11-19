package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/tasklog"
)

// mcc logs reads from the agent running on the same host, found through MCC_HOME.
func TestLogsPrintsATasksOutputFromTheRunningAgent(t *testing.T) {
	// A short path: a Unix socket's is limited to about a hundred bytes.
	home, err := os.MkdirTemp("/tmp", "mcc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("MCC_HOME", home)
	t.Setenv("MCC_TASK_SECRET", "")

	store := tasklog.New(filepath.Join(home, "task-output"), tasklog.Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = store.ServeUnix(ctx, logSocket(home)) }()
	for i := 0; ; i++ {
		if _, err := os.Stat(logSocket(home)); err == nil {
			break
		}
		if i > 500 {
			t.Fatal("the socket was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	l, err := store.Create(tasklog.Meta{ID: "task-1", Task: "build"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte("compiling\n"))
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = l.Write([]byte("done\n"))
		zero := 0
		l.Close(&zero, "")
	}()

	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	runErr := logs([]string{"-f", "task-1"})
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if runErr != nil || string(out) != "compiling\ndone\n" {
		t.Fatalf("mcc logs -f printed %q, %v", out, runErr)
	}

	if err := logs([]string{"no-such-task"}); err == nil || !strings.Contains(err.Error(), "no output for that task") {
		t.Errorf("a task that never ran here: %v", err)
	}
}
