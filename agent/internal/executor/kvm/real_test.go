package kvm

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/agent"
	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
	"github.com/my-compute-cloud/mcc/agent/internal/runnerbin"
)

// TestRealVM runs against real VMs on a Linux host with KVM, which takes minutes and
// gigabytes, so it only runs when asked:
//
//	MCC_KVM_IMAGE=<registry image, or directory with disk.qcow2 and config.json> \
//	  [MCC_KVM_PRELOAD=mysql:8.4,redis:8] [MCC_KVM_DIR=<state directory>] \
//	  go test -run RealVM -v -timeout 2h ./internal/executor/kvm
//
// It prepares the image, with the container images if named; starts a job VM with a larger
// disk, a writable cache share and the real Actions runner shared read-only; checks them from
// inside the VM; and then runs the agent's entry command far enough for the runner to start
// and turn down a configuration that is not GitHub's.
func TestRealVM(t *testing.T) {
	source := os.Getenv("MCC_KVM_IMAGE")
	if source == "" {
		t.Skip("MCC_KVM_IMAGE is not set")
	}
	dir := os.Getenv("MCC_KVM_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	k := New(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if list := os.Getenv("MCC_KVM_PRELOAD"); list != "" {
		k.Preload = strings.Split(list, ",")
	}
	for _, p := range k.Preflight() {
		t.Logf("preflight: %s", p)
	}
	ctx := context.Background()
	img := protocol.ImageSpec{Name: "real", Source: source, Platform: "linux"}

	started := time.Now()
	if err := k.Prepare(ctx, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("prepared in %s", time.Since(started).Round(time.Second))
	if !k.Ready(ctx, img) {
		t.Fatal("not ready after prepare")
	}
	info, err := k.baseInfo(k.base(img))
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	if err := k.Refresh(ctx, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("refreshed in %s", time.Since(started).Round(time.Millisecond))

	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[k.Arch]
	runners := runnerbin.New(filepath.Join(dir, "runner-linux"), "linux-"+arch)
	runner, err := runners.Update(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "from-host"), []byte("host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := int(info.DiskBytes>>30) + 20
	started = time.Now()
	h, err := k.Start(ctx, "realtest", img, executor.StartOptions{
		Resources: executor.Resources{CPUs: 4, MemoryMB: 4096, DiskGB: want},
		SharedDirs: map[string]executor.SharedDir{
			"mcc-cache":  {Path: cacheDir, Writable: true},
			"mcc-runner": {Path: runner},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Stop(context.Background(), h) })
	t.Logf("started in %s", time.Since(started).Round(time.Millisecond))
	if list, _ := k.List(ctx); len(list) != 1 || list[0] != h {
		t.Errorf("list: %v", list)
	}

	script := `set -euo pipefail
IFS= read -r FROM_STDIN
echo "stdin=$FROM_STDIN"
ln -sfn ` + executor.LinuxSharesDir + `/mcc-cache "$HOME/.mcc-cache"
export MCC_CACHE_DIR="$HOME/.mcc-cache"
if [ -r /etc/mcc/job-env ]; then . /etc/mcc/job-env; fi
for i in $(seq 1 30); do
  GB=$(( $(df --output=size -B1 / | tail -1) / 1000000000 ))
  [ "$GB" -ge ` + fmt.Sprint(want-4) + ` ] && break
  sleep 2
done
echo "disk_gb=$GB"
[ "$GB" -ge ` + fmt.Sprint(want-4) + ` ] && echo disk_grown
echo "from_host=$(cat "$MCC_CACHE_DIR/from-host")"
echo guest > "$MCC_CACHE_DIR/from-guest"
echo "npm_config_cache=${npm_config_cache:-unset}"
npm install --silent --prefix "$(mktemp -d)" semver >/dev/null
echo "npm_cache_entries=$(find "$MCC_CACHE_DIR/npm" -type f | wc -l | tr -d ' ')"
R=` + executor.LinuxSharesDir + `/mcc-runner
[ -x "$R/run.sh" ] && echo runner_shared
touch "$R/by-job" 2>/dev/null || echo runner_read_only_for_job
sudo touch "$R/by-root" 2>/dev/null || echo runner_read_only_for_root
sudo mount -o remount,rw "$R" 2>/dev/null && sudo touch "$R/by-root" 2>/dev/null || echo runner_cannot_be_made_writable
curl -fsS -o /dev/null https://github.com && echo internet_reachable
`
	for _, ref := range k.Preload {
		script += `docker image inspect ` + ref + ` >/dev/null && echo "preloaded=` + ref + `"
`
	}
	var out bytes.Buffer
	code, err := k.Exec(ctx, h, executor.Command{Script: script, Stdin: strings.NewReader("secret-line\n"), Output: &out})
	t.Logf("in the VM:\n%s", out.String())
	if err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	got := out.String()
	for _, line := range []string{
		"stdin=secret-line", "from_host=host", "disk_grown", "runner_shared", "internet_reachable",
		"runner_read_only_for_job", "runner_read_only_for_root", "runner_cannot_be_made_writable",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q", line)
		}
	}
	if strings.Contains(got, "npm_cache_entries=0") {
		t.Error("npm did not write to the cache")
	}
	for _, ref := range k.Preload {
		if !strings.Contains(got, "preloaded="+ref) {
			t.Errorf("%s was not in the job VM before the job pulled anything", ref)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(cacheDir, "from-guest")); string(b) != "guest\n" {
		t.Errorf("the host does not see what the job wrote: %q", b)
	}
	if st, err := os.Stat(filepath.Join(cacheDir, "from-guest")); err == nil {
		if uid := fileUID(st); uid != os.Getuid() {
			t.Errorf("what the job wrote belongs to uid %d on the host, not to this user", uid)
		}
	}
	for _, f := range []string{"by-job", "by-root"} {
		if _, err := os.Stat(filepath.Join(runner, f)); err == nil {
			t.Errorf("the job wrote %s into the host's runner directory", f)
		}
	}
	if code, _ := k.Exec(ctx, h, executor.Command{Script: "exit 7"}); code != 7 {
		t.Errorf("exit code %d, want 7", code)
	}

	// The agent's own entry command, with a configuration that is well formed but is no
	// registration: the runner starts, finds that it has no credentials, and keeps retrying
	// until it is stopped, as an idle runner is.
	jit := base64.StdEncoding.EncodeToString([]byte(`{".runner":"` + base64.StdEncoding.EncodeToString([]byte(`{"gitHubUrl":"https://github.com/my-compute-cloud/none"}`)) + `"}`))
	out.Reset()
	entry, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	code, err = k.Exec(entry, h, executor.Command{Script: agent.EntryScript(img), Stdin: strings.NewReader(jit + "\n"), Output: &out})
	t.Logf("entry command: exit %d, %v:\n%s", code, err, executor.LastLines(out.String(), 6))
	if entry.Err() == nil {
		t.Errorf("the entry command ended by itself: exit %d, %v", code, err)
	}
	if !runnerStarted(out.String()) {
		t.Error("the Actions runner did not start from the entry command")
	}

	if err := k.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(k.vmDir(h.Name)); !os.IsNotExist(err) {
		t.Errorf("the VM's directory is still there: %v", err)
	}
	if list, _ := k.List(ctx); len(list) != 0 {
		t.Errorf("VMs left: %v", list)
	}
}
