package agent

import (
	"cmp"
	"context"
	"sync"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/config"
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
func (c *cloud) handleAttach(ctx context.Context, m protocol.Attach) {
	refuse := func(why string) {
		c.send(&protocol.Output{Type: protocol.TypeOutput, StreamID: m.StreamID, Error: why})
	}
	store, key := c.o.TaskOutput, c.o.OutputKey
	if store == nil || key == nil {
		refuse(relayRefused)
		return
	}
	ch, streamNonce, secret, err := tasklog.HostChannel(key, m.ClientKey, m.Salt, m.Auth, m.TaskID, int64(m.Offset), m.Follow)
	if err != nil {
		refuse(relayRefused)
		return
	}
	// A task's output is for the cloud that asked for the task, and no other.
	meta, err := store.Meta(m.TaskID)
	if err != nil || cmp.Or(meta.Cloud, config.DefaultCloud) != c.name || !meta.Allows(secret) || int64(m.Offset) > store.Size(m.TaskID) || m.Offset < 0 {
		refuse(relayRefused)
		return
	}

	sctx, cancel := context.WithCancel(ctx)
	c.streams.mu.Lock()
	if c.streams.cancel == nil {
		c.streams.cancel = map[string]context.CancelFunc{}
	}
	if _, dup := c.streams.cancel[m.StreamID]; dup || len(c.streams.cancel) >= maxStreams {
		c.streams.mu.Unlock()
		cancel()
		refuse("this host is sending as many streams as it will; try again shortly")
		return
	}
	c.streams.cancel[m.StreamID] = cancel
	c.streams.mu.Unlock()

	// First, what the caller needs to open this stream and no other.
	c.send(&protocol.Output{Type: protocol.TypeOutput, StreamID: m.StreamID, Nonce: streamNonce})
	go func() {
		defer func() {
			cancel()
			c.streams.mu.Lock()
			delete(c.streams.cancel, m.StreamID)
			c.streams.mu.Unlock()
		}()
		w := &partWriter{c: c, ch: ch, stream: m.StreamID, offset: int64(m.Offset), ctx: sctx}
		end, err := store.Copy(w, m.TaskID, int64(m.Offset), m.Follow, sctx.Done(), nil)
		if sctx.Err() != nil {
			return // the caller left, or the connection that carried the stream is gone
		}
		if err != nil {
			refuse("reading the output failed")
			return
		}
		// How it ended is sealed too: the control plane cannot give a task another exit code,
		// or end the stream early and have it pass for the whole output.
		sum := tasklog.Summary{Truncated: end.Truncated, Code: end.Code, ExitCode: end.ExitCode}
		if end.Running() {
			sum = tasklog.Summary{Truncated: end.Truncated, Code: "running"}
		}
		c.send(&protocol.Output{Type: protocol.TypeOutput, StreamID: m.StreamID, End: true, Summary: w.ch.SealEnd(w.n, w.offset, sum)})
	}()
}

func (c *cloud) handleDetach(m protocol.Detach) {
	c.streams.mu.Lock()
	cancel := c.streams.cancel[m.StreamID]
	c.streams.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// partWriter sends what it is given as sealed parts of one stream, no faster than one per
// partInterval: a task that prints a line at a time is read in fewer, larger parts.
type partWriter struct {
	c      *cloud
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
	ok := w.c.send(&protocol.Output{
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
