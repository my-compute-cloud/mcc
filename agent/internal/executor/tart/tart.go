// Package tart runs job VMs with Tart (https://tart.run). The host owner installs Tart; MCC
// neither bundles nor redistributes it.
//
// Images are OCI images. Each is cloned once to a local base VM ("mcc-base-<hash>"); each job VM
// ("mcc-<assignment>") is a copy-on-write clone of that base, deleted when the job ends. Commands
// run through the Tart guest agent (`tart exec`), so no SSH credentials are needed.
package tart

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

const (
	jobPrefix  = "mcc-"
	basePrefix = "mcc-base-"
)

type Tart struct {
	Bin string
	Log *slog.Logger
	// BootTimeout bounds waiting for the guest agent after boot.
	BootTimeout time.Duration

	mu      sync.Mutex
	running map[string]*exec.Cmd // VM name -> `tart run` process
	prepMu  sync.Mutex           // serialises image pulls
}

func New(log *slog.Logger) *Tart {
	bin, err := exec.LookPath("tart")
	if err != nil {
		bin = "tart"
	}
	return &Tart{Bin: bin, Log: log, BootTimeout: 5 * time.Minute, running: map[string]*exec.Cmd{}}
}

// BaseName is the local VM an image is cloned to before jobs clone from it.
func BaseName(source string) string {
	h := sha256.Sum256([]byte(source))
	return basePrefix + hex.EncodeToString(h[:])[:12]
}

// VMName is the job VM for an assignment.
func VMName(assignmentID string) string {
	return jobPrefix + assignmentID
}

