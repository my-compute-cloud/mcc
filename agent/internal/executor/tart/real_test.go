package tart

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// TestRealVM runs against real Tart VMs, which takes minutes and tens of gigabytes, so it
// only runs when asked:
//
//	MCC_TART_IMAGE=<local VM or registry image> [MCC_TART_XCODE=/Applications/Xcode.app] go test -run RealVM -v -timeout 2h ./internal/executor/tart
//	MCC_TART_IMAGE=<Linux image> MCC_TART_PLATFORM=linux [MCC_TART_PRELOAD=mysql:8.4,redis:8] go test ...
//
// It prepares the image, with the host's Xcode or the container images if named, starts a job
// VM with a larger disk and a writable cache share, and checks them from inside the VM.
func TestRealVM(t *testing.T) {
	source := os.Getenv("MCC_TART_IMAGE")
	if source == "" {
		t.Skip("MCC_TART_IMAGE is not set")
	}
	tt := New(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	tt.Xcode = os.Getenv("MCC_TART_XCODE")
	if list := os.Getenv("MCC_TART_PRELOAD"); list != "" {
		tt.Preload = strings.Split(list, ",")
	}
	ctx := context.Background()
	img := protocol.ImageSpec{Name: "real", Source: source, Platform: "darwin"}
	if os.Getenv("MCC_TART_PLATFORM") == "linux" {
		img.Platform = "linux"
	}

	started := time.Now()
	if err := tt.Prepare(ctx, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("prepared in %s", time.Since(started).Round(time.Second))
	base, _ := tt.base(ctx, img)
	t.Cleanup(func() { _, _ = tt.run(context.Background(), "delete", base) })

	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "from-host"), []byte("host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := tt.diskGB(ctx, base) + 20
	h, err := tt.Start(ctx, "realtest", img, executor.StartOptions{
		Resources:  executor.Resources{CPUs: 4, MemoryMB: 8192, DiskGB: want},
		SharedDirs: map[string]executor.SharedDir{"mcc-cache": {Path: cacheDir, Writable: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tt.Stop(context.Background(), h) })

	script := `set -euo pipefail
# The guest grows its volume just after boot; a little goes to other partitions.
for i in $(seq 1 30); do
  GB=$(( $(diskutil info -plist / | plutil -extract APFSContainerSize raw -o - -) / 1000000000 ))
  [ "$GB" -ge ` + fmt.Sprint(want-2) + ` ] && break
  sleep 2
done
echo "disk_gb=$GB after $i checks"
[ "$GB" -ge ` + fmt.Sprint(want-2) + ` ] && echo disk_grown
C="/Volumes/My Shared Files/mcc-cache"
echo "from_host=$(cat "$C/from-host")"
echo guest > "$C/from-guest"
mkdir -p "$C/npm" && npm_config_cache="$C/npm" npm install --silent --prefix "$(mktemp -d)" semver >/dev/null
echo "npm_cache_entries=$(find "$C/npm" -type f | wc -l | tr -d ' ')"
`
	if tt.Xcode != "" {
		script += `echo "xcode=$(xcodebuild -version | tr '\n' ' ')"
echo "developer_dir=$(xcode-select -p)"
D=$(mktemp -d); printf 'print("built with swiftc")\n' > "$D/main.swift"
xcrun swiftc "$D/main.swift" -o "$D/main" && "$D/main"
echo "sdk=$(xcrun --sdk macosx --show-sdk-version)"
`
	}
	if img.Platform == "linux" {
		script = realLinuxScript(want, tt.Preload)
	}
	var out bytes.Buffer
	code, err := tt.Exec(ctx, h, executor.Command{Script: script, Output: &out})
	t.Logf("in the VM:\n%s", out.String())
	if err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	got := out.String()
	for _, line := range []string{"from_host=host", "disk_grown"} {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q", line)
		}
	}
	if strings.Contains(got, "npm_cache_entries=0") {
		t.Error("npm did not write to the cache")
	}
	for _, ref := range tt.Preload {
		if img.Platform == "linux" && !strings.Contains(got, "preloaded="+ref) {
			t.Errorf("%s was not in the job VM before the job pulled anything", ref)
		}
	}
	if tt.Xcode != "" && !strings.Contains(got, "built with swiftc") {
		t.Error("the Xcode in the VM could not build a program")
	}
	if b, _ := os.ReadFile(filepath.Join(cacheDir, "from-guest")); string(b) != "guest\n" {
		t.Errorf("the host does not see what the job wrote: %q", b)
	}
}

// realLinuxScript does in a Linux guest what the agent's entry command does before the runner
// starts (link the cache, read the image's job environment), then checks
// the disk, the cache, and that preloaded container images are there without a pull.
func realLinuxScript(wantGB int, preload []string) string {
	script := `set -euo pipefail
ln -sfn /mnt/mcc/mcc-cache "$HOME/.mcc-cache"
export MCC_CACHE_DIR="$HOME/.mcc-cache"
if [ -r /etc/mcc/job-env ]; then . /etc/mcc/job-env; fi
for i in $(seq 1 30); do
  GB=$(( $(df --output=size -B1 / | tail -1) / 1000000000 ))
  [ "$GB" -ge ` + fmt.Sprint(wantGB-4) + ` ] && break
  sleep 2
done
echo "disk_gb=$GB"
[ "$GB" -ge ` + fmt.Sprint(wantGB-4) + ` ] && echo disk_grown
echo "from_host=$(cat "$MCC_CACHE_DIR/from-host")"
echo guest > "$MCC_CACHE_DIR/from-guest"
echo "npm_config_cache=${npm_config_cache:-unset}"
npm install --silent --prefix "$(mktemp -d)" semver >/dev/null
echo "npm_cache_entries=$(find "$MCC_CACHE_DIR/npm" -type f | wc -l | tr -d ' ')"
`
	for _, ref := range preload {
		script += `docker image inspect ` + ref + ` >/dev/null && echo "preloaded=` + ref + `"
`
	}
	if len(preload) > 0 {
		script += `START=$(date +%s.%N)
docker run -d --name svc --tmpfs /var/lib/mysql:rw -e MYSQL_ROOT_PASSWORD=r ` + preload[0] + ` >/dev/null
echo "service container started in $(echo "$(date +%s.%N) - $START" | bc)s, without a pull"
`
	}
	return script
}
