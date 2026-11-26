// Package tart runs job VMs with Tart (https://tart.run). The host owner installs Tart; MCC
// neither bundles nor redistributes it.
//
// Images are OCI images. Each is cloned once to a local base VM ("mcc-base-<hash>"); each job VM
// ("mcc-<assignment>") is a copy-on-write clone of that base, deleted when the job ends. Commands
// run through the Tart guest agent (`tart exec`), so no SSH credentials are needed.
//
// The owner can have the host add to a base before jobs clone it: its own Xcode for macOS
// images (xcode.go), so jobs get Xcode without anyone distributing an image that contains it,
// and container images for Linux images (preload.go), so jobs start containers without pulling.
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
	"regexp"
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
	// Dir is where the agent writes down which VMs it made. Tart's VMs are the whole account's,
	// not the agent's: the owner's own VMs sit beside the agent's, and may be named anything.
	// So a VM is the agent's to stop and delete only if it is written down here, never because
	// of its name. Empty keeps the record in memory, which forgets when the agent stops.
	Dir string
	// BootTimeout bounds waiting for the guest agent after boot.
	BootTimeout time.Duration
	// Xcode is the host's Xcode.app to copy into every macOS base, or "" for none.
	Xcode string
	// Preload lists container images to pull into every Linux base.
	Preload []string

	mu      sync.Mutex
	builds  map[string]int       // base -> how many times this agent has built it
	running map[string]*exec.Cmd // VM name -> `tart run` process
	made    map[string]bool      // job VMs this agent made, when there is no Dir to keep them in
	prepMu  sync.Mutex           // serialises image pulls
	built   map[string]string    // base name -> the image digest it was built from
	xcode   xcodeInfo            // the host Xcode's version, read at most once a minute
}

func New(log *slog.Logger) *Tart {
	bin, err := exec.LookPath("tart")
	if err != nil {
		bin = "tart"
	}
	return &Tart{Bin: bin, Log: log, BootTimeout: 5 * time.Minute, running: map[string]*exec.Cmd{}, builds: map[string]int{}, made: map[string]bool{}, built: map[string]string{}}
}

// BaseName is the local VM an image is cloned to before jobs clone from it, when nothing is
// added to the image on this host.
func BaseName(source string) string {
	h := sha256.Sum256([]byte(source))
	return basePrefix + hex.EncodeToString(h[:])[:12]
}

// base is the local VM jobs clone from. Its name also covers what this host adds to the image,
// so that a change to that (another Xcode, or none) makes a new base rather than reusing one
// with the wrong contents.
func (t *Tart) base(ctx context.Context, image protocol.ImageSpec) (string, error) {
	// A job VM or a base is not an image, whoever asks: one holds another job's work, and the
	// name of one not yet made would be cloned the moment it appears.
	if strings.HasPrefix(strings.ToLower(image.Source), jobPrefix) {
		return "", fmt.Errorf("image source %s names one of the agent's own VMs", image.Source)
	}
	name := BaseName(image.Source)
	if t.preloads(image) {
		return name + "-" + executor.PreloadKey(t.Preload), nil
	}
	if !t.addsXcode(image) {
		return name, nil
	}
	x, err := t.hostXcode(ctx)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(t.Xcode + "\n" + x.build))
	return name + "-x" + hex.EncodeToString(h[:])[:8], nil
}

// VMName is the job VM for an assignment.
func VMName(assignmentID string) string {
	return jobPrefix + assignmentID
}