func (t *Tart) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, t.Bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("tart %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

type vmInfo struct {
	Name    string `json:"Name"`
	Source  string `json:"Source"`
	Running bool   `json:"Running"`
}

func (t *Tart) list(ctx context.Context) ([]vmInfo, error) {
	out, err := t.run(ctx, "list", "--source", "local", "--format", "json")
	if err != nil {
		return nil, err
	}
	var vms []vmInfo
	if err := json.Unmarshal([]byte(out), &vms); err != nil {
		return nil, fmt.Errorf("parsing tart list: %w", err)
	}
	return vms, nil
}

func (t *Tart) exists(ctx context.Context, name string) bool {
	vms, err := t.list(ctx)
	if err != nil {
		return false
	}
	for _, v := range vms {
		if v.Name == name {
			return true
		}
	}
	return false
}

func (t *Tart) Ready(ctx context.Context, image protocol.ImageSpec) bool {
	return t.exists(ctx, BaseName(image.Source))
}

func (t *Tart) Prepare(ctx context.Context, image protocol.ImageSpec) error {
	t.prepMu.Lock()
	defer t.prepMu.Unlock()
	base := BaseName(image.Source)
	if t.exists(ctx, base) {
		return nil
	}
	t.Log.Info("preparing image", "image", image.Name, "source", image.Source)
	if _, err := t.run(ctx, "pull", image.Source); err != nil {
		return err
	}
	_, err := t.run(ctx, "clone", image.Source, base)
	return err
}

// Refresh pulls the image again and, if the tag now points somewhere new, replaces the base.
// Running job VMs are independent clones and are unaffected.
func (t *Tart) Refresh(ctx context.Context, image protocol.ImageSpec) error {
	t.prepMu.Lock()
	defer t.prepMu.Unlock()
	if _, err := t.run(ctx, "pull", image.Source); err != nil {
		return err
	}
	base := BaseName(image.Source)
	next := base + "-next"
	_, _ = t.run(ctx, "delete", next)
	if _, err := t.run(ctx, "clone", image.Source, next); err != nil {
		return err
	}
	if t.exists(ctx, base) {
		if _, err := t.run(ctx, "delete", base); err != nil {
			return err
		}
	}
	_, err := t.run(ctx, "rename", next, base)
	return err
}

func (t *Tart) Start(ctx context.Context, assignmentID string, image protocol.ImageSpec, opts executor.StartOptions) (executor.Handle, error) {
	h := executor.Handle{AssignmentID: assignmentID, Name: VMName(assignmentID)}
	base := BaseName(image.Source)
	if !t.exists(ctx, base) {
		return h, fmt.Errorf("image %s is not prepared", image.Name)
	}
	if _, err := t.run(ctx, "clone", base, h.Name); err != nil {
		return h, err
	}
	set := []string{"set", h.Name}
	if opts.Resources.CPUs > 0 {
		set = append(set, "--cpu", fmt.Sprint(opts.Resources.CPUs))
	}
	if opts.Resources.MemoryMB > 0 {
		set = append(set, "--memory", fmt.Sprint(opts.Resources.MemoryMB))
	}
	// A fresh MAC avoids DHCP lease clashes between clones of one base.
	set = append(set, "--random-mac")
	if _, err := t.run(ctx, set...); err != nil {
		_ = t.Stop(context.Background(), h)
		return h, err
	}

	args := []string{"run", h.Name, "--no-graphics"}
	if opts.IsolateNetwork {
		args = append(args, "--net-softnet")
	}
	names := make([]string, 0, len(opts.SharedDirs))
	for n := range opts.SharedDirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		args = append(args, fmt.Sprintf("--dir=%s:%s:ro", n, opts.SharedDirs[n]))
	}
	// Not tied to ctx: the VM lives until Stop, whatever happens to the caller.
	cmd := exec.Command(t.Bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = t.Stop(context.Background(), h)
		return h, fmt.Errorf("tart run: %w", err)
	}
	t.mu.Lock()
	t.running[h.Name] = cmd
	t.mu.Unlock()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	deadline := time.Now().Add(t.BootTimeout)
	for {
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := t.run(probe, "exec", h.Name, "true")
		cancel()
		if err == nil {
			return h, nil
		}
		select {
		case <-exited:
			_ = t.Stop(context.Background(), h)
			return h, fmt.Errorf("vm exited during boot: %s", strings.TrimSpace(stderr.String()))
		case <-ctx.Done():
			_ = t.Stop(context.Background(), h)
			return h, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if time.Now().After(deadline) {
			_ = t.Stop(context.Background(), h)
			return h, errors.New("guest agent did not respond; does the image include the Tart guest agent?")
		}
	}
}

func (t *Tart) Exec(ctx context.Context, h executor.Handle, c executor.Command) (int, error) {
	args := []string{"exec"}
	if c.Stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, h.Name, "/bin/bash", "-c", c.Script)
	cmd := exec.CommandContext(ctx, t.Bin, args...)
	cmd.Stdin = c.Stdin
	out := c.Output
	if out == nil {
		out = io.Discard
	}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func (t *Tart) Stop(ctx context.Context, h executor.Handle) error {
	_, _ = t.run(ctx, "stop", h.Name, "--timeout", "15")
	t.mu.Lock()
	cmd := t.running[h.Name]
	delete(t.running, h.Name)
	t.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if !t.exists(ctx, h.Name) {
		return nil
	}
	_, err := t.run(ctx, "delete", h.Name)
	return err
}

func (t *Tart) List(ctx context.Context) ([]executor.Handle, error) {
	vms, err := t.list(ctx)
	if err != nil {
		return nil, err
	}
	var out []executor.Handle
	for _, v := range vms {
		if strings.HasPrefix(v.Name, jobPrefix) && !strings.HasPrefix(v.Name, basePrefix) {
			out = append(out, executor.Handle{AssignmentID: strings.TrimPrefix(v.Name, jobPrefix), Name: v.Name})
		}
	}
	return out, nil
}

// NetworkIsolationAvailable checks that Softnet can run without a password prompt: Tart needs
// the softnet binary to be setuid root (see https://tart.run/faq/ for setup).
func (t *Tart) NetworkIsolationAvailable() bool {
	p, err := exec.LookPath("softnet")
	if err != nil {
		// Tart also looks next to its own binary.
		p = filepath.Join(filepath.Dir(t.Bin), "softnet")
	}
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0 && info.Mode()&os.ModeSetuid != 0
}

// MaxVMs: Apple's licence and the Virtualization framework allow two macOS guests per host.
func (t *Tart) MaxVMs(image protocol.ImageSpec) int {
	if image.Platform == "darwin" || image.Platform == "" {
		return 2
	}
	return 0
}
