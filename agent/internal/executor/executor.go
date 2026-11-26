// Package executor is the agent's interface to whatever runs work: a virtual machine made for
// it, or a process on the host itself. Adding a platform is adding an implementation; the agent
// core does not know about Tart.
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
	// KeptDirs are directories that outlive the work, for an executor that runs work on the
	// host itself. A VM has nothing of the kind.
	KeptDirs []KeptDir
}

// KeptDir is a directory work finds where it left it last time: checkouts and build output
// that make the next run faster. Whoever shares a Key shares what is in it, so a Key is as
// narrow as the trust between the pieces of work that use it.
type KeptDir struct {
	// Link is where the work finds the directory, relative to its own.
	Link string
	// Key names whose directory it is. Two pieces of work with one Key never have the same
	// directory at the same time; the second gets another.
	Key string
	// Wipe are entries deleted from it before and after each piece of work.
	Wipe []string
}

// Command is the workload's entry command. Agents build it from a fixed template per workload
// kind; the control plane never supplies a command.
type Command struct {
	Script string
	// Env is extra environment for the command, as KEY=value. Only an executor that runs work
	// on the host itself sets it; a guest's environment is its image's business.
	Env []string
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

// LateShares is an executor whose guests see what the host puts in a shared directory after
// the machine has booted. Shares are fixed when a machine boots, so on such an executor a
// machine can be booted before anyone knows whose work it will run, given an empty directory
// to share, and handed what one job needs only once there is a job: its cache, and what else
// the agent fills in for it.
type LateShares interface {
	LateShares(image protocol.ImageSpec) bool
}

// BaseVersions is an executor that can say which version of an image a new machine would be
// made from. A machine booted from an older version is not given work once there is a newer.
type BaseVersions interface {
	BaseVersion(ctx context.Context, image protocol.ImageSpec) string
}
