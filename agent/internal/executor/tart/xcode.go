package tart

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// xcodeTimeout bounds copying Xcode into a base and setting it up.
const xcodeTimeout = 45 * time.Minute

// xcodeScript runs in the guest, with an archive of the host's Xcode on its standard input.
// It is fixed: nothing from the control plane reaches it.
//
// The archive is streamed rather than read from a shared directory because macOS guests
// cannot follow the symbolic links inside Xcode's frameworks on a shared directory.
const xcodeScript = `set -euo pipefail
sudo -n true 2>/dev/null || { echo "the image's user cannot use sudo without a password"; exit 1; }
sudo rm -rf /Applications/Xcode.app
sudo mkdir -p /Applications/Xcode.app
sudo tar -xpf - -C /Applications/Xcode.app
[ -d /Applications/Xcode.app/Contents/Developer ] || { echo "the copy of Xcode is incomplete"; exit 1; }
sudo xcode-select -s /Applications/Xcode.app/Contents/Developer
sudo xcodebuild -license accept
sudo xcodebuild -runFirstLaunch
xcodebuild -version
sync
`

type xcodeInfo struct {
	version, build string
	read           time.Time
}

func (t *Tart) addsXcode(image protocol.ImageSpec) bool {
	return t.Xcode != "" && image.Platform != "linux"
}

// hostXcode reads the version of the Xcode the owner named. An update to it on the host shows
// up here, which changes the base's name and so leads to a new base.
func (t *Tart) hostXcode(_ context.Context) (xcodeInfo, error) {
	t.mu.Lock()
	cached := t.xcode
	t.mu.Unlock()
	if cached.build != "" && time.Since(cached.read) < time.Minute {
		return cached, nil
	}
	plist, err := os.ReadFile(filepath.Join(t.Xcode, "Contents", "version.plist"))
	build := plistString(plist, "ProductBuildVersion")
	if err != nil || build == "" {
		return xcodeInfo{}, fmt.Errorf("policy names the Xcode at %s, but its version cannot be read there", t.Xcode)
	}
	info := xcodeInfo{version: plistString(plist, "CFBundleShortVersionString"), build: build, read: time.Now()}
	t.mu.Lock()
	t.xcode = info
	t.mu.Unlock()
	return info, nil
}

// plistString finds a string value in an XML property list, which is what Xcode's
// version.plist is. Enough for two known keys; not a property list parser.
func plistString(plist []byte, key string) string {
	m := regexp.MustCompile(`<key>` + regexp.QuoteMeta(key) + `</key>\s*<string>([^<]*)</string>`).FindSubmatch(plist)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// dirGB is the size of a directory's files, rounded up to whole gigabytes.
func dirGB(root string) int {
	var bytes int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			bytes += info.Size()
		}
		return nil
	})
	return int((bytes + (1<<30 - 1)) >> 30)
}

// installXcode boots the VM once, copies the host's Xcode onto its disk, sets it up, and shuts
// the VM down again.
func (t *Tart) installXcode(ctx context.Context, vm string) error {
	x, err := t.hostXcode(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(t.Xcode, "Contents", "Developer")); err != nil {
		return fmt.Errorf("%s is not an Xcode", t.Xcode)
	}
	ctx, cancel := context.WithTimeout(ctx, xcodeTimeout)
	defer cancel()
	t.Log.Info("adding the host's Xcode to the image", "xcode", x.version, "build", x.build)
	started := time.Now()

	// Room for the copy, with some to spare for what first launch installs.
	if disk := t.diskGB(ctx, vm); disk > 0 {
		if _, err := t.run(ctx, "set", vm, "--disk-size", fmt.Sprint(disk+dirGB(t.Xcode)+5)); err != nil {
			return err
		}
	}
	// The VM must be shut down whether or not this works, and that cannot wait on ctx.
	defer t.halt(context.WithoutCancel(ctx), vm, 60)
	if err := t.boot(ctx, vm); err != nil {
		return err
	}
	// macOS tar carries extended attributes along, which code signatures need.
	archive := exec.CommandContext(ctx, "/usr/bin/tar", "-cf", "-", "-C", t.Xcode, ".")
	var archiveErr bytes.Buffer
	archive.Stderr = &archiveErr
	stream, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	if err := archive.Start(); err != nil {
		return fmt.Errorf("reading %s: %w", t.Xcode, err)
	}
	var out bytes.Buffer
	code, err := t.Exec(ctx, executor.Handle{Name: vm}, executor.Command{Script: xcodeScript, Stdin: stream, Output: &out})
	// If the guest stopped reading early, this ends the archive rather than waiting on it.
	_ = stream.Close()
	archived := archive.Wait()
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("setting up Xcode in the VM failed: %s", executor.LastLines(out.String(), 5))
	}
	if archived != nil {
		return fmt.Errorf("reading %s: %v: %s", t.Xcode, archived, executor.LastLines(archiveErr.String(), 3))
	}
	t.Log.Info("Xcode added", "xcode", x.version, "took", time.Since(started).Round(time.Second))
	return nil
}