// jobName keeps a name from being anything but one file name of ours.
var jobName = regexp.MustCompile(`^mcc-[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)

func (t *Tart) recordPath(name string) string { return filepath.Join(t.Dir, "jobs", name) }

// record writes down that a job VM is about to be made, before it is.
func (t *Tart) record(name string) error {
	if !jobName.MatchString(name) {
		return fmt.Errorf("%q cannot name a job's VM", name)
	}
	if t.Dir == "" {
		t.mu.Lock()
		t.made[name] = true
		t.mu.Unlock()
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(t.recordPath(name)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(t.recordPath(name), nil, 0o600)
}

func (t *Tart) forget(name string) {
	if t.Dir == "" {
		t.mu.Lock()
		delete(t.made, name)
		t.mu.Unlock()
		return
	}
	_ = os.Remove(t.recordPath(name))
}

// recorded lists the job VMs this agent has written down as its own.
func (t *Tart) recorded() []string {
	var out []string
	if t.Dir == "" {
		t.mu.Lock()
		defer t.mu.Unlock()
		for name := range t.made {
			out = append(out, name)
		}
		return out
	}
	entries, _ := os.ReadDir(filepath.Join(t.Dir, "jobs"))
	for _, e := range entries {
		if !e.IsDir() && jobName.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func (t *Tart) isRecorded(name string) bool {
	for _, n := range t.recorded() {
		if n == name {
			return true
		}
	}
	return false
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
	base, err := t.base(ctx, image)
	return err == nil && t.exists(ctx, base)
}

func (t *Tart) Prepare(ctx context.Context, image protocol.ImageSpec) error {
	t.prepMu.Lock()
	defer t.prepMu.Unlock()
	base, err := t.base(ctx, image)
	if err != nil {
		return err
	}
	if t.exists(ctx, base) {
		return nil
	}
	t.Log.Info("preparing image", "image", image.Name, "source", image.Source)
	if err := t.pull(ctx, image.Source); err != nil {
		return err
	}
	return t.build(ctx, image, base)
}

// Refresh pulls the image again and, if the tag now points somewhere new, replaces the base.
// Running job VMs are independent clones and are unaffected.
func (t *Tart) Refresh(ctx context.Context, image protocol.ImageSpec) error {
	t.prepMu.Lock()
	defer t.prepMu.Unlock()
	base, err := t.base(ctx, image)
	if err != nil {
		return err
	}
	if err := t.pull(ctx, image.Source); err != nil {
		return err
	}
	// Preloaded container images are tags too, and may have moved although the image has not.
	if d := t.digest(ctx, image.Source); d != "" && d == t.built[base] && t.exists(ctx, base) && !t.preloads(image) {
		return nil
	}
	return t.build(ctx, image, base)
}

// pull fetches an image from its registry. A source that names a VM already on this host, such
// as one the owner built here, is used as it is.
func (t *Tart) pull(ctx context.Context, source string) error {
	if t.exists(ctx, source) {
		return nil
	}
	_, err := t.run(ctx, "pull", source)
	return err
}

// digest is the exact image a source refers to at the moment, or "" when Tart cannot say.
func (t *Tart) digest(ctx context.Context, source string) string {
	out, err := t.run(ctx, "fqn", source)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// build makes the base from the pulled image: a clone, plus whatever this host adds. It is put
// together under another name and swapped in whole, so a failure leaves the old base in place.
func (t *Tart) build(ctx context.Context, image protocol.ImageSpec, base string) error {
	next := base + "-next"
	_, _ = t.run(ctx, "delete", next)
	if _, err := t.run(ctx, "clone", image.Source, next); err != nil {
		return err
	}
	if t.addsXcode(image) {
		if err := t.installXcode(ctx, next); err != nil {
			_, _ = t.run(context.WithoutCancel(ctx), "delete", next)
			return fmt.Errorf("adding Xcode to %s: %w", image.Name, err)
		}
	}
	if t.preloads(image) {
		if err := t.preloadImages(ctx, next); err != nil {
			_, _ = t.run(context.WithoutCancel(ctx), "delete", next)
			return fmt.Errorf("adding container images to %s: %w", image.Name, err)
		}
	}
	if t.exists(ctx, base) {
		if _, err := t.run(ctx, "delete", base); err != nil {
			return err
		}
	}
	if _, err := t.run(ctx, "rename", next, base); err != nil {
		return err
	}
	t.built[base] = t.digest(ctx, image.Source)
	t.mu.Lock()
	t.builds[base]++
	t.mu.Unlock()
	t.pruneBases(ctx, image.Source, base)
	return nil
}

// pruneBases deletes this image's other bases: ones made before the owner changed what the
// host adds to it. Each is tens of gigabytes.
func (t *Tart) pruneBases(ctx context.Context, source, keep string) {
	vms, err := t.list(ctx)
	if err != nil {
		return
	}
	prefix := BaseName(source)
	for _, v := range vms {
		if v.Name != keep && (v.Name == prefix || strings.HasPrefix(v.Name, prefix+"-")) {
			if _, err := t.run(ctx, "delete", v.Name); err != nil {
				t.Log.Warn("deleting an outdated base failed", "vm", v.Name, "err", err)
			}
		}
	}
}

// diskGB is a VM's disk size, or 0 when Tart does not report one.
func (t *Tart) diskGB(ctx context.Context, name string) int {
	out, err := t.run(ctx, "get", name, "--format", "json")
	if err != nil {
		return 0
	}
	var info struct {
		Disk int `json:"Disk"`
	}
	_ = json.Unmarshal([]byte(out), &info)
	return info.Disk
}

// boot starts a VM and waits for its guest agent. The VM is not tied to ctx: it lives until
// halted, whatever happens to the caller.
func (t *Tart) boot(ctx context.Context, name string, args ...string) error {
	cmd := exec.Command(t.Bin, append([]string{"run", name, "--no-graphics"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("tart run: %w", err)
	}
	t.mu.Lock()
	t.running[name] = cmd
	t.mu.Unlock()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	deadline := time.Now().Add(t.BootTimeout)
	for {
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := t.run(probe, "exec", name, "true")
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("vm exited during boot: %s", strings.TrimSpace(stderr.String()))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if time.Now().After(deadline) {
			return errors.New("guest agent did not respond; does the image include the Tart guest agent?")
		}
	}
}

// halt shuts a VM down, giving the guest the given seconds to do it cleanly.
func (t *Tart) halt(ctx context.Context, name string, seconds int) {
	_, _ = t.run(ctx, "stop", name, "--timeout", fmt.Sprint(seconds))
	t.mu.Lock()
	cmd := t.running[name]
	delete(t.running, name)
	t.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (t *Tart) Start(ctx context.Context, assignmentID string, image protocol.ImageSpec, opts executor.StartOptions) (executor.Handle, error) {
	h := executor.Handle{AssignmentID: assignmentID, Name: VMName(assignmentID)}
	base, err := t.base(ctx, image)
	if err != nil {
		return h, err
	}
	if !t.exists(ctx, base) {
		return h, fmt.Errorf("image %s is not prepared", image.Name)
	}
	// A VM of this name that is already there is not ours: we have not made it yet. Writing it
	// down would make it ours to delete.
	if t.exists(ctx, h.Name) && !t.isRecorded(h.Name) {
		return h, fmt.Errorf("a VM named %s already exists and was not made by this agent", h.Name)
	}
	if err := t.record(h.Name); err != nil {
		return h, err
	}
	if _, err := t.run(ctx, "clone", base, h.Name); err != nil {
		t.forget(h.Name)
		return h, err
	}
	set := []string{"set", h.Name}
	if opts.Resources.CPUs > 0 {
		set = append(set, "--cpu", fmt.Sprint(opts.Resources.CPUs))
	}
	if opts.Resources.MemoryMB > 0 {
		set = append(set, "--memory", fmt.Sprint(opts.Resources.MemoryMB))
	}
	// The guest grows its volume to fill the disk when it boots. Tart refuses to shrink one.
	if want := opts.Resources.DiskGB; want > 0 && want > t.diskGB(ctx, h.Name) {
		set = append(set, "--disk-size", fmt.Sprint(want))
	}
	// A fresh MAC avoids DHCP lease clashes between clones of one base.
	set = append(set, "--random-mac")
	if _, err := t.run(ctx, set...); err != nil {
		_ = t.Stop(context.Background(), h)
		return h, err
	}

	var args []string
	if opts.IsolateNetwork {
		args = append(args, "--net-softnet")
	}
	names := make([]string, 0, len(opts.SharedDirs))
	for n := range opts.SharedDirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		dir := fmt.Sprintf("--dir=%s:%s", n, opts.SharedDirs[n].Path)
		if !opts.SharedDirs[n].Writable {
			dir += ":ro"
		}
		args = append(args, dir)
	}
	if err := t.boot(ctx, h.Name, args...); err != nil {
		_ = t.Stop(context.Background(), h)
		return h, err
	}
	if image.Platform == "linux" && len(names) > 0 {
		if err := t.mountShares(ctx, h); err != nil {
			_ = t.Stop(context.Background(), h)
			return h, err
		}
	}
	return h, nil
}

// mountSharesScript mounts what the host shares in a Linux guest, which does not do so by
// itself as a macOS guest does. Tart puts every share under one mount, each in a directory of
// its name.
const mountSharesScript = `set -e
D="` + executor.LinuxSharesDir + `"
mountpoint -q "$D" && exit 0
sudo mkdir -p "$D"
sudo mount -t virtiofs com.apple.virtio-fs.automount "$D"
`

func (t *Tart) mountShares(ctx context.Context, h executor.Handle) error {
	var out bytes.Buffer
	code, err := t.Exec(ctx, h, executor.Command{Script: mountSharesScript, Output: &out})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("mounting the host's shared directories in the VM failed: %s", executor.LastLines(out.String(), 3))
	}
	return nil
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

// Stop halts and deletes a job's VM. It touches only a VM this agent wrote down as its own:
// asked about any other, it does nothing, whatever the VM is called.
func (t *Tart) Stop(ctx context.Context, h executor.Handle) error {
	if !t.isRecorded(h.Name) {
		return nil
	}
	t.halt(ctx, h.Name, 15)
	if t.exists(ctx, h.Name) {
		if _, err := t.run(ctx, "delete", h.Name); err != nil {
			return err
		}
	}
	t.forget(h.Name)
	return nil
}

// List returns the job VMs this agent made that are still there, for cleaning up after a
// crash. It goes by the agent's own record, not by what VMs are called: the owner of the
// machine may have VMs of their own under any name, including ones that look like ours.
func (t *Tart) List(ctx context.Context) ([]executor.Handle, error) {
	vms, err := t.list(ctx)
	if err != nil {
		return nil, err
	}
	there := map[string]bool{}
	for _, v := range vms {
		there[v.Name] = true
	}
	var out []executor.Handle
	for _, name := range t.recorded() {
		if !there[name] {
			t.forget(name) // it was deleted without us
			continue
		}
		out = append(out, executor.Handle{AssignmentID: strings.TrimPrefix(name, jobPrefix), Name: name})
	}
	return out, nil
}

// NetworkIsolationMissing says what the lack of isolation means on this host and how to get it.
func (t *Tart) NetworkIsolationMissing() string {
	return "Softnet is not set up, so VMs share the host's network. See docs/agent.md to enable Softnet."
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

// GuestPlatforms: Tart runs macOS and Linux guests on Apple Silicon.
func (t *Tart) GuestPlatforms() []string { return []string{"darwin", "linux"} }

// LateShares: a macOS guest sees at once what the host moves into or clones into a shared
// directory after boot. That was checked on macOS 26 guests; Linux guests have not been, and
// their shares are mounted by the agent, so they get theirs at boot as before.
func (t *Tart) LateShares(image protocol.ImageSpec) bool {
	return image.Platform == "darwin" || image.Platform == ""
}

// BaseVersion names the base a new VM of the image is cloned from, and how many times this
// agent has rebuilt it, which a refresh does when the image has changed.
func (t *Tart) BaseVersion(ctx context.Context, image protocol.ImageSpec) string {
	base, err := t.base(ctx, image)
	if err != nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return fmt.Sprintf("%s#%d", base, t.builds[base])
}

// MaxVMs: Apple's licence and the Virtualization framework allow two macOS guests per host.
func (t *Tart) MaxVMs(image protocol.ImageSpec) int {
	if image.Platform == "darwin" || image.Platform == "" {
		return 2
	}
	return 0
}
