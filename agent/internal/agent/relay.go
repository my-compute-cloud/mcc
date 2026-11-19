package agent

import (
	"context"
	"sync"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
	"github.com/my-compute-cloud/mcc/agent/internal/tasklog"
)

// The most relayed streams at once, and the least time between two parts of one: together they
// bound what reading output can cost the connection that also carries the host's work.
const (
	maxStreams   = 8
	partInterval = 100 * time.Millisecond
)

// streams are the relayed reads in progress, by stream id.
type streams struct {
	mu     sync.Mutex
	cancel map[string]context.CancelFunc
}

// One answer for everything a caller might be probing for: no such task here, no secret, or
// not the right one.
const relayRefused = "no output for that task and secret on this host"

// handleAttach starts sending a task's output to a caller attached through the control
// plane. The caller proves it holds the task's secret, and every part is sealed for it: the
// control plane, which carries the stream, can read none of it.
func (a *Agent) handleAttach(ctx context.Context, m protocol.Attach) {
	refuse := func(why string) {
		a.send(&protocol.Output{Type: protocol.TypeOutput, StreamID: m.StreamID, Error: why})
	}
	store, key := a.o.TaskOutput, a.o.OutputKey
	if store == nil || key == nil {
		refuse(relayRefused)
		return
	}
	ch, secret, err := tasklog.HostChannel(key, m.ClientKey, m.Salt, m.Auth)
	if err != nil {
		refuse(relayRefused)
		return
	}
	meta, err := store.Meta(m.TaskID)
	if err != nil || !meta.Allows(secret) || int64(m.Offset) > store.Size(m.TaskID) || m.Offset < 0 {
		refuse(relayRefused)
		return
	}

	sctx, cancel := context.WithCancel(ctx)
	a.streams.mu.Lock()
	if a.streams.cancel == nil {
		a.streams.cancel = map[string]context.CancelFunc{}
	}
	if _, dup := a.streams.cancel[m.StreamID]; dup || len(a.streams.cancel) >= maxStreams {
		a.streams.mu.Unlock()
		cancel()
		refuse("this host is sending as many streams as it will; try again shortly")
		return
	}
	a.streams.cancel[m.StreamID] = cancel
	a.streams.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			a.streams.mu.Lock()
			delete(a.streams.cancel, m.StreamID)
			a.streams.mu.Unlock()
		}()
		w := &partWriter{a: a, ch: ch, stream: m.StreamID, offset: int64(m.Offset), ctx: sctx}
		end, err := store.Copy(w, m.TaskID, int64(m.Offset), m.Follow, sctx.Done(), nil)
		if sctx.Err() != nil {
			return // the caller left, or the connection that carried the stream is gone
		}
		if err != nil {
			refuse("reading the output failed")
			return
		}
		out := &protocol.Output{Type: protocol.TypeOutput, StreamID: m.StreamID, End: true, Truncated: end.Truncated, Code: end.Code}
		switch {
		case end.Running():
			out.Code = "running"
		case end.ExitCode != nil:
			out.ExitCode = *end.ExitCode
		}
		a.send(out)
	}()
}

func (a *Agent) handleDetach(m protocol.Detach) {
	a.streams.mu.Lock()
	cancel := a.streams.cancel[m.StreamID]
	a.streams.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// partWriter sends what it is given as sealed parts of one stream, no faster than one per
// partInterval: a task that prints a line at a time is read in fewer, larger parts.
type partWriter struct {
	a      *Agent
	ch     *tasklog.Channel
	stream string
	offset int64
	n      uint64
	last   time.Time
	ctx    context.Context
}

func (w *partWriter) Write(p []byte) (int, error) {
	if wait := partInterval - time.Since(w.last); w.n > 0 && wait > 0 {
		select {
		case <-time.After(wait):
		case <-w.ctx.Done():
			return 0, w.ctx.Err()
		}
	}
	ok := w.a.send(&protocol.Output{
		Type: protocol.TypeOutput, StreamID: w.stream, Seq: int(w.n), Offset: int(w.offset),
		Data: w.ch.SealPart(w.n, w.offset, p),
	})
	if !ok {
		return 0, context.Canceled
	}
	w.n++
	w.offset += int64(len(p))
	w.last = time.Now()
	return len(p), nil
}
