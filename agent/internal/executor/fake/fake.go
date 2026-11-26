// Package fake is an in-memory executor for tests and for exercising the protocol on machines
// without virtualization (MCC_EXECUTOR=fake). Exec prints what a runner would and exits.
package fake

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

type Fake struct {
	mu       sync.Mutex
	commands map[string][]string
	prepared map[string]bool
	vms      map[string]executor.Handle
	Started  []string
	Stopped  []string
	Stdin    map[string]string
	// Scripts records the entry command each machine was given, by assignment.
	Scripts map[string]string
	// Options records what each machine was started with, by assignment.
	Options map[string]executor.StartOptions

	// JobDuration is how long Exec "runs" after taking a job. Zero exits immediately.
	JobDuration time.Duration
	// TakeJob controls whether Exec prints the line that means a job was taken.
	TakeJob bool
	// ExitCode is returned from Exec.
	ExitCode int
	// FailStart makes Start fail.
	FailStart error
	// Block makes Exec wait until its context ends.
	Block bool
	// Late makes it an executor whose macOS guests see what the host puts in a share after
	// boot (executor.LateShares), as Tart's do.
	Late bool
	// Version is what BaseVersion says of every image.
	Version string
	// StartDelay is how long Start takes, as a boot does.
	StartDelay time.Duration
	// During is called in Exec, as what runs in the machine would do its work.
	During func(h executor.Handle, script string)
}

func New() *Fake {
	return &Fake{commands: map[string][]string{}, prepared: map[string]bool{}, vms: map[string]executor.Handle{}, Stdin: map[string]string{}, Options: map[string]executor.StartOptions{}, Scripts: map[string]string{}, TakeJob: true}
}

func (f *Fake) Prepare(_ context.Context, image protocol.ImageSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared[image.Source] = true
	return nil
}

func (f *Fake) Refresh(ctx context.Context, image protocol.ImageSpec) error {
	return f.Prepare(ctx, image)
}

func (f *Fake) Ready(_ context.Context, image protocol.ImageSpec) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepared[image.Source]
}

func (f *Fake) Start(ctx context.Context, id string, image protocol.ImageSpec, opts executor.StartOptions) (executor.Handle, error) {
	f.mu.Lock()
	delay := f.StartDelay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return executor.Handle{AssignmentID: id, Name: "mcc-" + id}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Options[id] = opts
	h := executor.Handle{AssignmentID: id, Name: "mcc-" + id}
	if f.FailStart != nil {
		return h, f.FailStart
	}
	if !f.prepared[image.Source] {
		return h, fmt.Errorf("image %s not prepared", image.Name)
	}
	f.vms[id] = h
	f.Started = append(f.Started, id)
	return h, nil
}

func (f *Fake) Exec(ctx context.Context, h executor.Handle, c executor.Command) (int, error) {
	f.mu.Lock()
	f.Scripts[h.AssignmentID] = c.Script
	f.commands[h.AssignmentID] = append(f.commands[h.AssignmentID], c.Script)
	f.mu.Unlock()
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		f.mu.Lock()
		f.Stdin[h.AssignmentID] = string(b)
		f.mu.Unlock()
	}
	out := c.Output
	if out == nil {
		out = io.Discard
	}
	f.mu.Lock()
	during := f.During
	f.mu.Unlock()
	if during != nil {
		during(h, c.Script)
	}
	fmt.Fprintln(out, "√ Connected to GitHub")
	if f.Block {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	if f.TakeJob {
		fmt.Fprintln(out, "Running job: build")
	}
	select {
	case <-time.After(f.JobDuration):
	case <-ctx.Done():
		return -1, ctx.Err()
	}
	return f.ExitCode, nil
}

func (f *Fake) Stop(_ context.Context, h executor.Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vms, h.AssignmentID)
	f.Stopped = append(f.Stopped, h.AssignmentID)
	return nil
}

func (f *Fake) List(context.Context) ([]executor.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]executor.Handle, 0, len(f.vms))
	for _, h := range f.vms {
		out = append(out, h)
	}
	return out, nil
}

// Leave adds a VM as if left behind by a crash.
func (f *Fake) Leave(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[id] = executor.Handle{AssignmentID: id, Name: "mcc-" + id}
}

func (f *Fake) Running() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.vms)
}

func (f *Fake) LateShares(image protocol.ImageSpec) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Late && image.Platform != "linux"
}

func (f *Fake) BaseVersion(context.Context, protocol.ImageSpec) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Version
}

// SetVersion changes what BaseVersion says, as a refreshed image would.
func (f *Fake) SetVersion(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Version = v
}

// Commands are the commands run in a machine, in order; Scripts keeps only the last.
func (f *Fake) Commands(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands[id]...)
}

func (f *Fake) NetworkIsolationAvailable() bool { return true }
func (f *Fake) MaxVMs(image protocol.ImageSpec) int {
	if image.Platform == "linux" {
		return 0
	}
	return 2
}
func (f *Fake) GuestPlatforms() []string { return []string{"darwin", "linux"} }
func (f *Fake) Snapshot() (started, stopped []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Started...), append([]string(nil), f.Stopped...)
}

// Script is the entry command the machine for an assignment was given.
func (f *Fake) Script(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Scripts[id]
}

// Given returns what a machine was started and run with: its entry command, what that command
// read on stdin, and its start options.
func (f *Fake) Given(assignmentID string) (entry, stdin string, opts executor.StartOptions) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Scripts[assignmentID], f.Stdin[assignmentID], f.Options[assignmentID]
}
