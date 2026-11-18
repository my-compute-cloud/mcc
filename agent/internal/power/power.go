// Package power keeps the host awake while VMs run.
package power

import (
	"log/slog"
	"os/exec"
	"runtime"
	"sync"
)

// Awake holds a `caffeinate -i` process while at least one hold is active (macOS only).
type Awake struct {
	Log   *slog.Logger
	mu    sync.Mutex
	holds int
	cmd   *exec.Cmd
}

func (a *Awake) Acquire() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.holds++
	if a.holds == 1 && runtime.GOOS == "darwin" {
		cmd := exec.Command("caffeinate", "-i")
		if err := cmd.Start(); err != nil {
			a.Log.Warn("could not prevent sleep", "err", err)
			return
		}
		a.cmd = cmd
		go func() { _ = cmd.Wait() }()
	}
}

func (a *Awake) Release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.holds == 0 {
		return
	}
	a.holds--
	if a.holds == 0 && a.cmd != nil {
		_ = a.cmd.Process.Kill()
		a.cmd = nil
	}
}
