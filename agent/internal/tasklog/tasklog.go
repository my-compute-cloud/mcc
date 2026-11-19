// Package tasklog keeps what tasks print, on the host that ran them, and lets a caller read it
// from there. Nothing in it goes to the control plane.
//
// Each run has two files in the store's directory, named by the task's id:
//
//	<id>.log   everything the task wrote to stdout and stderr, up to a limit
//	<id>.json  what is known about the run: when, how it ended, who may read it
//
// Files are pruned by age and by total size, oldest first, and never while the task runs.
package tasklog

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Meta is what is written down about one run.
type Meta struct {
	// ID is what the output is asked for by: the task's id in the control plane's API.
	ID           string `json:"id"`
	AssignmentID string `json:"assignment_id"`
	Task         string `json:"task"`
	// SecretHash is the SHA-256, in hex, of a secret the task's caller chose. Whoever shows
	// that secret may read the output. The agent and the control plane only ever hold the hash.
	SecretHash string    `json:"secret_hash,omitempty"`
	Started    time.Time `json:"started"`
	// Ended is nil while the task runs.
	Ended *time.Time `json:"ended,omitempty"`
	// ExitCode is set if the task ran to an exit; Code says why it did not (see protocol.Status).
	ExitCode *int   `json:"exit_code,omitempty"`
	Code     string `json:"code,omitempty"`
	// Truncated is true if the task wrote more than was kept.
	Truncated bool `json:"truncated,omitempty"`
}

// Running reports whether the task has not ended.
func (m Meta) Running() bool { return m.Ended == nil }

// Allows reports whether a secret is the one whose hash the task's caller gave. A run with no
// hash has no secret that opens it.
func (m Meta) Allows(secret string) bool {
	if m.SecretHash == "" || secret == "" {
		return false
	}
	sum := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(strings.ToLower(m.SecretHash))) == 1
}

// Limits on what is kept. A zero value means the default.
type Limits struct {
	// PerTask is the most kept of one run's output; what it writes beyond that is dropped.
	PerTask int64
	// Total is the most kept of all runs together.
	Total int64
	// KeepFor is how long a run's output is kept after it ends.
	KeepFor time.Duration
}

const (
	defaultPerTask = 64 << 20
	defaultTotal   = 1 << 30
	defaultKeepFor = 7 * 24 * time.Hour
)

func (l Limits) withDefaults() Limits {
	if l.PerTask <= 0 {
		l.PerTask = defaultPerTask
	}
	if l.Total <= 0 {
		l.Total = defaultTotal
	}
	if l.KeepFor <= 0 {
		l.KeepFor = defaultKeepFor
	}
	return l
}

type Store struct {
	Dir    string
	Limits Limits
	// Now is the clock (tests override it).
	Now func() time.Time

	mu   sync.Mutex
	live map[string]*Log
}

func New(dir string, limits Limits) *Store {
	return &Store{Dir: dir, Limits: limits.withDefaults(), Now: time.Now, live: map[string]*Log{}}
}

// idPattern keeps an id from being anything but one file name.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)

// ErrNotFound is returned for output that does not exist, or was pruned.
var ErrNotFound = errors.New("no output for that task on this host")

func (s *Store) logPath(id string) string  { return filepath.Join(s.Dir, id+".log") }
func (s *Store) metaPath(id string) string { return filepath.Join(s.Dir, id+".json") }

// Recover closes the records of runs that an earlier agent left open: it stopped while they
// ran, and their output ends where it stopped.
func (s *Store) Recover() {
	for _, m := range s.list() {
		if m.Running() {
			m.Code = "agent_stopped"
			t := s.Now()
			m.Ended = &t
			_ = s.writeMeta(m)
		}
	}
}

// Log receives one run's output.
type Log struct {
	s    *Store
	meta Meta
	f    *os.File

	mu      sync.Mutex
	size    int64
	closed  bool
	changed chan struct{} // closed and replaced whenever there is more to read, or the run ends
}

