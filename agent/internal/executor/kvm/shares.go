package kvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
)

// share is one host directory as a VM gets it.
type share struct {
	name string
	// socket is where virtiofsd serves a writable directory.
	socket string
	// disk is a read-only directory as a disk image, and serial how the guest finds that disk.
	disk, serial string
}

var shareName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,35}$`)

// startShares gets a VM's shared directories ready before QEMU starts.
//
// A writable directory is served by virtiofsd, one per directory, in a user namespace of its
// own in which the job's user in the guest is this process's user on the host: what a job
// writes belongs to whoever runs mcc, whose own access to the host bounds what the daemon can
// do. Other users in the guest have no counterpart on the host, and their writes may fail.
//
// A read-only directory becomes an ext4 disk image, attached as a read-only disk. QEMU
// refuses writes to it whoever in the guest asks, where a read-only virtiofs would rest on
// the daemon's word (and the virtiofsd most hosts have cannot give it).
func (k *KVM) startShares(ctx context.Context, m *machine, s bootSpec) ([]share, error) {
	names := make([]string, 0, len(s.shares))
	for n := range s.shares {
		if !shareName.MatchString(n) {
			return nil, fmt.Errorf("%q cannot name a shared directory", n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var out []share
	for i, n := range names {
		d := s.shares[n]
		if !d.Writable {
			disk, err := k.shareDisk(ctx, d.Path)
			if err != nil {
				return nil, fmt.Errorf("sharing %s: %w", d.Path, err)
			}
			out = append(out, share{name: n, disk: disk, serial: fmt.Sprintf("mccshare%d", i)})
			continue
		}
		socket := filepath.Join(m.dir, fmt.Sprintf("fs%d.sock", i))
		// A socket's path has a short limit.
		if len(socket) > 100 {
			return nil, fmt.Errorf("the state directory's path is too long for a socket: %s", socket)
		}
		_ = os.Remove(socket)
		cmd := exec.Command(k.Tools.Virtiofsd,
			"--socket-path="+socket,
			"--shared-dir="+d.Path,
			"--sandbox", "namespace",
			"--uid-map", fmt.Sprintf(":%d:%d:1:", s.config.UID, os.Getuid()),
			"--gid-map", fmt.Sprintf(":%d:%d:1:", s.config.GID, os.Getgid()),
		)
		logPath := filepath.Join(m.dir, fmt.Sprintf("fs%d.log", i))
		logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		cmd.Stdout, cmd.Stderr = logf, logf
		err = cmd.Start()
		logf.Close()
		if err != nil {
			return nil, fmt.Errorf("starting virtiofsd: %w", err)
		}
		h := &helper{cmd: cmd, done: make(chan struct{})}
		go func() {
			_ = cmd.Wait()
			close(h.done)
		}()
		m.helpers = append(m.helpers, h)
		failed := func() error {
			return fmt.Errorf("virtiofsd did not start: %s", executor.LastLines(readFile(logPath), 3))
		}
		deadline := time.After(10 * time.Second)
		for {
			if _, err := os.Stat(socket); err == nil {
				break
			}
			select {
			case <-h.done:
				return nil, failed()
			case <-deadline:
				return nil, failed()
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
		out = append(out, share{name: n, socket: socket})
	}
	return out, nil
}

// mountScript mounts the shares in the guest, each at executor.LinuxSharesDir/<name>.
func mountScript(shares []share) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, sh := range shares {
		at := executor.LinuxSharesDir + "/" + sh.name
		fmt.Fprintf(&b, "sudo mkdir -p %s\n", at)
		if sh.socket != "" {
			fmt.Fprintf(&b, "sudo mount -t virtiofs %s %s\n", sh.name, at)
			continue
		}
		dev := "/dev/disk/by-id/virtio-" + sh.serial
		// The disk's name appears a moment after the disk does.
		fmt.Fprintf(&b, "for i in $(seq 50); do [ -e %s ] && break; sleep 0.2; done\n", dev)
		fmt.Fprintf(&b, "sudo mount -o ro,noload %s %s\n", dev, at)
	}
	return b.String()
}

// shareDisk is an ext4 image of a directory as it is now, made once and kept until the
// directory changes. Files keep their owners and modes, so the guest's user reads what
// everyone on the host may read.
func (k *KVM) shareDisk(ctx context.Context, dir string) (string, error) {
	k.shareMu.Lock()
	defer k.shareMu.Unlock()
	var files, blocks int64
	var newest time.Time
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		blocks += (info.Size() + 4095) / 4096
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if files == 0 {
		return "", errors.New("not a directory")
	}
	pathSum := sha256.Sum256([]byte(dir))
	stateSum := sha256.Sum256(fmt.Appendf(nil, "%d %d %d", files, blocks, newest.UnixNano()))
	prefix := hex.EncodeToString(pathSum[:])[:12] + "-"
	root := filepath.Join(k.Dir, "shares")
	img := filepath.Join(root, prefix+hex.EncodeToString(stateSum[:])[:12]+".img")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	now := time.Now()
	if _, err := os.Stat(img); err == nil {
		_ = os.Chtimes(img, now, now)
		return img, nil
	}

	// Room for the files, a block of directory for each, the inode table and the journal.
	inodes := files + files/4 + 1024
	sizeKB := (blocks+files)*4*11/10 + inodes/2 + 64*1024
	tmp := img + ".tmp"
	_ = os.Remove(tmp)
	if _, err := k.run(ctx, k.Tools.Mke2fs, "-q", "-t", "ext4", "-m", "0", "-L", "mcc-share",
		"-N", fmt.Sprint(inodes), "-d", dir, tmp, fmt.Sprintf("%dK", sizeKB)); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	// A new ext4 has a lost+found that only root may read, which would stop a job copying the
	// directory whole.
	if _, err := k.run(ctx, k.Tools.Debugfs, "-w", "-R", "rmdir lost+found", tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, img); err != nil {
		return "", err
	}
	// Earlier states of this directory, and images of directories not shared for a week, such
	// as old runner versions. A VM still using one keeps it open, so deleting it is safe.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if p == img {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if strings.HasPrefix(e.Name(), prefix) || now.Sub(info.ModTime()) > 7*24*time.Hour {
			_ = os.Remove(p)
		}
	}
	return img, nil
}
