//go:build darwin

package cache

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path, data string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestALentCacheKeepsWhatTheJobAddedAndChanged(t *testing.T) {
	root := t.TempDir()
	m := New(root, "", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	unit := filepath.Join(root, "data", "github.com+acme+one")
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	write(t, filepath.Join(unit, "npm", "kept"), "kept", old)
	write(t, filepath.Join(unit, "npm", "deleted"), "deleted", old)
	write(t, filepath.Join(unit, "nix", "aaa.narinfo"), "path aaa", old)
	write(t, filepath.Join(unit, "changed"), "before", old)

	slot := filepath.Join(root, "slots", "vm")
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	into := filepath.Join(slot, "cache")
	giveBack, err := m.Lend("https://github.com/acme/one", into)
	if err != nil {
		t.Fatal(err)
	}

	// The clone is the cache as it was, times and all: a Nix path's time is when a job last
	// used it, and a clone that made every path new would keep every path for good.
	st, err := os.Stat(filepath.Join(into, "nix", "aaa.narinfo"))
	if err != nil || !st.ModTime().Equal(old) {
		t.Fatalf("the clone of a path: %v, %v (want %v)", st, err, old)
	}

	// What a job does to its copy.
	now := time.Now().Truncate(time.Second)
	write(t, filepath.Join(into, "npm", "added"), "added", now)
	write(t, filepath.Join(into, "fresh", "deep", "file"), "deep", now)
	write(t, filepath.Join(into, "changed"), "after", now)
	if err := os.Chtimes(filepath.Join(into, "nix", "aaa.narinfo"), now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(into, "npm", "deleted")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(into, "nix", "nar", ".bbb.123"), "half a nar", now)
	if err := os.Symlink("/etc", filepath.Join(into, "link")); err != nil {
		t.Fatal(err)
	}
	// It may not touch the cache itself, which is not what it was given.
	if _, err := os.Stat(filepath.Join(unit, "npm", "added")); err == nil {
		t.Fatal("the job wrote the cache itself")
	}

	giveBack()

	for path, want := range map[string]string{
		"npm/added":       "added",
		"fresh/deep/file": "deep",
		"changed":         "after",
		"npm/kept":        "kept",
		// Another job of the repository may still want what this one deleted.
		"npm/deleted": "deleted",
	} {
		if got := read(t, filepath.Join(unit, path)); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	if st, err := os.Stat(filepath.Join(unit, "nix", "aaa.narinfo")); err != nil || !st.ModTime().Equal(now) {
		t.Errorf("a path the job used is not marked as used: %v, %v", st, err)
	}
	if _, err := os.Lstat(filepath.Join(unit, "link")); err == nil {
		t.Error("a link the job made was kept")
	}
	if _, err := os.Lstat(filepath.Join(unit, "nix", "nar", ".bbb.123")); err == nil {
		t.Error("a file the job was still writing was kept")
	}
	if _, err := os.Stat(into); err == nil {
		t.Error("the copy was left behind")
	}
	if m.inUse["github.com+acme+one"] != 0 {
		t.Error("the cache is still held")
	}
}

func TestNothingIsWrittenIntoACacheThroughALink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	unit := filepath.Join(root, "unit")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	// Left by a job before jobs had copies: a directory of the cache that is a link.
	if err := os.Symlink(outside, filepath.Join(unit, "npm")); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "copy")
	write(t, filepath.Join(src, "npm", "planted"), "x", time.Now())
	if _, err := merge(src, unit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "planted")); err == nil {
		t.Fatal("a file went through a link out of the cache")
	}
}
