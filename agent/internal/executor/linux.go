package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// LinuxSharesDir is where a Linux guest finds the host's shared directories: each at
// LinuxSharesDir/<share name>. macOS guests mount shares by themselves; Linux guests do not, so
// an executor mounts them there before Start returns, in whatever way its hypervisor shares
// directories.
const LinuxSharesDir = "/mnt/mcc"

// PreloadScript runs in a Linux guest, with one container image reference per line on its
// standard input, and pulls each into the guest's Docker. It is fixed, and the references come
// from the owner's policy, not from the control plane.
const PreloadScript = `set -euo pipefail
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

// PreloadKey names a list of container images, so that a change to the list makes a new base.
func PreloadKey(refs []string) string {
	refs = append([]string(nil), refs...)
	sort.Strings(refs)
	h := sha256.Sum256([]byte(strings.Join(refs, "\n")))
	return "p" + hex.EncodeToString(h[:])[:8]
}

// LastLines is the end of a command's output, on one line, for an error message.
func LastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
