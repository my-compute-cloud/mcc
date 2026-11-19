package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// An agent is upgraded by installing a newer one where the service starts it from. Nothing
// stops the one that is running: it notices, finishes what it has, and then replaces itself
// with the new one. A system update never costs a running job.

// Upgrade says how the agent switches to a newer version of itself.
type Upgrade struct {
	// Path is the file the service started the agent from. When what is there changes, a
	// different agent has been installed.
	Path string
	// Argv is what to start it with.
	Argv []string
	// Wait is how long running work may hold the switch back. After it the agent switches
	// anyway and what is left is stopped. Zero waits as long as it takes.
	Wait time.Duration
	// Every is how often to look. Zero means 30 seconds.
	Every time.Duration
	// Exec replaces this process with the new agent; it returns only on failure. Nil uses the
	// system's exec. Tests replace it.
	Exec func(path string, argv, env []string) error
}

// installed identifies what is at a path: where it leads, and that file's size and time.
// Under Nix the path is a link that is pointed somewhere new; elsewhere the file is replaced.
func installed(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %d %d", real, st.Size(), st.ModTime().UnixNano()), nil
}

// WatchUpgrade looks for a newer agent until ctx ends. It returns only then, or not at all
// if it switches.
func (a *Agent) WatchUpgrade(ctx context.Context, u Upgrade) {
	if u.Every <= 0 {
		u.Every = 30 * time.Second
	}
	if u.Exec == nil {
		u.Exec = syscall.Exec
	}
	running, err := installed(u.Path)
	if err != nil {
		a.log.Warn("cannot tell which agent is installed, so a newer one will not be switched to by itself", "path", u.Path, "err", err)
		return
	}
	t := time.NewTicker(u.Every)
	defer t.Stop()
	var failed string // what could not be started, so that it is not tried in a loop
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now, err := installed(u.Path)
		if err != nil || now == running || now == failed {
			continue // while an install is half done the path may not resolve; look again
		}
		a.log.Info("a different agent is installed; switching to it once idle", "path", u.Path)
		a.Drain(DrainUpgrade)
		if !a.awaitSwitch(ctx, u, running) {
			a.Resume(DrainUpgrade)
			continue
		}
		a.log.Info("switching to the installed agent")
		err = u.Exec(u.Path, u.Argv, os.Environ())
		// Still here: it could not be started. Go on as before, and leave that one alone.
		a.log.Error("the installed agent could not be started; carrying on with this one", "err", err)
		failed, _ = installed(u.Path)
		a.Resume(DrainUpgrade)
	}
}

// awaitSwitch waits until the agent may replace itself: when it is idle, or when the wait
// has run out, in which case what is left is stopped first. It reports false if there is no
// longer anything to switch to: ctx ended, or the agent that was running is installed again.
func (a *Agent) awaitSwitch(ctx context.Context, u Upgrade, running string) bool {
	var deadline <-chan time.Time
	if u.Wait > 0 {
		timer := time.NewTimer(u.Wait)
		defer timer.Stop()
		deadline = timer.C
	}
	t := time.NewTicker(min(u.Every, time.Second))
	defer t.Stop()
	for {
		if a.Idle() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			a.log.Warn("work has held the switch back for as long as the policy allows; stopping it", "waited", u.Wait)
			a.shutdown()
			return true
		case <-t.C:
		}
		// An upgrade that was taken back is no reason to keep draining.
		if now, err := installed(u.Path); err == nil && now == running {
			a.log.Info("the agent that is running is the one installed again; not switching")
			return false
		}
	}
}
