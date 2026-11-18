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
			if m.inUse[unit]--; m.inUse[unit] <= 0 {
				delete(m.inUse, unit)
			}
			m.mu.Unlock()
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