// Create starts the record of a run. A second run with the same id replaces the first.
func (s *Store) Create(m Meta) (*Log, error) {
	if !idPattern.MatchString(m.ID) {
		return nil, fmt.Errorf("task id %q cannot name a file", m.ID)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	m.Started, m.Ended, m.ExitCode, m.Code, m.Truncated = s.Now(), nil, nil, "", false
	f, err := os.OpenFile(s.logPath(m.ID), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := s.writeMeta(m); err != nil {
		f.Close()
		return nil, err
	}
	l := &Log{s: s, meta: m, f: f, changed: make(chan struct{})}
	s.mu.Lock()
	s.live[m.ID] = l
	s.mu.Unlock()
	return l, nil
}

// Write keeps p, or as much of it as the limit leaves room for. It never fails the task: a
// task must not stop because its output could not be kept.
func (l *Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return len(p), nil
	}
	room := l.s.Limits.PerTask - l.size
	keep := p
	if int64(len(keep)) > room {
		keep = keep[:max(room, 0)]
		l.meta.Truncated = true
	}
	if len(keep) > 0 {
		n, _ := l.f.Write(keep)
		l.size += int64(n)
		l.wake()
	}
	return len(p), nil
}

// wake tells readers there is something new. The caller holds l.mu.
func (l *Log) wake() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// Close ends the record with how the run ended: its exit code, or a code saying why it has
// none.
func (l *Log) Close(exitCode *int, code string) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	_ = l.f.Close()
	t := l.s.Now()
	l.meta.Ended, l.meta.ExitCode, l.meta.Code = &t, exitCode, code
	m := l.meta
	_ = l.s.writeMeta(m)
	l.wake()
	l.mu.Unlock()

	l.s.mu.Lock()
	if l.s.live[m.ID] == l {
		delete(l.s.live, m.ID)
	}
	l.s.mu.Unlock()
}

// state is the run's record as it stands, and a channel closed when it next changes.
func (l *Log) state() (Meta, int64, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.meta, l.size, l.changed
}

// Meta returns the record of a run.
func (s *Store) Meta(id string) (Meta, error) {
	if !idPattern.MatchString(id) {
		return Meta{}, ErrNotFound
	}
	s.mu.Lock()
	l := s.live[id]
	s.mu.Unlock()
	if l != nil {
		m, _, _ := l.state()
		return m, nil
	}
	return s.readMeta(id)
}

// Copy writes a run's output to w from offset on. With follow it keeps going as the task
// writes more, and returns when the task has ended and everything has been written, or when
// done is closed. flush, if not nil, is called after each write. It returns the run's record as
// it stood at the end.
func (s *Store) Copy(w io.Writer, id string, offset int64, follow bool, done <-chan struct{}, flush func()) (Meta, error) {
	m, err := s.Meta(id)
	if err != nil {
		return m, err
	}
	f, err := os.Open(s.logPath(id))
	if err != nil {
		return m, ErrNotFound
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return m, err
	}
	for {
		// Look at the state before reading: whatever was written before this point is in the
		// file, so reading to its end and then finding the run ended misses nothing.
		s.mu.Lock()
		l := s.live[id]
		s.mu.Unlock()
		var changed <-chan struct{}
		if l != nil {
			m, _, changed = l.state()
		} else if m, err = s.readMeta(id); err != nil {
			return m, err
		}
		if _, err := io.Copy(w, f); err != nil {
			return m, err
		}
		if flush != nil {
			flush()
		}
		if l == nil || !m.Running() || !follow {
			return m, nil
		}
		select {
		case <-changed:
		case <-done:
			return m, nil
		}
	}
}

// Size is how much of a run's output is kept.
func (s *Store) Size(id string) int64 {
	if !idPattern.MatchString(id) {
		return 0
	}
	st, err := os.Stat(s.logPath(id))
	if err != nil {
		return 0
	}
	return st.Size()
}

func (s *Store) writeMeta(m Meta) error {
	b, _ := json.Marshal(m)
	tmp := s.metaPath(m.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(m.ID))
}

func (s *Store) readMeta(id string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		return m, ErrNotFound
	}
	if err := json.Unmarshal(b, &m); err != nil || m.ID != id {
		return m, ErrNotFound
	}
	return m, nil
}

// list returns the record of every run kept, as written down.
func (s *Store) list() []Meta {
	entries, _ := os.ReadDir(s.Dir)
	var out []Meta
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		if m, err := s.readMeta(id); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// Prune deletes the output of runs that ended longer ago than KeepFor, and then of the oldest
// runs until what is left fits in Total. A task that is running is never touched.
func (s *Store) Prune() {
	now := s.Now()
	type kept struct {
		m    Meta
		size int64
	}
	var ended []kept
	var total int64
	for _, m := range s.list() {
		size := s.Size(m.ID)
		s.mu.Lock()
		_, live := s.live[m.ID]
		s.mu.Unlock()
		if live || m.Running() {
			total += size
			continue
		}
		if now.Sub(*m.Ended) > s.Limits.KeepFor {
			s.remove(m.ID)
			continue
		}
		total += size
		ended = append(ended, kept{m, size})
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].m.Ended.Before(*ended[j].m.Ended) })
	for _, k := range ended {
		if total <= s.Limits.Total {
			break
		}
		s.remove(k.m.ID)
		total -= k.size
	}
}

func (s *Store) remove(id string) {
	_ = os.Remove(s.logPath(id))
	_ = os.Remove(s.metaPath(id))
}
