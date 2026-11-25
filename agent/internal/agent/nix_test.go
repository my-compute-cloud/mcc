package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// fakeNix makes a Nix directory with a database in write-ahead mode, as a running Nix
// leaves it, holding one path.
func fakeNix(t *testing.T) *HostNix {
	t.Helper()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "var", "nix", "db")
	for _, d := range []string{db, filepath.Join(dir, "store")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(db, "schema"), []byte("10"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(sqlite, filepath.Join(db, "db.sqlite"),
		"pragma journal_mode=wal; create table ValidPaths (path text); insert into ValidPaths values ('/nix/store/abc-hello');").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return &HostNix{Dir: dir, Work: filepath.Join(t.TempDir(), "nix"), Sqlite: sqlite}
}

func TestNixSnapshotIsAWholeDatabase(t *testing.T) {
	n := fakeNix(t)
	dir, remove, err := n.Snapshot(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(n.Sqlite, filepath.Join(dir, "db", "db.sqlite"), "select path from ValidPaths").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "/nix/store/abc-hello" {
		t.Fatalf("the copy holds %q (%v)", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "db", "schema")); string(b) != "10" {
		t.Fatalf("schema %q", b)
	}
	for _, d := range nixStateDirs {
		if _, err := os.Stat(filepath.Join(dir, d)); err != nil {
			t.Fatalf("missing %s", d)
		}
	}
	remove()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the copy was not removed")
	}
}

func TestANixSnapshotIsOnlyEverMadeInsideItsWorkDirectory(t *testing.T) {
	n := fakeNix(t)
	beside := filepath.Join(filepath.Dir(n.Work), "keep")
	if err := os.WriteFile(beside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"..", "../..", "a/b", "/tmp/x", "", "."} {
		if _, remove, err := n.Snapshot(context.Background(), id); err == nil {
			remove()
			t.Errorf("Snapshot(%q) should fail", id)
		}
	}
	if _, err := os.Stat(beside); err != nil {
		t.Errorf("a refused snapshot removed what was beside the work directory: %v", err)
	}
}

func TestJobsGetTheHostsNixStoreReadOnly(t *testing.T) {
	n := fakeNix(t)
	f := fake.New()
	cp, _, _ := startAgentOpts(t, config.DefaultPolicy(), f, func(o *Options) { o.HostNix = n })
	<-cp.hellos
	cp.next(isType("capacity"))

	cp.send(startMsg("n1"))
	cp.next(status("n1", "finished"))
	shares := f.Options["n1"].SharedDirs
	store, state := shares[nixStoreShare], shares[nixStateShare]
	if store.Path != filepath.Join(n.Dir, "store") || store.Writable {
		t.Fatalf("store share %+v", store)
	}
	if state.Path != filepath.Join(n.Work, "n1") || state.Writable {
		t.Fatalf("state share %+v", state)
	}
	// The copy of the database is the job's and goes with it.
	if _, err := os.Stat(state.Path); !os.IsNotExist(err) {
		t.Fatal("the copy outlived the job")
	}

	// A script is not a job: it gets the VM and nothing of the host's.
	m := startMsg("n2")
	m["workload"] = map[string]any{"kind": "script", "script": "true"}
	cp.send(m)
	cp.next(func(m map[string]any) bool { return m["type"] == "status" && m["assignment_id"] == "n2" })
	if got := f.Options["n2"].SharedDirs; len(got) != 0 {
		t.Fatalf("a script was given %+v", got)
	}
}

// The entry commands are shell that only ever runs inside a VM; this at least catches one
// that does not parse.
func TestEntryScriptsParse(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		script := entryScript(protocol.ImageSpec{Platform: platform})
		if !strings.Contains(script, "mcc_nix || true") {
			t.Fatalf("%s: the Nix setup is missing", platform)
		}
		// The runner is not exec'd: what the job fetched is kept once it has ended, and the
		// entry command still ends as the runner did.
		keep, exit := strings.Index(script, "mcc_nix_keep || true"), strings.LastIndex(script, "exit $rc")
		if run := strings.Index(script, "./run.sh --jitconfig"); run < 0 || keep < run || exit < keep || strings.Contains(script, "exec ./run.sh") {
			t.Fatalf("%s: sources are not kept after the runner ends", platform)
		}
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", platform, err, out)
		}
	}
}

// An image no pool of this host is set to is one a job may name: it is not fetched ahead of
// time, and is fetched and run when a job asks for it.
func TestAnImageAJobNamesIsFetchedWhenAsked(t *testing.T) {
	f := fake.New()
	cp, _, _ := startAgent(t, config.DefaultPolicy(), f)
	<-cp.hellos
	cp.next(isType("capacity"))

	tahoe := protocol.ImageSpec{Name: "tahoe", Source: "ghcr.io/acme/tahoe:1", Platform: "darwin"}
	cp.send(map[string]any{
		"type":   "config",
		"pools":  []protocol.PoolConfig{{Name: "mac", Label: "mcc-macos", Image: image.Name}},
		"images": []protocol.ImageSpec{image, tahoe},
	})
	// A start is handled after the config before it, so by the time this one has run the
	// agent has prepared what it was going to.
	cp.send(startMsg("p1"))
	cp.next(status("p1", "finished"))
	ctx := context.Background()
	if !f.Ready(ctx, image) || f.Ready(ctx, tahoe) {
		t.Fatalf("ahead of time: pool's image ready %v, the other %v", f.Ready(ctx, image), f.Ready(ctx, tahoe))
	}

	m := startMsg("p2")
	m["image"] = tahoe
	cp.send(m)
	cp.next(status("p2", "finished"))
	if !f.Ready(ctx, tahoe) {
		t.Fatal("the named image was not fetched for the job that asked")
	}
}

