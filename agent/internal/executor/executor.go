// Package executor is the agent's interface to virtualization. Adding a platform is adding an
// implementation; the agent core does not know about Tart.
package executor

import (
	"context"
	"io"

	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// Handle identifies a machine this agent created.
type Handle struct {
	AssignmentID string
	Name         string
}

// Resources for one machine.
type Resources struct {
	CPUs     int
	MemoryMB int
	// DiskGB grows the machine's disk to this size. Zero, or a size below the image's, leaves
	// the image's size: a disk can grow but not shrink.
	DiskGB int
}

// SharedDir is a host directory mounted in the guest.
type SharedDir struct {
	Path string
	// Writable lets the guest change the directory. Everything else is mounted read-only.
	Writable bool
}

// Options that apply to starting a machine.
type StartOptions struct {
	Resources Resources
	// IsolateNetwork asks for internet-only networking.
	IsolateNetwork bool
	// SharedDirs maps a share name to a host directory mounted in the guest.
	SharedDirs map[string]SharedDir
}

// Command is the workload's entry command. Agents build it from a fixed template per workload
// kind; the control plane never supplies a command.
type Command struct {
	Script string
	// Stdin carries secrets such as the runner configuration, so they never appear in process
	// arguments on the host.
	Stdin  io.Reader
	Output io.Writer
}

type Executor interface {
	// Prepare makes an image available locally. It may take a long time on first use.
	Prepare(ctx context.Context, image protocol.ImageSpec) error
	// Refresh pulls a newer version of a prepared image, if there is one.
	Refresh(ctx context.Context, image protocol.ImageSpec) error
	// Ready reports whether Prepare has completed for the image.
	Ready(ctx context.Context, image protocol.ImageSpec) bool
	// Start creates and boots an isolated machine from a prepared image.
	Start(ctx context.Context, assignmentID string, image protocol.ImageSpec, opts StartOptions) (Handle, error)
	// Exec runs the command in the machine and returns its exit code once it exits.
	Exec(ctx context.Context, h Handle, cmd Command) (int, error)
	// Stop shuts the machine down and deletes it.
	Stop(ctx context.Context, h Handle) error
	// List returns machines this agent owns, for crash recovery.
	List(ctx context.Context) ([]Handle, error)
	// NetworkIsolationAvailable reports whether IsolateNetwork can be honoured.
	NetworkIsolationAvailable() bool
	// MaxVMs is the platform's own limit on concurrent machines (0 for none).
	MaxVMs(image protocol.ImageSpec) int
	// GuestPlatforms are the ImageSpec platforms this executor can run on this host.
	GuestPlatforms() []string
}
