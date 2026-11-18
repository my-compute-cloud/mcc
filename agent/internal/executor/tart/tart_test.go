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
  list)
    printf '['; sep=''; while read -r n; do [ -n "$n" ] && printf '%s{"Name":"%s","Source":"local","Running":false}' "$sep" "$n" && sep=','; done < "$VMS"; printf ']\n' ;;
  clone) echo "$3" >> "$VMS" ;;
  delete) grep -vx "$2" "$VMS" > "$VMS.tmp"; mv "$VMS.tmp" "$VMS" ;;
  rename) grep -vx "$2" "$VMS" > "$VMS.tmp"; echo "$3" >> "$VMS.tmp"; mv "$VMS.tmp" "$VMS" ;;
  run) sleep 30 ;;
  exec)
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
		Resources:      executor.Resources{CPUs: 4, MemoryMB: 8192},
		IsolateNetwork: true,
		SharedDirs:     map[string]string{"mcc-runner": "/runner/2.330.0"},
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
		"clone " + img.Source + " " + base,
		"clone " + base + " mcc-a1",
		"set mcc-a1 --cpu 4 --memory 8192 --random-mac",
		"run mcc-a1 --no-graphics --net-softnet --dir=mcc-runner:/runner/2.330.0:ro",
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
