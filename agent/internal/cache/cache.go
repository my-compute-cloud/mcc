// Package cache keeps host directories that jobs use to keep downloads between runs.
//
// A job's VM is deleted when the job ends, so everything it downloaded goes with it. When the
// owner sets a cache directory, each job gets a writable share of one subdirectory of it, and
// the image points package managers there.
//
// A writable share is the one thing a job can leave behind for a later job, so who shares with
// whom is the owner's decision (Scope). Under the cache directory:
//
//	data/<unit>/   what jobs see; one unit per scope
//	index.json     when each unit was last used; kept outside data so jobs cannot change it
package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// ScopeRunner gives one cache to each repository or organization that runners register
	// with: jobs share only with jobs from the same one.
	ScopeRunner = "runner"
	// ScopeHost gives every job on the host the same cache.
	ScopeHost = "host"
)

type Manager struct {
	Root string
	// Scope is ScopeRunner or ScopeHost.
	Scope string
	// MaxBytes is the size above which unused units are deleted. Zero means no limit.
	MaxBytes int64
	Log      *slog.Logger
	Now      func() time.Time

	mu    sync.Mutex
	inUse map[string]int
}

func New(root, scope string, maxGB int, log *slog.Logger) *Manager {
	if scope == "" {
		scope = ScopeRunner
	}
	return &Manager{Root: root, Scope: scope, MaxBytes: int64(maxGB) << 30, Log: log, Now: time.Now, inUse: map[string]int{}}
}

var segment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// unit names the cache for a runner registered with githubURL, for example
// "github.com+acme+builds". '+' cannot appear in a GitHub host, owner or repository name, so
// two different URLs never share a unit.
func (m *Manager) unit(githubURL string) (string, error) {
	if m.Scope == ScopeHost {
		return "host", nil
	}
	u, err := url.Parse(githubURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("cannot tell which repository or organization %q is", githubURL)
	}
	parts := append([]string{u.Hostname()}, strings.Split(strings.Trim(u.Path, "/"), "/")...)
	if len(parts) < 2 || len(parts) > 3 {
		return "", fmt.Errorf("cannot tell which repository or organization %q is", githubURL)
	}
	for _, p := range parts {
		if !segment.MatchString(p) {
			return "", fmt.Errorf("cannot tell which repository or organization %q is", githubURL)
		}
	}
	return strings.ToLower(strings.Join(parts, "+")), nil
}

// Acquire returns the directory to share into a job's VM and a function to call when the VM
// is gone. A unit is never deleted while a job holds it.
func (m *Manager) Acquire(githubURL string) (string, func(), error) {
	unit, err := m.unit(githubURL)
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(m.Root, "data", unit)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	m.inUse[unit]++
	index := m.readIndex()
	index[unit] = m.Now()
	m.writeIndex(index)
	m.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			m.mu.Lock()
			idle := false
			if m.inUse[unit]--; m.inUse[unit] <= 0 {
				delete(m.inUse, unit)
				idle = true
			}
			m.mu.Unlock()
			if idle {
				if n, freed := TrimNix(dir, m.Now().Add(-NixUnusedFor)); n > 0 {
					m.Log.Info("deleted Nix paths no job has used lately", "cache", unit, "paths", n, "freed_mb", freed>>20)
				}
			}
		})
	}
	return dir, release, nil
}

func (m *Manager) readIndex() map[string]time.Time {
	index := map[string]time.Time{}
	if b, err := os.ReadFile(filepath.Join(m.Root, "index.json")); err == nil {
		_ = json.Unmarshal(b, &index)
	}
	return index
}

func (m *Manager) writeIndex(index map[string]time.Time) {
	b, _ := json.MarshalIndent(index, "", "  ")
	tmp := filepath.Join(m.Root, "index.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, filepath.Join(m.Root, "index.json"))
	}
}

// size adds up the files under dir. It does not follow symbolic links, which a job could have
// pointed anywhere.
func size(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// NixUnusedFor is how long a Nix path stays in a cache after the last job that used it.
const NixUnusedFor = 14 * 24 * time.Hour

// TrimNix deletes from a cache's Nix store paths (dir/nix, a binary cache the jobs write)
// those no job has used since cutoff, and says how many and how many bytes. A job marks
// what it used by touching the path's .narinfo, so that file's time is when it was last
// used. Deleting one only costs a later job a copy from wherever it came from first.
//
// Without this the cache would only grow: each version of each package a repository's jobs
// ever used would stay until the whole cache was deleted.
func TrimNix(dir string, cutoff time.Time) (paths int, freed int64) {
	nix := filepath.Join(dir, "nix")
	// A job writes this directory, and could have made it, or nar inside it, a link to
	// somewhere else on the host. Nothing is deleted through a link.
	for _, d := range []string{nix, filepath.Join(nix, "nar")} {
		if st, err := os.Lstat(d); err != nil || !st.IsDir() {
			return 0, 0
		}
	}
	entries, err := os.ReadDir(nix)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".narinfo") {
			continue
		}
		info, err := e.Info()
		// Only ordinary files: a job could have put a link here that points elsewhere.
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		nar := filepath.Join(nix, "nar", strings.TrimSuffix(name, ".narinfo")+".nar")
		if st, err := os.Lstat(nar); err == nil && st.Mode().IsRegular() {
			freed += st.Size()
			_ = os.Remove(nar)
		}
		// The description last, so that a path is never described without its archive for
		// longer than it takes to delete one file.
		if os.Remove(filepath.Join(nix, name)) == nil {
			paths++
		}
	}
	return paths, freed
}

// Prune deletes whole units, least recently used first, until the cache is within MaxBytes.
// Units in use by a running job are kept. Deleting a unit only costs the next job its
// downloads.
func (m *Manager) Prune() {
	if m.MaxBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(filepath.Join(m.Root, "data"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		m.Log.Warn("reading the cache directory failed", "err", err)
		return
	}
	type unit struct {
		name string
		size int64
		used time.Time
	}
	m.mu.Lock()
	index := m.readIndex()
	m.mu.Unlock()
	var units []unit
	var total int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		u := unit{name: e.Name(), size: size(filepath.Join(m.Root, "data", e.Name())), used: index[e.Name()]}
		units = append(units, u)
		total += u.size
	}
	if total <= m.MaxBytes {
		return
	}
	sort.Slice(units, func(i, j int) bool { return units[i].used.Before(units[j].used) })
	for _, u := range units {
		if total <= m.MaxBytes {
			break
		}
		m.mu.Lock()
		busy := m.inUse[u.name] > 0
		m.mu.Unlock()
		if busy {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.Root, "data", u.name)); err != nil {
			m.Log.Warn("deleting a cache failed", "cache", u.name, "err", err)
			continue
		}
		total -= u.size
		m.mu.Lock()
		index := m.readIndex()
		delete(index, u.name)
		m.writeIndex(index)
		m.mu.Unlock()
		m.Log.Info("deleted a cache to stay within cache_max_gb", "cache", u.name, "freed_mb", u.size>>20)
	}
}
