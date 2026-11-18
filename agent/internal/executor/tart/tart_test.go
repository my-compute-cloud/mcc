package tart

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// stubTart writes a fake tart that logs its arguments and keeps a list of VMs in a file.
func stubTart(t *testing.T) (*Tart, string) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "calls")
	vms := filepath.Join(dir, "vms")
	_ = os.WriteFile(vms, nil, 0o644)
	script := `#!/bin/bash
echo "$*" >> "` + logf + `"
VMS="` + vms + `"
case "$1" in
  get) echo '{"Disk":50}' ;;
  fqn) echo "$2@sha256:$(cat "` + dir + `/digest" 2>/dev/null)" ;;
  list)
    printf '['; sep=''; while read -r n; do [ -n "$n" ] && printf '%s{"Name":"%s","Source":"local","Running":false}' "$sep" "$n" && sep=','; done < "$VMS"; printf ']\n' ;;
  clone) echo "$3" >> "$VMS" ;;
  delete) grep -vx "$2" "$VMS" > "$VMS.tmp"; mv "$VMS.tmp" "$VMS" ;;
  rename) grep -vx "$2" "$VMS" > "$VMS.tmp"; echo "$3" >> "$VMS.tmp"; mv "$VMS.tmp" "$VMS" ;;
  run) sleep 30 ;;
  exec)
    case "$*" in
      *xcode-select*) cat > /dev/null; exit 0 ;;
      *"docker pull"*) cat > "` + dir + `/preloaded"; exit 0 ;;
    esac
    if [ "$2" = "-i" ]; then read -r line; echo "stdin:$line"; echo "Running job: build"; exit 3; fi ;;
esac
exit 0
`
	bin := filepath.Join(dir, "tart")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tt := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	tt.Bin = bin
	tt.BootTimeout = 5 * time.Second
	return tt, logf
}

func calls(t *testing.T, logf string) []string {
	b, _ := os.ReadFile(logf)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(l, "list") {
			out = append(out, l)
		}
	}
	return out
}

