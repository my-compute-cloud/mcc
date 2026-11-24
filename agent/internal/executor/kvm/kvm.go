// Package kvm runs job VMs on Linux hosts with QEMU and KVM. The host owner installs QEMU; MCC
// neither bundles nor redistributes it.
//
// Images are OCI artifacts holding a qcow2 disk (oci.go). Each is fetched once to a local base
// ("mcc-base-<hash>"); each job VM ("mcc-<assignment>") boots from a copy-on-write overlay of
// that base, deleted when the job ends.
//
// There is no guest agent. Every VM gets a new SSH key, handed to it as a systemd credential in
// an SMBIOS string, which only the host can set; the image lets that key log in as the job's
// user. The guest's SSH port is forwarded to a port on the host's loopback address, and
// commands run through the host's ssh.
//
// Directories are shared two ways (shares.go): a writable one through virtiofs, and a
// read-only one as a read-only disk made from the directory, which the guest cannot write to
// whatever it does.
package kvm

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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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

// Tools are the host programs the executor runs.
type Tools struct {
	QEMU, QEMUImg, Virtiofsd, SSH, SSHKeygen, Mke2fs, Debugfs string
}

type KVM struct {
	// Dir holds everything the executor keeps: images/, vms/ and shares/.
	Dir string
	Log *slog.Logger
	// BootTimeout bounds waiting for a VM to accept the host's key after boot.
	BootTimeout time.Duration
	// Preload lists container images to pull into every base.
	Preload []string
	// Arch is the host's and so the guests' architecture, as Go names it: amd64 or arm64.
	Arch  string
	Tools Tools
	HTTP  *http.Client
	// control sends commands to a VM's QEMU through its control socket (tests replace it).
	control func(socket string, commands ...string) error

	mu      sync.Mutex
	running map[string]*machine // VM name -> its processes
	prepMu  sync.Mutex          // serialises image pulls
	baseMu  sync.RWMutex        // held for reading while a VM is being started from a base
	shareMu sync.Mutex          // serialises making read-only share disks
}

// machine is a booted VM: its QEMU, the helpers serving its shares, and how to reach it.
type machine struct {
	dir     string
	user    string
	port    int
	qemu    *exec.Cmd
	exited  chan struct{}
	helpers []*helper
}

// helper is a process that serves a VM, such as a virtiofsd.
type helper struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func New(dir string, log *slog.Logger) *KVM {
	arch := runtime.GOARCH
	qemu := "qemu-system-x86_64"
	if arch == "arm64" {
		qemu = "qemu-system-aarch64"
	}
	return &KVM{
		Dir:         dir,
		Log:         log,
		BootTimeout: 5 * time.Minute,
		Arch:        arch,
		Tools: Tools{
			QEMU:      find(qemu),
			QEMUImg:   find("qemu-img"),
			Virtiofsd: find("virtiofsd", "/usr/libexec/virtiofsd", "/usr/lib/qemu/virtiofsd", "/usr/lib/virtiofsd"),
			SSH:       find("ssh"),
			SSHKeygen: find("ssh-keygen"),
			Mke2fs:    find("mke2fs", "/usr/sbin/mke2fs", "/sbin/mke2fs"),
			Debugfs:   find("debugfs", "/usr/sbin/debugfs", "/sbin/debugfs"),
		},
		HTTP:    http.DefaultClient,
		control: qmp,
		running: map[string]*machine{},
	}
}

