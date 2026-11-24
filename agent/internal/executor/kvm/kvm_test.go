package kvm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// stubHost gives an executor fake host programs that log their arguments: a qemu and a
// virtiofsd that stay running as the real ones do, and an ssh that plays the guest.
func stubHost(t *testing.T) (*KVM, string) {
	// Socket paths have a short limit, and the usual temporary directory is long on macOS.
	dir, err := os.MkdirTemp("/tmp", "mcc-kvm-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "bin")
	logf := filepath.Join(dir, "calls")
	stubs := map[string]string{
		"qemu": `prev=; for a in "$@"; do [ "$prev" = -pidfile ] && echo $$ > "$a"; prev=$a; done
exec sleep 30`,
		"qemu-img": `case "$1" in
  info) cat "` + dir + `/diskinfo" 2>/dev/null || echo '{"virtual-size":42949672960,"format":"qcow2"}' ;;
  create) for a in "$@"; do case "$a" in */vms/*/disk.qcow2) : > "$a" ;; esac; done ;;
esac`,
		"virtiofsd": `for a in "$@"; do case "$a" in --socket-path=*) : > "${a#--socket-path=}" ;; esac; done
exec sleep 30`,
		"ssh-keygen": `prev=; for a in "$@"; do [ "$prev" = -f ] && { echo private > "$a"; echo "ssh-ed25519 AAAATESTKEY mcc" > "$a.pub"; }; prev=$a; done`,
		"debugfs":    `:`,
		"mke2fs":     `prev=; for a in "$@"; do case "$a" in *K) : > "$prev" ;; esac; prev=$a; done`,
		"ssh": `for a in "$@"; do last=$a; done
case "$last" in
  true) exit 0 ;;
  *"docker pull"*) cat > "` + dir + `/preloaded"; exit 0 ;;
  *"sudo mount"*) exit 0 ;;
esac
read -r line; echo "stdin:$line"; echo "Running job: build"; exit 3`,
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range stubs {
		script := "#!/bin/bash\necho \"" + name + " $*\" >> \"" + logf + "\"\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Firmware files as a distribution would install them.
	code, vars := filepath.Join(dir, "CODE.fd"), filepath.Join(dir, "VARS.fd")
	_ = os.WriteFile(code, []byte("code"), 0o644)
	_ = os.WriteFile(vars, []byte("vars"), 0o644)
	old := firmwares
	firmwares = [][2]string{{code, vars}}
	t.Cleanup(func() { firmwares = old })

	k := New(filepath.Join(dir, "state"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	k.Arch = "arm64"
	k.BootTimeout = 5 * time.Second
	k.Tools = Tools{
		QEMU: filepath.Join(bin, "qemu"), QEMUImg: filepath.Join(bin, "qemu-img"), Virtiofsd: filepath.Join(bin, "virtiofsd"),
		SSH: filepath.Join(bin, "ssh"), SSHKeygen: filepath.Join(bin, "ssh-keygen"), Mke2fs: filepath.Join(bin, "mke2fs"), Debugfs: filepath.Join(bin, "debugfs"),
	}
	// The fake qemu has no control socket; a command to it ends the process, as both of the
	// commands the executor sends end a real one.
	k.control = func(socket string, _ ...string) error {
		var pid int
		var err error
		// The fake may not have got as far as writing its pid yet.
		for i := 0; i < 200; i++ {
			if _, err = fmt.Sscan(readFile(filepath.Join(filepath.Dir(socket), "qemu.pid")), &pid); err == nil {
				return syscall.Kill(pid, syscall.SIGTERM)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return err
	}
	return k, logf
}

// calls are the logged invocations of one tool, without its name.
func calls(logf, tool string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(readFile(logf)), "\n") {
		if rest, ok := strings.CutPrefix(l, tool+" "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// started waits for a program the executor starts without waiting for, such as qemu, to have
// logged its invocations: it may not have run yet when Start returns.
func started(t *testing.T, logf, tool string, n int) []string {
	t.Helper()
	for i := 0; i < 200; i++ {
		if got := calls(logf, tool); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was not started %d times: %q", tool, n, calls(logf, tool))
	return nil
}

// fakeRegistry serves images the way a registry does, anonymous pull token included.
type fakeRegistry struct {
	srv   *httptest.Server
	mu    sync.Mutex
	blobs map[string][]byte // digest -> content
	tags  map[string]string // repo:tag -> manifest digest
	pulls int               // blob downloads
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newRegistry(t *testing.T) *fakeRegistry {
	r := &fakeRegistry{blobs: map[string][]byte{}, tags: map[string]string{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"anonymous"}`))
			return
		}
		if req.Header.Get("Authorization") != "Bearer anonymous" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.srv.URL+`/token",service="registry.test",scope="repository:x:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v2/"), "/")
		kind, ref, repo := parts[len(parts)-2], parts[len(parts)-1], strings.Join(parts[:len(parts)-2], "/")
		digest := ref
		if kind == "manifests" && !strings.HasPrefix(ref, "sha256:") {
			digest = r.tags[repo+":"+ref]
		}
		b, ok := r.blobs[digest]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if kind == "blobs" {
			r.pulls++
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRegistry) put(b []byte) descriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := digestOf(b)
	r.blobs[d] = b
	return descriptor{Digest: d, Size: int64(len(b))}
}

// push publishes an image under repo:tag as an index with a manifest for each architecture,
// the disk split into pieces. It returns the reference to pull it by.
func (r *fakeRegistry) push(repo, tag string, disks map[string][]string) string {
	var index manifest
	index.MediaType = mediaIndex
	for arch, pieces := range disks {
		cfg, _ := json.Marshal(ImageConfig{Architecture: arch, OS: "linux", User: "admin", UID: 1000, GID: 109})
		m := manifest{MediaType: mediaManifest, Config: r.put(cfg)}
		m.Config.MediaType = mediaConfig
		for _, p := range pieces {
			l := r.put([]byte(p))
			l.MediaType = mediaDisk
			m.Layers = append(m.Layers, l)
		}
		b, _ := json.Marshal(m)
		d := r.put(b)
		d.MediaType = mediaManifest
		d.Platform = &struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		}{arch, "linux"}
		index.Manifests = append(index.Manifests, d)
	}
	b, _ := json.Marshal(index)
	d := r.put(b)
	r.mu.Lock()
	r.tags[repo+":"+tag] = d.Digest
	r.mu.Unlock()
	return strings.TrimPrefix(r.srv.URL, "http://") + "/" + repo + ":" + tag
}

func TestLifecycleCommands(t *testing.T) {
	k, logf := stubHost(t)
	reg := newRegistry(t)
	ctx := context.Background()
	source := reg.push("acme/ubuntu-runner", "24.04", map[string][]string{
		"arm64": {"first piece, ", "second piece"},
		"amd64": {"another machine's disk"},
	})
	img := protocol.ImageSpec{Name: "ubuntu", Source: source, Platform: "linux"}
	base := BaseName(source)

	if k.Ready(ctx, img) {
		t.Fatal("ready before prepare")
	}
	if err := k.Prepare(ctx, img); err != nil {
		t.Fatal(err)
	}
	if !k.Ready(ctx, img) {
		t.Fatal("not ready after prepare")
	}
	baseDisk := filepath.Join(k.Dir, "images", base, "disk.qcow2")
	if got := readFile(baseDisk); got != "first piece, second piece" {
		t.Fatalf("the base's disk is %q: the pieces for this architecture, in order, were expected", got)
	}

	runner, cache := filepath.Join(filepath.Dir(k.Dir), "runner"), filepath.Join(filepath.Dir(k.Dir), "cache")
	for _, d := range []string{runner, cache} {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = os.WriteFile(filepath.Join(runner, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	h, err := k.Start(ctx, "a1", img, executor.StartOptions{
		Resources: executor.Resources{CPUs: 4, MemoryMB: 8192, DiskGB: 120},
		SharedDirs: map[string]executor.SharedDir{
			"mcc-runner": {Path: runner},
			"mcc-cache":  {Path: cache, Writable: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "mcc-a1" {
		t.Fatalf("name %s", h.Name)
	}
	vm := filepath.Join(k.Dir, "vms", "mcc-a1")

	var out bytes.Buffer
	code, err := k.Exec(ctx, h, executor.Command{Script: "echo 'hi'", Stdin: strings.NewReader("jit\n"), Output: &out})
	if err != nil || code != 3 {
		t.Fatalf("exec: code=%d err=%v out=%q", code, err, out.String())
	}
	if !strings.Contains(out.String(), "stdin:jit") {
		t.Fatalf("stdin was not passed to the guest: %q", out.String())
	}

	if got := calls(logf, "qemu-img"); len(got) != 2 || got[1] != "create -q -f qcow2 -F qcow2 -b "+baseDisk+" "+vm+"/disk.qcow2 120G" {
		t.Fatalf("qemu-img calls: %q", got)
	}
	if got := calls(logf, "ssh-keygen"); len(got) != 1 || got[0] != "-q -t ed25519 -N  -C mcc -f "+vm+"/key" {
		t.Fatalf("ssh-keygen calls: %q", got)
	}
	wantFS := fmt.Sprintf("--socket-path=%s/fs0.sock --shared-dir=%s --sandbox namespace --uid-map :1000:%d:1: --gid-map :109:%d:1:", vm, cache, os.Getuid(), os.Getgid())
	if got := started(t, logf, "virtiofsd", 1); len(got) != 1 || got[0] != wantFS {
		t.Fatalf("virtiofsd calls: %q", got)
	}
	mke2fs := calls(logf, "mke2fs")
	if len(mke2fs) != 1 || !strings.Contains(mke2fs[0], "-d "+runner+" "+k.Dir+"/shares/") {
		t.Fatalf("mke2fs calls: %q", mke2fs)
	}
	if got := calls(logf, "debugfs"); len(got) != 1 || !strings.HasPrefix(got[0], "-w -R rmdir lost+found "+k.Dir+"/shares/") {
		t.Fatalf("debugfs calls: %q", got)
	}
	qemu := started(t, logf, "qemu", 1)
	if len(qemu) != 1 {
		t.Fatalf("qemu calls: %q", qemu)
	}
	for _, want := range []string{
		"-name mcc-a1 ", "-nodefaults", "-cpu host -smp 4 -m 8192M",
		"-object memory-backend-memfd,id=mem,size=8192M,share=on",
		"-machine virt,accel=kvm,gic-version=host,memory-backend=mem",
		"readonly=on,file=" + filepath.Dir(k.Dir) + "/CODE.fd",
		"if=pflash,format=raw,file=" + vm + "/vars.fd",
		"-drive if=none,id=root,format=qcow2,discard=unmap,file=" + vm + "/disk.qcow2,cache=unsafe",
		"-chardev socket,id=fs0,path=" + vm + "/fs0.sock -device vhost-user-fs-pci,chardev=fs0,tag=mcc-cache",
		"-drive if=none,id=ro1,format=raw,readonly=on,file=" + k.Dir + "/shares/",
		"-device virtio-blk-pci,drive=ro1,serial=mccshare1",
		"hostfwd=tcp:127.0.0.1:",
		"-smbios type=11,value=io.systemd.credential:mcc.authorized_key=ssh-ed25519 AAAATESTKEY mcc",
		"-qmp unix:" + vm + "/qmp.sock,server=on,wait=off",
	} {
		if !strings.Contains(qemu[0], want) {
			t.Errorf("qemu was not given %q in:\n%s", want, qemu[0])
		}
	}
	if readFile(vm+"/vars.fd") != "vars" {
		t.Error("the VM did not get its own copy of the firmware's variables")
	}

	ssh := calls(logf, "ssh")
	// The probe, the mounts, and the command.
	if len(ssh) != 3 {
		t.Fatalf("ssh calls: %q", ssh)
	}
	for _, c := range ssh {
		for _, want := range []string{"-F none -i " + vm + "/key -p ", "IdentitiesOnly=yes", "BatchMode=yes", " admin@127.0.0.1 "} {
			if !strings.Contains(c, want) {
				t.Errorf("ssh was not given %q in: %s", want, c)
			}
		}
	}
	for _, want := range []string{
		"sudo mount -t virtiofs mcc-cache /mnt/mcc/mcc-cache",
		"sudo mount -o ro,noload /dev/disk/by-id/virtio-mccshare1 /mnt/mcc/mcc-runner",
	} {
		if !strings.Contains(readFile(logf), want) {
			t.Errorf("the guest was not told to %q", want)
		}
	}
	// The script reaches the guest's shell as one quoted word.
	if !strings.HasSuffix(ssh[2], `exec /bin/bash -c 'echo '\''hi'\'''`) {
		t.Errorf("the command was not quoted for the guest's shell: %s", ssh[2])
	}

	listed, err := k.List(ctx)
	if err != nil || len(listed) != 1 || listed[0] != h {
		t.Fatalf("list: %v %v", listed, err)
	}
	var pid int
	_, _ = fmt.Sscan(readFile(vm+"/qemu.pid"), &pid)
	if err := k.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vm); !os.IsNotExist(err) {
		t.Fatal("the VM's directory was not deleted")
	}
	if pid == 0 || syscall.Kill(pid, 0) == nil {
		t.Fatalf("qemu (pid %d) is still running after stop", pid)
	}
	if listed, _ := k.List(ctx); len(listed) != 0 {
		t.Fatalf("job VMs left: %v", listed)
	}
	if !k.Ready(ctx, img) {
		t.Fatal("the base is gone after a job")
	}
}

func TestDiskIsNotShrunkAndDefaultsApply(t *testing.T) {
	k, logf := stubHost(t)
	k.Arch = "amd64"
	reg := newRegistry(t)
	ctx := context.Background()
	img := protocol.ImageSpec{Name: "u", Source: reg.push("acme/u", "1", map[string][]string{"amd64": {"disk"}}), Platform: "linux"}
	if err := k.Prepare(ctx, img); err != nil {
		t.Fatal(err)
	}
	// The stub reports a 40 GiB disk.
	h, err := k.Start(ctx, "small", img, executor.StartOptions{Resources: executor.Resources{DiskGB: 40}})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Stop(ctx, h)
	if got := calls(logf, "qemu-img"); !strings.HasSuffix(got[1], "/vms/mcc-small/disk.qcow2") {
		t.Fatalf("a size was given although the image's disk is as large: %q", got[1])
	}
	qemu := started(t, logf, "qemu", 1)[0]
	for _, want := range []string{"-machine q35,accel=kvm ", "-smp 2 -m 4096M"} {
		if !strings.Contains(qemu, want) {
			t.Errorf("qemu was not given %q in:\n%s", want, qemu)
		}
	}
	// No shares: no shared memory, no firmware on x86, and nothing mounted.
	for _, not := range []string{"memory-backend", "pflash", "vhost-user"} {
		if strings.Contains(qemu, not) {
			t.Errorf("qemu was given %q in:\n%s", not, qemu)
		}
	}
	if len(calls(logf, "virtiofsd")) != 0 || strings.Contains(readFile(logf), "sudo mount") {
		t.Error("shares were set up although none were asked for")
	}
}

func TestStartRefusals(t *testing.T) {
	k, _ := stubHost(t)
	ctx := context.Background()
	img := protocol.ImageSpec{Name: "u", Source: "ghcr.io/acme/u:1", Platform: "linux"}
	if _, err := k.Start(ctx, "a1", img, executor.StartOptions{}); err == nil || !strings.Contains(err.Error(), "not prepared") {
		t.Fatalf("an unprepared image was started: %v", err)
	}
	for _, id := range []string{"../../etc", "a/b", "", ".hidden"} {
		if _, err := k.Start(ctx, id, img, executor.StartOptions{}); err == nil || !strings.Contains(err.Error(), "cannot name a VM") {
			t.Errorf("assignment id %q was accepted: %v", id, err)
		}
	}
	if err := k.Stop(ctx, executor.Handle{Name: "../images"}); err == nil {
		t.Error("stop accepted a name that is not a job VM's")
	}
	if k.NetworkIsolationAvailable() {
		t.Error("network isolation is not built")
	}
}

func TestRefreshReplacesBaseOnlyWhenTagMoves(t *testing.T) {
	k, _ := stubHost(t)
	reg := newRegistry(t)
	ctx := context.Background()
	source := reg.push("acme/u", "1", map[string][]string{"arm64": {"old"}})
	img := protocol.ImageSpec{Name: "u", Source: source, Platform: "linux"}
	disk := filepath.Join(k.Dir, "images", BaseName(source), "disk.qcow2")
	if err := k.Prepare(ctx, img); err != nil {
		t.Fatal(err)
	}
	pulls := reg.pulls
	if err := k.Refresh(ctx, img); err != nil {
		t.Fatal(err)
	}
	// The config is read again to check the image; the disk is not.
	if reg.pulls != pulls+1 {
		t.Fatalf("an unchanged image was downloaded again: %d blob requests", reg.pulls-pulls)
	}
	reg.push("acme/u", "1", map[string][]string{"arm64": {"new"}})
	if err := k.Refresh(ctx, img); err != nil {
		t.Fatal(err)
	}
	if got := readFile(disk); got != "new" {
		t.Fatalf("the base still has the old disk: %q", got)
	}
}

func TestPreloadedContainerImages(t *testing.T) {
	k, logf := stubHost(t)
	k.Preload = []string{"redis:8", "mysql:8.4"}
	reg := newRegistry(t)
	ctx := context.Background()
	source := reg.push("acme/u", "1", map[string][]string{"arm64": {"disk"}})
	img := protocol.ImageSpec{Name: "u", Source: source, Platform: "linux"}

	// A base from before the owner listed container images.
	plain := filepath.Join(k.Dir, "images", BaseName(source))
	_ = os.MkdirAll(plain, 0o700)
	_ = os.WriteFile(filepath.Join(plain, "image.json"), []byte("{}"), 0o600)

	if k.Ready(ctx, img) {
		t.Fatal("a base without the container images counted as ready")
	}
	if err := k.Prepare(ctx, img); err != nil {
		t.Fatal(err)
	}
	base := BaseName(source) + "-" + executor.PreloadKey(k.Preload)
	if _, err := os.Stat(filepath.Join(k.Dir, "images", base, "image.json")); err != nil {
		t.Fatalf("no base named for the container images: %v", err)
	}
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Error("the base without container images was kept")
	}
	if got := readFile(filepath.Join(filepath.Dir(k.Dir), "preloaded")); got != "redis:8\nmysql:8.4\n" {
		t.Fatalf("the guest was told to pull %q", got)
	}
	// The base's own disk is booted, and safely: it is what jobs will run from.
	qemu := started(t, logf, "qemu", 1)
	if len(qemu) != 1 || !strings.Contains(qemu[0], "file="+k.Dir+"/images/"+base+".next/disk.qcow2 ") || strings.Contains(qemu[0], "cache=unsafe") {
		t.Fatalf("qemu calls: %q", qemu)
	}
	if entries, _ := os.ReadDir(filepath.Join(k.Dir, "vms")); len(entries) != 0 {
		t.Errorf("the VM that pulled the container images left files: %v", entries)
	}

	// A VM that does not shut down by itself must not become a base.
	k.Preload = []string{"postgres:17"}
	k.control = func(string, ...string) error { return fmt.Errorf("no answer") }
	if err := k.Prepare(ctx, img); err == nil || !strings.Contains(err.Error(), "shut down cleanly") {
		t.Fatalf("prepare: %v", err)
	}
	if k.Ready(ctx, img) {
		t.Error("a base was kept from a VM that did not shut down cleanly")
	}
}

func TestImagesThatAreRefused(t *testing.T) {
	k, _ := stubHost(t)
	reg := newRegistry(t)
	ctx := context.Background()
	host := strings.TrimPrefix(reg.srv.URL, "http://")
	check := func(name, source, want string) {
		t.Helper()
		err := k.Prepare(ctx, protocol.ImageSpec{Name: name, Source: source, Platform: "linux"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error about %q", name, err, want)
		}
		if entries, _ := os.ReadDir(filepath.Join(k.Dir, "images")); len(entries) != 0 {
			t.Errorf("%s: left %v behind", name, entries)
		}
	}

	check("other architecture", reg.push("acme/x86", "1", map[string][]string{"amd64": {"disk"}}), "no disk for arm64")
	check("missing", host+"/acme/none:1", "404")
	check("not a reference", "ubuntu", "not an image reference")

	// A Tart image, which is what the same name holds for Mac hosts.
	tart, _ := json.Marshal(manifest{MediaType: mediaManifest, Config: descriptor{MediaType: mediaTartConfig, Digest: digestOf(nil), Size: 2}})
	reg.tags["acme/tart:1"] = reg.put(tart).Digest
	check("tart image", host+"/acme/tart:1", "only Mac hosts")

	// A layer whose content is not what the manifest promised.
	source := reg.push("acme/tampered", "1", map[string][]string{"arm64": {"disk"}})
	reg.blobs[digestOf([]byte("disk"))] = []byte("evil")
	check("tampered", source, "not what its digest says")

	// A disk that would have QEMU read another file on the host.
	source = reg.push("acme/backing", "1", map[string][]string{"arm64": {"qcow2 with a backing file"}})
	_ = os.WriteFile(filepath.Join(filepath.Dir(k.Dir), "diskinfo"), []byte(`{"virtual-size":1073741824,"format":"qcow2","backing-filename":"/etc/shadow"}`), 0o644)
	check("backing file", source, "refers to another file")
}

func TestLocalImageDirectory(t *testing.T) {
	k, _ := stubHost(t)
	ctx := context.Background()
	dir := filepath.Join(filepath.Dir(k.Dir), "built")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "disk.qcow2"), []byte("built here"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"architecture":"arm64","os":"linux","user":"admin","uid":1000,"gid":1000}`), 0o644)
	img := protocol.ImageSpec{Name: "local", Source: dir, Platform: "linux"}
	if err := k.Prepare(ctx, img); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(k.Dir, "images", BaseName(dir), "disk.qcow2")
	if readFile(disk) != "built here" {
		t.Fatal("the base is not a copy of the directory's disk")
	}
	_ = os.WriteFile(filepath.Join(dir, "disk.qcow2"), []byte("built again"), 0o644)
	if err := k.Refresh(ctx, img); err != nil {
		t.Fatal(err)
	}
	if readFile(disk) != "built again" {
		t.Fatal("refresh did not pick up the rebuilt disk")
	}
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"architecture":"arm64","user":"root; reboot","uid":1000,"gid":1000}`), 0o644)
	if err := k.Refresh(ctx, img); err == nil {
		t.Fatal("a config whose user is not a user name was accepted")
	}
}

func TestWhatTheExecutorKeepsIsNotAnImage(t *testing.T) {
	k, _ := stubHost(t)
	ctx := context.Background()
	// A job's VM directory, made to look like an image.
	dir := filepath.Join(k.Dir, "vms", "mcc-other")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "disk.qcow2"), []byte("another job's work"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"architecture":"arm64","os":"linux","user":"admin","uid":1000,"gid":1000}`), 0o644)
	for _, src := range []string{dir, k.Dir, filepath.Join(k.Dir, "vms", "..", "vms", "mcc-other")} {
		img := protocol.ImageSpec{Name: "theirs", Source: src, Platform: "linux"}
		if err := k.Prepare(ctx, img); err == nil || !strings.Contains(err.Error(), "agent's own directory") {
			t.Fatalf("%s: prepare: %v", src, err)
		}
		if err := k.Refresh(ctx, img); err == nil {
			t.Fatalf("%s: refreshed", src)
		}
		if k.Ready(ctx, img) {
			t.Fatalf("%s: ready", src)
		}
	}
}

func TestReadOnlyShareDiskIsMadeOncePerState(t *testing.T) {
	k, logf := stubHost(t)
	ctx := context.Background()
	dir := filepath.Join(filepath.Dir(k.Dir), "runner")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "run.sh"), []byte("one"), 0o755)
	first, err := k.shareDisk(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := k.shareDisk(ctx, dir)
	if err != nil || again != first || len(calls(logf, "mke2fs")) != 1 {
		t.Fatalf("an unchanged directory was imaged again: %s, %v", again, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "new-file"), []byte("two"), 0o644)
	changed, err := k.shareDisk(ctx, dir)
	if err != nil || changed == first {
		t.Fatalf("a changed directory was not imaged again: %v", err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Error("the image of the directory's earlier state was kept")
	}
	if _, err := k.shareDisk(ctx, filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing directory was shared")
	}
}

func TestCrashRecoveryFindsAndDeletesLeftovers(t *testing.T) {
	k, _ := stubHost(t)
	ctx := context.Background()
	for _, n := range []string{"mcc-old1", "mcc-old2", "mcc-base-abc.next", "unrelated"} {
		_ = os.MkdirAll(filepath.Join(k.Dir, "vms", n), 0o700)
	}
	// An agent that crashed left this VM's QEMU running; the fake control reaches it.
	stale, err := os.StartProcess(k.Tools.QEMU, []string{"qemu", "-pidfile", filepath.Join(k.Dir, "vms", "mcc-old1", "qemu.pid")}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	go stale.Wait()
	for i := 0; i < 100 && readFile(filepath.Join(k.Dir, "vms", "mcc-old1", "qemu.pid")) == ""; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	listed, err := k.List(ctx)
	if err != nil || len(listed) != 2 || listed[0].AssignmentID != "old1" || listed[1].AssignmentID != "old2" {
		t.Fatalf("list: %v %v", listed, err)
	}
	for _, h := range listed {
		if err := k.Stop(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100 && syscall.Kill(stale.Pid, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(stale.Pid, 0) == nil {
		t.Error("the leftover qemu is still running")
	}
	if listed, _ := k.List(ctx); len(listed) != 0 {
		t.Fatalf("left: %v", listed)
	}
}

func TestParseReference(t *testing.T) {
	good := map[string]reference{
		"ghcr.io/my-compute-cloud/ubuntu-runner:24.04": {"ghcr.io", "my-compute-cloud/ubuntu-runner", "24.04", ""},
		"ghcr.io/acme/u":       {"ghcr.io", "acme/u", "latest", ""},
		"localhost:5000/u:dev": {"localhost:5000", "u", "dev", ""},
		"ghcr.io/acme/u@sha256:" + strings.Repeat("a", 64): {"ghcr.io", "acme/u", "", "sha256:" + strings.Repeat("a", 64)},
	}
	for s, want := range good {
		if got, err := parseReference(s); err != nil || got != want {
			t.Errorf("%s: %+v %v", s, got, err)
		}
	}
	for _, s := range []string{"ubuntu", "ghcr.io/Acme/u", "ghcr.io/acme/u:", "ghcr.io/../u", "ghcr.io/acme/u@sha256:abc", "-x/acme/u", "ghcr.io/acme/u?x=1"} {
		if _, err := parseReference(s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
	if r, _ := parseReference("localhost:5000/u"); r.base() != "http://localhost:5000" {
		t.Errorf("a registry on this machine: %s", r.base())
	}
	if r, _ := parseReference("ghcr.io/acme/u"); r.base() != "https://ghcr.io" {
		t.Errorf("a registry elsewhere: %s", r.base())
	}
}

// A control socket that speaks QEMU's protocol, events between answers included.
func TestQMP(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mcc-qmp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "qmp.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan string, 10)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			fmt.Fprintln(c, `{"QMP":{"version":{},"capabilities":[]}}`)
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				var m struct{ Execute string }
				_ = json.Unmarshal(sc.Bytes(), &m)
				got <- m.Execute
				switch m.Execute {
				case "quit":
					c.Close()
				case "bogus":
					fmt.Fprintln(c, `{"error":{"class":"CommandNotFound","desc":"no"}}`)
				default:
					fmt.Fprintln(c, `{"timestamp":{},"event":"POWERDOWN"}`)
					fmt.Fprintln(c, `{"return":{}}`)
				}
			}
			c.Close()
		}
	}()
	if err := qmp(socket, "system_powerdown"); err != nil {
		t.Fatal(err)
	}
	if a, b := <-got, <-got; a != "qmp_capabilities" || b != "system_powerdown" {
		t.Fatalf("sent %s, %s", a, b)
	}
	if err := qmp(socket, "quit"); err != nil {
		t.Fatalf("quit, after which QEMU closes the socket: %v", err)
	}
	if err := qmp(socket, "bogus"); err == nil {
		t.Fatal("an error from QEMU was not reported")
	}
	if err := qmp(filepath.Join(dir, "none.sock"), "quit"); err == nil {
		t.Fatal("a missing socket was not reported")
	}
}
