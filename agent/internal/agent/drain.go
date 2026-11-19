package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// A draining agent takes no new work and lets what it has finish. It drains before it
// switches to a newer version of itself, and when the host's owner asks it to.

// Why an agent is draining, as the control plane and `mcc check` are told.
const (
	DrainUpgrade = "upgrade" // a newer version is installed and waits for the agent to be idle
	DrainOwner   = "owner"   // the host's owner asked, with `mcc drain`
)

// Drain stops the agent taking new work. A reason already there is kept: an upgrade that is
// waiting is not forgotten because the owner also asked, or the other way round.
func (a *Agent) Drain(reason string) {
	a.mu.Lock()
	already := a.draining[reason]
	a.draining[reason] = true
	a.mu.Unlock()
	if !already {
		a.log.Info("draining: no new work is taken, and what is running goes on", "why", reason)
		a.sendCapacity()
	}
}

// Resume takes back one reason for draining. The agent takes work again once none is left.
func (a *Agent) Resume(reason string) {
	a.mu.Lock()
	was := a.draining[reason]
	delete(a.draining, reason)
	left := len(a.draining)
	ctx := a.runCtx
	a.mu.Unlock()
	if !was {
		return
	}
	if left == 0 {
		a.log.Info("no longer draining")
		// Images that were asked for while draining were not started.
		if ctx != nil {
			a.prepareMissing(ctx)
		}
	}
	a.sendCapacity()
}

// Draining is why the agent is draining, or "" if it is not. An upgrade is named first: it
// is the one that ends by itself.
func (a *Agent) Draining() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.drainingLocked()
}

func (a *Agent) drainingLocked() string {
	switch {
	case a.draining[DrainUpgrade]:
		return DrainUpgrade
	case a.draining[DrainOwner]:
		return DrainOwner
	}
	return ""
}

// Idle reports whether the agent has nothing in hand: no work, not even a final status still
// to be delivered, and no image being fetched or refreshed.
func (a *Agent) Idle() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.assignments) == 0 && len(a.preparing) == 0 && a.refreshing == 0
}

// WaitIdle returns nil once the agent is idle, or ctx's error.
func (a *Agent) WaitIdle(ctx context.Context) error {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		if a.Idle() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Activity is what the agent is doing, for its owner.
type Activity struct {
	Version string `json:"version"`
	// Connected is whether it has a connection to the control plane now.
	Connected bool           `json:"connected"`
	Work      []ActivityWork `json:"work"`
	// Preparing lists images being fetched or made ready.
	Preparing []string `json:"preparing"`
	// Draining is why it takes no new work, or "".
	Draining string `json:"draining"`
	Idle     bool   `json:"idle"`
}

// ActivityWork is one piece of work the agent has.
type ActivityWork struct {
	ID    string                        `json:"id"`
	State protocol.AgentAssignmentState `json:"state"`
	// Kind is "task", "script" or "job" (a GitHub Actions job).
	Kind string `json:"kind"`
	Task string `json:"task,omitempty"`
	// OnHost is true for work that runs on the host itself, not in a VM.
	OnHost bool `json:"on_host"`
}

func (a *Agent) Activity() Activity {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := Activity{
		Version:   Version,
		Connected: a.conn != nil,
		Work:      []ActivityWork{},
		Preparing: []string{},
		Draining:  a.drainingLocked(),
		Idle:      len(a.assignments) == 0 && len(a.preparing) == 0 && a.refreshing == 0,
	}
	for _, as := range a.assignments {
		kind := "job"
		switch {
		case as.task != "":
			kind = "task"
		case as.script:
			kind = "script"
		}
		out.Work = append(out.Work, ActivityWork{ID: as.id, State: as.state, Kind: kind, Task: as.task, OnHost: as.host})
	}
	sort.Slice(out.Work, func(i, j int) bool { return out.Work[i].ID < out.Work[j].ID })
	for name := range a.preparing {
		out.Preparing = append(out.Preparing, name)
	}
	sort.Strings(out.Preparing)
	return out
}

// ControlHandler is what the agent serves to its owner on a socket only they can open:
//
//	GET  /v1/status   what the agent is doing (Activity)
//	POST /v1/drain    stop taking new work
//	POST /v1/resume   take work again
//
// Nothing outside the host reaches it. Draining a host is its owner's to decide, as its
// policy is.
func (a *Agent) ControlHandler() http.Handler {
	answer := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.Activity())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
		case r.Method == http.MethodPost && r.URL.Path == "/v1/drain":
			a.Drain(DrainOwner)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/resume":
			a.Resume(DrainOwner)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		answer(w)
	})
}