func TestLifecycleCommands(t *testing.T) {
	tt, logf := stubTart(t)
	ctx := context.Background()
	img := protocol.ImageSpec{Name: "seq", Source: "ghcr.io/cirruslabs/macos-sequoia-base:latest", Platform: "darwin"}
	base := BaseName(img.Source)

	if tt.Ready(ctx, img) {
		t.Fatal("ready before prepare")
	}
	if err := tt.Prepare(ctx, img); err != nil {
		t.Fatal(err)
	}
	if !tt.Ready(ctx, img) {
		t.Fatal("not ready after prepare")
	}
	h, err := tt.Start(ctx, "a1", img, executor.StartOptions{
		Resources:      executor.Resources{CPUs: 4, MemoryMB: 8192, DiskGB: 120},
		IsolateNetwork: true,
		SharedDirs: map[string]executor.SharedDir{
			"mcc-runner": {Path: "/runner/2.330.0"},
			"mcc-cache":  {Path: "/caches/acme", Writable: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := tt.Exec(ctx, h, executor.Command{Script: "echo hi", Stdin: strings.NewReader("jit\n"), Output: &out})
	if err != nil || code != 3 || !strings.Contains(out.String(), "stdin:jit") {
		t.Fatalf("exec: code=%d err=%v out=%q", code, err, out.String())
	}
	listed, _ := tt.List(ctx)
	if len(listed) != 1 || listed[0].AssignmentID != "a1" {
		t.Fatalf("list: %+v", listed)
	}
	if err := tt.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if listed, _ := tt.List(ctx); len(listed) != 0 {
		t.Fatalf("VM not deleted: %+v", listed)
	}

	want := []string{
		"pull " + img.Source,
		"delete " + base + "-next",
		"clone " + img.Source + " " + base + "-next",
		"rename " + base + "-next " + base,
		"fqn " + img.Source,
		"clone " + base + " mcc-a1",
		"get mcc-a1 --format json",
		"set mcc-a1 --cpu 4 --memory 8192 --disk-size 120 --random-mac",
		"run mcc-a1 --no-graphics --net-softnet --dir=mcc-cache:/caches/acme --dir=mcc-runner:/runner/2.330.0:ro",
		"exec -i mcc-a1 /bin/bash -c echo hi",
		"stop mcc-a1 --timeout 15",
		"delete mcc-a1",
	}
	// `tart run` stays running in the background, so the guest-agent probe (`exec … true`) can be
	// logged before it; the probe's position is not part of the contract, only that it happened.
	var got []string
	probes := 0
	for _, c := range calls(t, logf) {
		if c == "exec mcc-a1 true" {
			probes++
			continue
		}
		got = append(got, c)
	}
	if probes == 0 {
		t.Fatal("Start did not wait for the guest agent")
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRefreshReplacesBase(t *testing.T) {
	tt, logf := stubTart(t)
	ctx := context.Background()
	img := protocol.ImageSpec{Name: "seq", Source: "ghcr.io/x:latest"}
	_ = tt.Prepare(ctx, img)
	if err := tt.Refresh(ctx, img); err != nil {
		t.Fatal(err)
	}
	if !tt.Ready(ctx, img) {
		t.Fatal("base missing after refresh")
	}
	got := strings.Join(calls(t, logf), "\n")
	base := BaseName(img.Source)
	if !strings.Contains(got, "rename "+base+"-next "+base) {
		t.Fatalf("calls: %s", got)
	}
}

func TestListIgnoresBasesAndOthers(t *testing.T) {
	tt, _ := stubTart(t)
	ctx := context.Background()
	_, _ = tt.run(ctx, "clone", "x", "mcc-base-abc")
	_, _ = tt.run(ctx, "clone", "x", "someone-elses-vm")
	_, _ = tt.run(ctx, "clone", "x", "mcc-job1")
	l, _ := tt.List(ctx)
	if len(l) != 1 || l[0].AssignmentID != "job1" {
		t.Fatalf("%+v", l)
	}
}

func TestDiskIsNotShrunk(t *testing.T) {
	tt, logf := stubTart(t)
	ctx := context.Background()
	img := protocol.ImageSpec{Name: "seq", Source: "ghcr.io/x:latest"}
	_ = tt.Prepare(ctx, img)
	// The stub's VMs have 50 GB disks.
	h, err := tt.Start(ctx, "small", img, executor.StartOptions{Resources: executor.Resources{DiskGB: 40}})
	if err != nil {
		t.Fatal(err)
	}
	_ = tt.Stop(ctx, h)
	if got := strings.Join(calls(t, logf), "\n"); strings.Contains(got, "--disk-size") {
		t.Fatalf("asked Tart for a smaller disk:\n%s", got)
	}
}

func TestRefreshKeepsBaseWhenImageUnchanged(t *testing.T) {
	tt, logf := stubTart(t)
	ctx := context.Background()
	digest := filepath.Join(filepath.Dir(logf), "digest")
	_ = os.WriteFile(digest, []byte("one"), 0o644)
	img := protocol.ImageSpec{Name: "seq", Source: "ghcr.io/x:latest"}
	_ = tt.Prepare(ctx, img)
	before := len(calls(t, logf))
	if err := tt.Refresh(ctx, img); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls(t, logf)[before:], "\n"); strings.Contains(got, "clone") {
		t.Fatalf("rebuilt the base although the image had not changed:\n%s", got)
	}
	_ = os.WriteFile(digest, []byte("two"), 0o644)
	before = len(calls(t, logf))
	if err := tt.Refresh(ctx, img); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls(t, logf)[before:], "\n"); !strings.Contains(got, "rename "+BaseName(img.Source)+"-next") {
		t.Fatalf("did not rebuild the base for a new image:\n%s", got)
	}
}

// fakeXcode makes a directory that looks enough like an Xcode.app to be accepted.
func fakeXcode(t *testing.T, build string) string {
	app := filepath.Join(t.TempDir(), "Xcode.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "Developer"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<plist version="1.0"><dict><key>CFBundleShortVersionString</key><string>27.2</string>
	<key>ProductBuildVersion</key>
	<string>` + build + `</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "version.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestXcodeIsCopiedIntoTheBaseOnce(t *testing.T) {
	tt, logf := stubTart(t)
	ctx := context.Background()
	tt.Xcode = fakeXcode(t, "27B52")
	img := protocol.ImageSpec{Name: "tahoe", Source: "ghcr.io/x:tahoe", Platform: "darwin"}
	plain := BaseName(img.Source)
	// A base from before the owner asked for Xcode.
	_, _ = tt.run(ctx, "clone", "x", plain)

	if tt.Ready(ctx, img) {
		t.Fatal("a base without Xcode counted as ready")
	}
	if err := tt.Prepare(ctx, img); err != nil {
		t.Fatal(err)
	}
	base, _ := tt.base(ctx, img)
	if base == plain || !tt.Ready(ctx, img) {
		t.Fatalf("base %q", base)
	}
	got := strings.Join(calls(t, logf), "\n")
	for _, want := range []string{
		"set " + base + "-next --disk-size 56", // 50, an empty Xcode rounded up to 1, and 5 spare
		"run " + base + "-next --no-graphics",
		"exec -i " + base + "-next /bin/bash -c set -euo pipefail",
		"stop " + base + "-next --timeout 60",
		"rename " + base + "-next " + base,
		"delete " + plain,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "stop "+base+"-next") > strings.Index(got, "rename "+base+"-next") {
		t.Error("the base was put in place while its VM was still running")
	}

	// Jobs clone the base with Xcode; a second prepare does nothing.
	before := len(calls(t, logf))
	_ = tt.Prepare(ctx, img)
	if len(calls(t, logf)) != before {
		t.Error("prepared again")
	}

	// A new Xcode on the host means a new base.
	tt.Xcode = fakeXcode(t, "27C10")
	tt.xcode = xcodeInfo{}
	if tt.Ready(ctx, img) {
		t.Fatal("the base with the old Xcode counted as ready")
	}

	// Linux images are left alone.
	if b, _ := tt.base(ctx, protocol.ImageSpec{Source: "ghcr.io/x:linux", Platform: "linux"}); b != BaseName("ghcr.io/x:linux") {
		t.Errorf("linux base %q", b)
	}
}

func TestMissingXcodeFailsPrepare(t *testing.T) {
	tt, _ := stubTart(t)
	tt.Xcode = "/nowhere/Xcode.app"
	err := tt.Prepare(context.Background(), protocol.ImageSpec{Name: "tahoe", Source: "ghcr.io/x:tahoe"})
	if err == nil || !strings.Contains(err.Error(), "/nowhere/Xcode.app") {
		t.Fatalf("err = %v", err)
	}
}

func TestContainerImagesArePulledIntoLinuxBases(t *testing.T) {
	tt, logf := stubTart(t)
	ctx := context.Background()
	tt.Preload = []string{"redis:8", "mysql:8.4"}
	linux := protocol.ImageSpec{Name: "ubuntu", Source: "ghcr.io/x:ubuntu", Platform: "linux"}
	if err := tt.Prepare(ctx, linux); err != nil {
		t.Fatal(err)
	}
	base, _ := tt.base(ctx, linux)
	if base == BaseName(linux.Source) || !tt.Ready(ctx, linux) {
		t.Fatalf("base %q", base)
	}
	got := strings.Join(calls(t, logf), "\n")
	if strings.Index(got, "stop "+base+"-next") > strings.Index(got, "rename "+base+"-next") {
		t.Errorf("the base was put in place while its VM was still running:\n%s", got)
	}
	// The references reach the guest on standard input, not in a command line.
	refs, _ := os.ReadFile(filepath.Join(filepath.Dir(logf), "preloaded"))
	if string(refs) != "redis:8\nmysql:8.4\n" {
		t.Errorf("the guest was given %q", refs)
	}
	if strings.Contains(got, "mysql:8.4") {
		t.Errorf("an image reference appeared in a command:\n%s", got)
	}

	// The order of the list does not matter; its contents do.
	tt.Preload = []string{"mysql:8.4", "redis:8"}
	if !tt.Ready(ctx, linux) {
		t.Error("reordering the list made a new base")
	}
	tt.Preload = []string{"mysql:8.4"}
	if tt.Ready(ctx, linux) {
		t.Error("a base with another list counted as ready")
	}

	// macOS images are left alone.
	mac := protocol.ImageSpec{Name: "tahoe", Source: "ghcr.io/x:tahoe", Platform: "darwin"}
	if b, _ := tt.base(ctx, mac); b != BaseName(mac.Source) {
		t.Errorf("macOS base %q", b)
	}

	// A refresh pulls the container images again even when the VM image has not changed.
	tt.Preload = []string{"redis:8", "mysql:8.4"}
	before := len(calls(t, logf))
	if err := tt.Refresh(ctx, linux); err != nil {
		t.Fatal(err)
	}
	if again := strings.Join(calls(t, logf)[before:], "\n"); !strings.Contains(again, "rename "+base+"-next") {
		t.Errorf("refresh did not rebuild the base:\n%s", again)
	}
}
