package cache

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func manager(t *testing.T, scope string, maxBytes int64) *Manager {
	m := New(t.TempDir(), scope, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.MaxBytes = maxBytes
	return m
}

func TestUnits(t *testing.T) {
	m := manager(t, "", 0)
	for url, want := range map[string]string{
		"https://github.com/Acme/Builds":  "github.com+acme+builds",
		"https://github.com/acme/builds/": "github.com+acme+builds",
		"https://github.com/acme":         "github.com+acme",
		"https://ghe.example.com/acme/x":  "ghe.example.com+acme+x",
	} {
		got, err := m.unit(url)
		if err != nil || got != want {
			t.Errorf("unit(%q) = %q, %v; want %q", url, got, err, want)
		}
	}
	for _, url := range []string{"", "github.com/acme", "https://github.com", "https://github.com/../etc", "https://github.com/a/b/c", "https://github.com/a/..", "https://github.com/a+b/c"} {
		if got, err := m.unit(url); err == nil {
			t.Errorf("unit(%q) = %q; want an error", url, got)
		}
	}
	if got, _ := manager(t, ScopeHost, 0).unit("anything"); got != "host" {
		t.Errorf("host scope: %q", got)
	}
}

func TestReposDoNotShare(t *testing.T) {
	m := manager(t, ScopeRunner, 0)
	a, _, _ := m.Acquire("https://github.com/acme/one")
	b, _, _ := m.Acquire("https://github.com/acme/two")
	again, _, _ := m.Acquire("https://github.com/acme/one")
	if a == b || a != again {
		t.Fatalf("a=%s b=%s again=%s", a, b, again)
	}
}

func fill(t *testing.T, dir string, n int) {
	if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPruneDeletesLeastRecentlyUsedAndSparesBusy(t *testing.T) {
	m := manager(t, ScopeRunner, 250)
	now := time.Now()
	m.Now = func() time.Time { return now }
	old, releaseOld, _ := m.Acquire("https://github.com/acme/old")
	fill(t, old, 100)
	now = now.Add(time.Hour)
	busy, _, _ := m.Acquire("https://github.com/acme/busy") // oldest but one, still running
	fill(t, busy, 100)
	now = now.Add(time.Hour)
	recent, releaseRecent, _ := m.Acquire("https://github.com/acme/recent")
	fill(t, recent, 100)
	// A link out of the cache must be neither counted nor followed when deleting.
	outside := t.TempDir()
	fill(t, outside, 1000)
	_ = os.Symlink(outside, filepath.Join(recent, "link"))
	_ = os.Symlink(outside, filepath.Join(old, "link"))
	releaseOld()
	releaseRecent()

	m.Prune()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the least recently used cache was kept")
	}
	for _, dir := range []string{busy, recent} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s was deleted", filepath.Base(dir))
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "blob")); err != nil {
		t.Error("deleting a cache followed a link out of it")
	}
}

func TestNixPathsNoJobHasUsedLatelyAreDeleted(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := New(t.TempDir(), "runner", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Now = func() time.Time { return now }
	dir, release, _ := m.Acquire("https://github.com/acme/app")
	nar := filepath.Join(dir, "nix", "nar")
	if err := os.MkdirAll(nar, 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(hash string, used time.Time) {
		t.Helper()
		info := filepath.Join(dir, "nix", hash+".narinfo")
		for _, f := range []string{info, filepath.Join(nar, hash+".nar")} {
			if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// A job marks a path as used by touching its description.
		if err := os.Chtimes(info, used, used); err != nil {
			t.Fatal(err)
		}
	}
	put("stale", now.Add(-NixUnusedFor-time.Hour))
	put("fresh", now.Add(-time.Hour))
	_ = os.WriteFile(filepath.Join(dir, "nix", "nix-cache-info"), []byte("StoreDir: /nix/store\n"), 0o644)

	// While a job holds the cache nothing is deleted; when the last one lets go, the stale go.
	if _, err := os.Stat(filepath.Join(nar, "stale.nar")); err != nil {
		t.Fatal("deleted while a job held the cache")
	}
	release()
	for _, f := range []string{"stale.narinfo", "nar/stale.nar"} {
		if _, err := os.Stat(filepath.Join(dir, "nix", f)); !os.IsNotExist(err) {
			t.Errorf("%s was kept", f)
		}
	}
	for _, f := range []string{"fresh.narinfo", "nar/fresh.nar", "nix-cache-info"} {
		if _, err := os.Stat(filepath.Join(dir, "nix", f)); err != nil {
			t.Errorf("%s was deleted", f)
		}
	}

	// A job may have replaced the directory with a link out of the cache: nothing is deleted
	// through it.
	outside := t.TempDir()
	old := now.Add(-NixUnusedFor - time.Hour)
	_ = os.WriteFile(filepath.Join(outside, "victim.narinfo"), []byte("x"), 0o644)
	_ = os.Chtimes(filepath.Join(outside, "victim.narinfo"), old, old)
	linked := t.TempDir()
	_ = os.Symlink(outside, filepath.Join(linked, "nix"))
	if n, _ := TrimNix(linked, now); n != 0 {
		t.Error("deleted through a link")
	}
	if _, err := os.Stat(filepath.Join(outside, "victim.narinfo")); err != nil {
		t.Error("a file outside the cache was deleted")
	}
}