// find is a program on PATH, or at one of the places distributions put it outside PATH.
func find(name string, elsewhere ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range elsewhere {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return name
}

// BaseName is the local base an image is kept as, when nothing is added to it on this host.
func BaseName(source string) string {
	h := sha256.Sum256([]byte(source))
	return basePrefix + hex.EncodeToString(h[:])[:12]
}

// VMName is the job VM for an assignment.
func VMName(assignmentID string) string {
	return jobPrefix + assignmentID
}

// base is the image's base on this host. Its name also covers the container images this host
// adds, so a change to that list makes a new base.
func (k *KVM) base(image protocol.ImageSpec) string {
	name := BaseName(image.Source)
	if k.preloads() {
		name += "-" + executor.PreloadKey(k.Preload)
	}
	return name
}

func (k *KVM) preloads() bool        { return len(k.Preload) > 0 }
func (k *KVM) imagesDir() string     { return filepath.Join(k.Dir, "images") }
func (k *KVM) vmDir(n string) string { return filepath.Join(k.Dir, "vms", n) }

// baseInfo is what is recorded beside a base's disk.
type baseInfo struct {
	Source string      `json:"source"`
	Digest string      `json:"digest"`
	Config ImageConfig `json:"config"`
	// DiskBytes is the disk's size as the guest sees it.
	DiskBytes int64 `json:"disk_bytes"`
}

func (k *KVM) baseInfo(base string) (baseInfo, error) {
	var info baseInfo
	b, err := os.ReadFile(filepath.Join(k.imagesDir(), base, "image.json"))
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}

func (k *KVM) run(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (k *KVM) Ready(_ context.Context, image protocol.ImageSpec) bool {
	_, err := k.baseInfo(k.base(image))
	return err == nil
}

func (k *KVM) Prepare(ctx context.Context, image protocol.ImageSpec) error {
	k.prepMu.Lock()
	defer k.prepMu.Unlock()
	if k.Ready(ctx, image) {
		return nil
	}
	k.Log.Info("preparing image", "image", image.Name, "source", image.Source)
	src, err := k.resolve(ctx, image.Source)
	if err != nil {
		return fmt.Errorf("%s: %w", image.Source, err)
	}
	return k.build(ctx, image, src)
}

// Refresh looks the image up again and, if the tag now points somewhere new, replaces the base.
// A running job VM keeps the base it started from open, so it is unaffected.
func (k *KVM) Refresh(ctx context.Context, image protocol.ImageSpec) error {
	k.prepMu.Lock()
	defer k.prepMu.Unlock()
	src, err := k.resolve(ctx, image.Source)
	if err != nil {
		return fmt.Errorf("%s: %w", image.Source, err)
	}
	// Preloaded container images are tags too, and may have moved although the image has not.
	if info, err := k.baseInfo(k.base(image)); err == nil && info.Digest == src.Digest && !k.preloads() {
		return nil
	}
	return k.build(ctx, image, src)
}

// source is an image found, and how to fetch its disk.
type source struct {
	resolved
	fetch func(ctx context.Context, to string) error
}

// resolve finds an image in its registry. A source that is a directory on this host, such as
// one the owner built here, is used as it is.
func (k *KVM) resolve(ctx context.Context, src string) (source, error) {
	if filepath.IsAbs(src) {
		// What the executor keeps is not an image: a job's VM there holds that job's work.
		if rel, err := filepath.Rel(filepath.Clean(k.Dir), filepath.Clean(src)); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return source{}, fmt.Errorf("an image cannot be inside the agent's own directory")
		}
		r, err := localImage(src)
		if err != nil {
			return source{}, err
		}
		if err := r.Config.validate(k.Arch); err != nil {
			return source{}, err
		}
		return source{r, func(_ context.Context, to string) error { return copyFile(filepath.Join(src, "disk.qcow2"), to) }}, nil
	}
	ref, err := parseReference(src)
	if err != nil {
		return source{}, err
	}
	reg := &registry{client: k.HTTP, ref: ref}
	r, err := reg.resolve(ctx, k.Arch)
	if err != nil {
		return source{}, err
	}
	return source{r, func(ctx context.Context, to string) error { return reg.download(ctx, r.Layers, to) }}, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// build makes the base: the image's disk, plus whatever this host adds. It is put together
// under another name and swapped in whole, so a failure leaves the old base in place.
func (k *KVM) build(ctx context.Context, image protocol.ImageSpec, src source) error {
	base := k.base(image)
	next := filepath.Join(k.imagesDir(), base+".next")
	_ = os.RemoveAll(next)
	if err := os.MkdirAll(next, 0o700); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = os.RemoveAll(next)
		return err
	}
	disk := filepath.Join(next, "disk.qcow2")
	if err := src.fetch(ctx, disk); err != nil {
		return fail(err)
	}
	size, err := k.inspect(ctx, disk)
	if err != nil {
		return fail(fmt.Errorf("%s: %w", image.Source, err))
	}
	info := baseInfo{Source: image.Source, Digest: src.Digest, Config: src.Config, DiskBytes: size}
	if k.preloads() {
		if err := k.preloadImages(ctx, base+".next", disk, info); err != nil {
			return fail(fmt.Errorf("adding container images to %s: %w", image.Name, err))
		}
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(filepath.Join(next, "image.json"), b, 0o600); err != nil {
		return fail(err)
	}

	k.baseMu.Lock()
	defer k.baseMu.Unlock()
	dir := filepath.Join(k.imagesDir(), base)
	if err := os.RemoveAll(dir); err != nil {
		return fail(err)
	}
	if err := os.Rename(next, dir); err != nil {
		return fail(err)
	}
	k.pruneBases(image.Source, base)
	return nil
}

// inspect checks a downloaded disk and returns its size as the guest sees it. A qcow2 file can
// name other files on the host as its backing or data file, and QEMU would then read them for
// the guest; an image that does is refused.
func (k *KVM) inspect(ctx context.Context, disk string) (int64, error) {
	out, err := k.run(ctx, k.Tools.QEMUImg, "info", "-f", "qcow2", "--output=json", disk)
	if err != nil {
		return 0, fmt.Errorf("the image's disk is not a qcow2 disk: %w", err)
	}
	var info struct {
		VirtualSize int64  `json:"virtual-size"`
		Backing     string `json:"backing-filename"`
		FullBacking string `json:"full-backing-filename"`
		Specific    struct {
			Data struct {
				DataFile string `json:"data-file"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return 0, fmt.Errorf("reading qemu-img info: %w", err)
	}
	if info.Backing != "" || info.FullBacking != "" || info.Specific.Data.DataFile != "" {
		return 0, errors.New("the image's disk refers to another file, which is not allowed")
	}
	if info.VirtualSize <= 0 {
		return 0, errors.New("the image's disk has no size")
	}
	return info.VirtualSize, nil
}

// pruneBases deletes this image's other bases: ones made before the owner changed what the
// host adds to it. Each is gigabytes.
func (k *KVM) pruneBases(source, keep string) {
	entries, _ := os.ReadDir(k.imagesDir())
	prefix := BaseName(source)
	for _, e := range entries {
		n := e.Name()
		if n != keep && (n == prefix || strings.HasPrefix(n, prefix+"-") || strings.HasPrefix(n, prefix+".")) {
			if err := os.RemoveAll(filepath.Join(k.imagesDir(), n)); err != nil {
				k.Log.Warn("deleting an outdated base failed", "base", n, "err", err)
			}
		}
	}
}

// bootSpec is a VM to boot.
type bootSpec struct {
	name string
	// disk is the qcow2 file the VM runs from: an overlay for a job.
	disk string
	// disposable says the disk is deleted when the VM stops, so writes need not be made safe
	// against the host losing power.
	disposable bool
	config     ImageConfig
	resources  executor.Resources
	shares     map[string]executor.SharedDir
}

// boot starts a VM and waits until the host's key can log in. The VM is not tied to ctx: it
// lives until halted, whatever happens to the caller.
func (k *KVM) boot(ctx context.Context, s bootSpec) error {
	dir := k.vmDir(s.name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	m := &machine{dir: dir, user: s.config.User, exited: make(chan struct{})}
	k.mu.Lock()
	k.running[s.name] = m
	k.mu.Unlock()

	key := filepath.Join(dir, "key")
	_ = os.Remove(key)
	_ = os.Remove(key + ".pub")
	if _, err := k.run(ctx, k.Tools.SSHKeygen, "-q", "-t", "ed25519", "-N", "", "-C", "mcc", "-f", key); err != nil {
		return err
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}
	if m.port, err = freePort(); err != nil {
		return err
	}
	shares, err := k.startShares(ctx, m, s)
	if err != nil {
		return err
	}
	args, err := k.qemuArgs(s, m, strings.TrimSpace(string(pub)), shares)
	if err != nil {
		return err
	}

	cmd := exec.Command(k.Tools.QEMU, args...)
	logf, err := os.OpenFile(filepath.Join(dir, "qemu.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	err = cmd.Start()
	logf.Close()
	if err != nil {
		return fmt.Errorf("starting qemu: %w", err)
	}
	m.qemu = cmd
	go func() {
		_ = cmd.Wait()
		close(m.exited)
	}()

	deadline := time.Now().Add(k.BootTimeout)
	for {
		probe, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := k.run(probe, k.Tools.SSH, append(k.sshArgs(m, 5), "true")...)
		cancel()
		if err == nil {
			break
		}
		select {
		case <-m.exited:
			return fmt.Errorf("the VM exited during boot: %s", executor.LastLines(readFile(filepath.Join(dir, "qemu.log")), 3))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if time.Now().After(deadline) {
			return errors.New("the VM did not accept the host's SSH key; was the image built for Linux hosts?")
		}
	}
	if len(shares) == 0 {
		return nil
	}
	var out bytes.Buffer
	code, err := k.Exec(ctx, executor.Handle{Name: s.name}, executor.Command{Script: mountScript(shares), Output: &out})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("mounting the host's shared directories in the VM failed: %s", executor.LastLines(out.String(), 3))
	}
	return nil
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// freePort is a TCP port on the loopback address that nothing listens on at the moment.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// sshArgs reach a VM: only with its own key, and with nothing taken from the configuration or
// the agent of whoever runs mcc. The host key is not checked, because the connection goes to
// this host's loopback address and from there straight into the VM that QEMU just started.
func (k *KVM) sshArgs(m *machine, connectTimeout int) []string {
	return []string{
		"-F", "none",
		"-i", filepath.Join(m.dir, "key"),
		"-p", fmt.Sprint(m.port),
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", fmt.Sprintf("ConnectTimeout=%d", connectTimeout),
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=6",
		m.user + "@127.0.0.1",
	}
}

// halt stops a VM's processes. A disposable VM is simply ended; otherwise the guest is asked to
// shut down and given the seconds to do it, and halt reports whether it did.
func (k *KVM) halt(name string, graceSeconds int) bool {
	k.mu.Lock()
	m := k.running[name]
	delete(k.running, name)
	k.mu.Unlock()
	dir := k.vmDir(name)
	sock := filepath.Join(dir, "qmp.sock")
	clean := false

	if m == nil || m.qemu == nil {
		// Not started by this process: left by an agent that crashed. QEMU's control socket
		// reaches exactly that VM, if it is still there.
		_ = k.control(sock, "quit")
		killStale(filepath.Join(dir, "qemu.pid"), dir)
	} else {
		if graceSeconds > 0 && k.control(sock, "system_powerdown") == nil {
			select {
			case <-m.exited:
				clean = true
			case <-time.After(time.Duration(graceSeconds) * time.Second):
			}
		}
		if !clean {
			wait := 5 * time.Second
			if k.control(sock, "quit") != nil {
				wait = 0
			}
			select {
			case <-m.exited:
			case <-time.After(wait):
				_ = m.qemu.Process.Kill()
				<-m.exited
			}
		}
	}
	if m != nil {
		// A virtiofsd ends by itself when its QEMU has gone.
		for _, h := range m.helpers {
			select {
			case <-h.done:
			case <-time.After(2 * time.Second):
				_ = h.cmd.Process.Kill()
				<-h.done
			}
		}
	}
	return clean
}

// killStale ends a QEMU left by an earlier agent, named by its pid file. The pid is only
// trusted where the process can be seen to be that VM's.
func killStale(pidFile, vmDir string) {
	var pid int
	if _, err := fmt.Sscan(readFile(pidFile), &pid); err != nil || pid <= 1 {
		return
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !bytes.Contains(cmdline, []byte(vmDir)) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var assignmentName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func (k *KVM) Start(ctx context.Context, assignmentID string, image protocol.ImageSpec, opts executor.StartOptions) (executor.Handle, error) {
	h := executor.Handle{AssignmentID: assignmentID, Name: VMName(assignmentID)}
	// The name becomes a directory.
	if !assignmentName.MatchString(assignmentID) {
		return h, fmt.Errorf("assignment id %q cannot name a VM", assignmentID)
	}
	if opts.IsolateNetwork {
		return h, errors.New("network isolation is not available on this host")
	}
	base := k.base(image)

	k.baseMu.RLock()
	info, err := k.baseInfo(base)
	if err != nil {
		k.baseMu.RUnlock()
		return h, fmt.Errorf("image %s is not prepared", image.Name)
	}
	dir := k.vmDir(h.Name)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		k.baseMu.RUnlock()
		return h, err
	}
	overlay := filepath.Join(dir, "disk.qcow2")
	create := []string{"create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", filepath.Join(k.imagesDir(), base, "disk.qcow2"), overlay}
	// The guest grows its volume to fill the disk when it boots. A disk cannot shrink.
	if want := int64(opts.Resources.DiskGB) << 30; want > info.DiskBytes {
		create = append(create, fmt.Sprintf("%dG", opts.Resources.DiskGB))
	}
	_, err = k.run(ctx, k.Tools.QEMUImg, create...)
	if err == nil {
		err = k.boot(ctx, bootSpec{
			name:       h.Name,
			disk:       overlay,
			disposable: true,
			config:     info.Config,
			resources:  opts.Resources,
			shares:     opts.SharedDirs,
		})
	}
	// QEMU has the base open by now, so the base may be replaced.
	k.baseMu.RUnlock()
	if err != nil {
		_ = k.Stop(context.Background(), h)
		return h, err
	}
	return h, nil
}

func (k *KVM) Exec(ctx context.Context, h executor.Handle, c executor.Command) (int, error) {
	k.mu.Lock()
	m := k.running[h.Name]
	k.mu.Unlock()
	if m == nil {
		return -1, fmt.Errorf("VM %s is not running", h.Name)
	}
	// ssh hands the command to the user's shell as one string.
	cmd := exec.CommandContext(ctx, k.Tools.SSH, append(k.sshArgs(m, 20), "exec /bin/bash -c "+shellQuote(c.Script))...)
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
		// ssh's own failures, as opposed to the command's, are 255.
		if ee.ExitCode() == 255 {
			return -1, errors.New("the connection to the VM was lost")
		}
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (k *KVM) Stop(_ context.Context, h executor.Handle) error {
	if !strings.HasPrefix(h.Name, jobPrefix) || strings.ContainsAny(h.Name, "/\\") {
		return fmt.Errorf("%q is not a job VM", h.Name)
	}
	k.halt(h.Name, 0)
	return os.RemoveAll(k.vmDir(h.Name))
}

func (k *KVM) List(_ context.Context) ([]executor.Handle, error) {
	entries, err := os.ReadDir(filepath.Join(k.Dir, "vms"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []executor.Handle
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() && strings.HasPrefix(n, jobPrefix) && !strings.HasPrefix(n, basePrefix) {
			out = append(out, executor.Handle{AssignmentID: strings.TrimPrefix(n, jobPrefix), Name: n})
		}
	}
	return out, nil
}

// NetworkIsolationAvailable: not yet. A VM reaches the internet through QEMU's user-mode
// network, which also lets it reach the host's local network and the host itself.
func (k *KVM) NetworkIsolationAvailable() bool { return false }

// NetworkIsolationMissing says what the lack of isolation means on this host.
func (k *KVM) NetworkIsolationMissing() string {
	return "a job on a Linux host can reach the host's local network and services listening on the host's loopback address. Run only jobs you trust. See docs/agent.md."
}

// GuestPlatforms: Linux guests of the host's architecture.
func (k *KVM) GuestPlatforms() []string { return []string{"linux"} }

// MaxVMs: nothing but the owner's policy limits Linux guests.
func (k *KVM) MaxVMs(protocol.ImageSpec) int { return 0 }

// Preflight lists what this host lacks to run VMs, in words for its owner.
func (k *KVM) Preflight() []string {
	var problems []string
	need := func(bin, what string) {
		if _, err := exec.LookPath(bin); err != nil {
			problems = append(problems, what)
		}
	}
	need(k.Tools.QEMU, filepath.Base(k.Tools.QEMU)+" is not installed")
	need(k.Tools.QEMUImg, "qemu-img is not installed")
	need(k.Tools.SSH, "ssh is not installed")
	need(k.Tools.SSHKeygen, "ssh-keygen is not installed")
	need(k.Tools.Mke2fs, "mke2fs is not installed (package e2fsprogs); it makes the disk the Actions runner is shared on")
	need(k.Tools.Debugfs, "debugfs is not installed (package e2fsprogs); it makes the disk the Actions runner is shared on")
	need(k.Tools.Virtiofsd, "virtiofsd is not installed; jobs cannot have a cache without it")
	need("newuidmap", "newuidmap is not installed (package uidmap); jobs cannot have a cache without it")
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		problems = append(problems, "/dev/kvm cannot be opened: "+err.Error()+" (is the user in the kvm group?)")
	} else {
		f.Close()
	}
	if _, err := k.firmware(); err != nil {
		problems = append(problems, err.Error())
	}
	return problems
}
