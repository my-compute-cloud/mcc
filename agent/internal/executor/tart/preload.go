package tart

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// preloadTimeout bounds pulling the owner's list of container images into a base.
const preloadTimeout = 45 * time.Minute

// preloadScript runs in a Linux guest, with one container image reference per line on its
// standard input. It is fixed, and the references come from the owner's policy, not from the
// control plane.
const preloadScript = `set -euo pipefail
command -v docker >/dev/null || { echo "the image has no docker to pull container images with"; exit 1; }
while IFS= read -r ref; do
  [ -n "$ref" ] || continue
  echo "pulling $ref"
  sudo docker pull --quiet -- "$ref"
done
# Stopped cleanly, so what was pulled is on disk before the VM is.
sudo systemctl stop docker.socket docker 2>/dev/null || true
sync
`

func (t *Tart) preloads(image protocol.ImageSpec) bool {
	return len(t.Preload) > 0 && image.Platform == "linux"
}

// preloadKey names the list, so that a change to it makes a new base.
func (t *Tart) preloadKey() string {
	refs := append([]string(nil), t.Preload...)
	sort.Strings(refs)
	h := sha256.Sum256([]byte(strings.Join(refs, "\n")))
	return "p" + hex.EncodeToString(h[:])[:8]
}

// preloadImages boots the VM once and pulls the owner's container images into its Docker, so
// that jobs, which clone it, start their containers and services without pulling.
func (t *Tart) preloadImages(ctx context.Context, vm string) error {
	ctx, cancel := context.WithTimeout(ctx, preloadTimeout)
	defer cancel()
	t.Log.Info("pulling container images into the image", "images", t.Preload)
	started := time.Now()

	defer t.halt(context.WithoutCancel(ctx), vm, 60)
	if err := t.boot(ctx, vm); err != nil {
		return err
	}
	var out bytes.Buffer
	code, err := t.Exec(ctx, executor.Handle{Name: vm}, executor.Command{
		Script: preloadScript,
		Stdin:  strings.NewReader(strings.Join(t.Preload, "\n") + "\n"),
		Output: &out,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("pulling container images in the VM failed: %s", lastLines(out.String(), 5))
	}
	t.Log.Info("container images pulled", "took", time.Since(started).Round(time.Second))
	return nil
}