// runPostBuildHook renders the hook the entry command writes to /etc/nix/mcc-built, with Nix
// replaced by stubs in bin, and runs it for paths, keeping into cache.
func runPostBuildHook(t *testing.T, cache, bin string, paths ...string) {
	t.Helper()
	start := strings.Index(nixSetup, "<<HOOK\n")
	end := strings.Index(nixSetup, "\nHOOK\n")
	if start < 0 || end < start {
		t.Fatal("the hook is missing from the Nix setup")
	}
	render := exec.Command("bash", "-c", "cat <<HOOK\n"+nixSetup[start+len("<<HOOK\n"):end]+"\nHOOK\n")
	render.Env = append(os.Environ(), "MCC_CACHE_DIR="+cache)
	hook, err := render.Output()
	if err != nil {
		t.Fatalf("rendering the hook: %v", err)
	}
	h := regexp.MustCompile(`(?m)^NS=.*$`).ReplaceAllString(string(hook), "")
	h = strings.Replace(h, "mkdir -p", "NS="+filepath.Join(bin, "nix-store")+"\nmkdir -p", 1)
	f := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(f, []byte(h), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", f)
	cmd.Env = append(os.Environ(), "OUT_PATHS="+strings.Join(paths, " "))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the hook: %v: %s", err, out)
	}
}

// nixStubs writes a nix and a nix-store that answer for two paths: a flake's source, which
// was fetched and is addressed by its content, and a build output, which has a deriver.
func nixStubs(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	stubs := map[string]string{
		"nix": `#!/bin/sh
for p in "$@"; do case "$p" in
  */aaaa-source) printf '%s\tca:fixed:r:sha256:1xcm cache.nixos.org-1:c2ln\n' "$p" ;;
  */bbbb-hello) printf '%s\tultimate mcc-1:aGVsbG8=\n' "$p" ;;
esac; done
`,
		"nix-store": `#!/bin/sh
case "$1 $2" in
  "-q --hash") echo sha256:0000 ;;
  "-q --size") echo 4 ;;
  "-q --references") ;;
  "-q --deriver") case "$3" in */bbbb-hello) echo /nix/store/cccc-hello.drv ;; *) echo unknown-deriver ;; esac ;;
  "--dump "*) printf nar ;;
esac
`,
	}
	for name, s := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// A fetched source keeps that it is addressed by its content, which a job that takes it
// from the cache, and a tool publishing what the job built, go by. A built path names its
// derivation.
func TestTheCacheKeepsWhatAddressesAPathByItsContent(t *testing.T) {
	cache, bin := t.TempDir(), nixStubs(t)
	src, built := "/nix/store/aaaa-source", "/nix/store/bbbb-hello"
	runPostBuildHook(t, cache, bin, src, built)

	read := func(h string) string {
		b, err := os.ReadFile(filepath.Join(cache, "nix", h+".narinfo"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	got := read("aaaa")
	want := "StorePath: /nix/store/aaaa-source\nURL: nar/aaaa.nar\nCompression: none\nNarHash: sha256:0000\nNarSize: 4\nReferences: \n" +
		"Sig: cache.nixos.org-1:c2ln\nCA: fixed:r:sha256:1xcm\n"
	if got != want {
		t.Fatalf("source narinfo:\n%s\nwant:\n%s", got, want)
	}
	got = read("bbbb")
	if !strings.Contains(got, "\nDeriver: cccc-hello.drv\n") || !strings.Contains(got, "\nSig: mcc-1:aGVsbG8=\n") || strings.Contains(got, "CA:") || strings.Contains(got, "ultimate") {
		t.Fatalf("built narinfo:\n%s", got)
	}

	// One kept before the CA line was gets it, once; one that is not addressed by its
	// content gets none.
	old := "StorePath: /nix/store/aaaa-source\nURL: nar/aaaa.nar\nCompression: none\nNarHash: sha256:0000\nNarSize: 4\nReferences: \nSig: cache.nixos.org-1:c2ln\n"
	if err := os.WriteFile(filepath.Join(cache, "nix", "aaaa.narinfo"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	builtBefore := read("bbbb")
	runPostBuildHook(t, cache, bin, src, built)
	runPostBuildHook(t, cache, bin, src, built)
	if got := read("aaaa"); got != old+"CA: fixed:r:sha256:1xcm\n" {
		t.Fatalf("backfilled source narinfo:\n%s", got)
	}
	if got := read("bbbb"); got != builtBefore {
		t.Fatalf("a built path's narinfo changed:\n%s", got)
	}
}
