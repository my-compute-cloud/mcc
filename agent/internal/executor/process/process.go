// Package process runs work on the host itself: a process in a directory, with no virtual
// machine around it. It is what a pool with isolation "host" uses, on hosts whose owner
// allows that.
//
// Each piece of work gets a directory of its own, deleted afterwards. It may also be lent
// kept directories (executor.KeptDir), which are not.
//
// It promises much less than a VM. The work runs as an account on this machine and can do
// whatever that account can. Stop kills what the work started that is still in its process
// group, and deletes its directory; a process that left the group, and files written
// elsewhere, stay. With a dedicated account (User) and nothing else running as it, Stop also
// kills everything that account still runs.
package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// JobDirEnv names the work's directory in its environment.
const JobDirEnv = "MCC_JOB_DIR"

type Process struct {
	// Dir holds one directory per piece of work.
	Dir string
	// StateDir holds what the agent needs to clean up after a crash. It is the agent's own, so
	// that work cannot change it.
	StateDir string
	// User is the account work runs as, entered with sudo. Empty means the agent's own.
	User string
	// Grace is how long work has to end after it is asked to, before it is killed.
	Grace time.Duration
	Log   *slog.Logger

	mu   sync.Mutex
	jobs map[string]*job
	// lanes are the kept directories in use, by "key/lane".
	lanes map[string]bool
}

type job struct {
	dir string
	cmd *exec.Cmd
	// kept are the directories this work was lent, to tidy and give back.
	kept []kept
}

type kept struct {
	lane string // "key/lane", as in Process.lanes
	dir  string
	wipe []string
}

// state is what is written down about a piece of work, to find it again after a crash.
type state struct {
	AssignmentID string `json:"assignment_id"`
	Dir          string `json:"dir"`
	// PID is the process group's leader, and Started when it began (see startedAt): a process
	// id is used again after a restart, and must not be taken for the work's.
	PID     int    `json:"pid,omitempty"`
	Started string `json:"started,omitempty"`
}

func New(dir, stateDir, user string, log *slog.Logger) *Process {
	return &Process{Dir: dir, StateDir: stateDir, User: user, Grace: 10 * time.Second, Log: log, jobs: map[string]*job{}, lanes: map[string]bool{}}
}

// Preflight reports what would stop work from running here.
func (p *Process) Preflight(ctx context.Context) []string {
	var problems []string
	if _, err := exec.LookPath("bash"); err != nil {
		problems = append(problems, "bash is not installed")
	}
	if p.User != "" {
		if out, err := p.asUser(ctx, "true").CombinedOutput(); err != nil {
			problems = append(problems, fmt.Sprintf("cannot run commands as %s with sudo without a password: %s", p.User, executor.LastLines(string(out), 2)))
		} else if out, err := p.asUser(ctx, "test", "-w", p.Dir).CombinedOutput(); err != nil {
			problems = append(problems, fmt.Sprintf("%s cannot write to %s %s", p.User, p.Dir, executor.LastLines(string(out), 2)))
		}
	} else if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		problems = append(problems, err.Error())
	}
	return problems
}

func (p *Process) Prepare(context.Context, protocol.ImageSpec) error { return nil }
func (p *Process) Refresh(context.Context, protocol.ImageSpec) error { return nil }
func (p *Process) Ready(context.Context, protocol.ImageSpec) bool    { return true }

// idPattern keeps an assignment id from being anything but one path element.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// Start makes the work's directory and copies the read-only shares into it, each under its
// share name. A writable share is not copied: the work is given its path (see Exec's Env).
func (p *Process) Start(ctx context.Context, assignmentID string, _ protocol.ImageSpec, opts executor.StartOptions) (executor.Handle, error) {
	h := executor.Handle{AssignmentID: assignmentID, Name: "job-" + assignmentID}
	if !idPattern.MatchString(assignmentID) {
		return h, fmt.Errorf("assignment id %q cannot name a directory", assignmentID)
	}
	dir := filepath.Join(p.Dir, h.Name)
	if err := p.writeState(state{AssignmentID: assignmentID, Dir: dir}); err != nil {
		return h, err
	}
	if err := p.mkdir(ctx, dir); err != nil {
		return h, err
	}
	for name, share := range opts.SharedDirs {
		if share.Writable {
			continue
		}
		if err := p.copyIn(ctx, share.Path, filepath.Join(dir, name)); err != nil {
			return h, fmt.Errorf("copying %s: %w", name, err)
		}
	}
	j := &job{dir: dir}
	p.mu.Lock()
	p.jobs[assignmentID] = j
	p.mu.Unlock()
	for _, k := range opts.KeptDirs {
		if err := p.lend(ctx, j, k); err != nil {
			return h, fmt.Errorf("kept directory for %s: %w", k.Key, err)
		}
	}
	return h, nil
}

// keyChars are what a kept directory's name may hold of its key; anything else becomes "_".
var keyChars = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// lend links a kept directory into the work's own. Kept directories live in Dir/keep, one per
// key and lane: the lowest lane no other work is using.
func (p *Process) lend(ctx context.Context, j *job, k executor.KeptDir) error {
	name := strings.Trim(keyChars.ReplaceAllString(k.Key, "_"), "_.")
	link := filepath.Join(j.dir, k.Link)
	if name == "" || !filepath.IsLocal(k.Link) {
		return fmt.Errorf("cannot name a directory for it")
	}
	for _, w := range k.Wipe {
		if !filepath.IsLocal(w) {
			return fmt.Errorf("cannot wipe %q", w)
		}
	}
	p.mu.Lock()
	lane := 1
	for p.lanes[fmt.Sprintf("%s/%d", name, lane)] {
		lane++
	}
	id := fmt.Sprintf("%s/%d", name, lane)
	p.lanes[id] = true
	kd := kept{lane: id, dir: filepath.Join(p.Dir, "keep", name, strconv.Itoa(lane)), wipe: k.Wipe}
	j.kept = append(j.kept, kd)
	p.mu.Unlock()

	if err := p.run(ctx, "mkdir", "-p", "--", kd.dir, filepath.Dir(link)); err != nil {
		return err
	}
	if err := p.wipe(ctx, kd); err != nil {
		return err
	}
	return p.run(ctx, "ln", "-s", "--", kd.dir, link)
}

// wipe deletes what must not pass from one piece of work to the next.
func (p *Process) wipe(ctx context.Context, k kept) error {
	for _, w := range k.wipe {
		if err := p.run(ctx, "rm", "-rf", "--", filepath.Join(k.dir, w)); err != nil {
			return err
		}
	}
	return nil
}

// run runs a command as the work's account.
func (p *Process) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if p.User != "" {
		cmd = p.asUser(ctx, append([]string{name}, args...)...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", name, executor.LastLines(string(out), 2))
	}
	return nil
}

// baseEnv is what work gets of the agent's own environment when it runs as the agent's
// account: enough to find the owner's tools, and nothing the agent was configured with.
var baseEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "LC_ALL"}

// Exec runs the script with bash in the work's directory, in a process group of its own.
func (p *Process) Exec(ctx context.Context, h executor.Handle, c executor.Command) (int, error) {
	p.mu.Lock()
	j := p.jobs[h.AssignmentID]
	p.mu.Unlock()
	if j == nil {
		return -1, fmt.Errorf("no work started for %s", h.AssignmentID)
	}
	env := append([]string{JobDirEnv + "=" + j.dir}, c.Env...)
	var cmd *exec.Cmd
	if p.User != "" {
		// A login shell, so the work has that account's own environment. What it is given on
		// top goes through env, since sudo does not pass the agent's environment on.
		args := append([]string{"env"}, env...)
		args = append(args, "bash", "-lc", `cd "$`+JobDirEnv+`" && exec bash -c "$0"`, c.Script)
		cmd = p.asUser(context.Background(), args...)
	} else {
		cmd = exec.Command("bash", "-c", c.Script)
		cmd.Dir = j.dir
		for _, k := range baseEnv {
			if v, ok := os.LookupEnv(k); ok {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		cmd.Env = append(cmd.Env, env...)
	}
	cmd.Stdin = c.Stdin
	out := c.Output
	if out == nil {
		out = io.Discard
	}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	p.mu.Lock()
	j.cmd = cmd
	p.mu.Unlock()
	_ = p.writeState(state{AssignmentID: h.AssignmentID, Dir: j.dir, PID: cmd.Process.Pid, Started: startedAt(cmd.Process.Pid)})

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitCode(err)
	case <-ctx.Done():
		p.killGroup(cmd.Process.Pid, done)
		return -1, ctx.Err()
	}
}

func exitCode(err error) (int, error) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// killGroup asks the work's process group to end, and kills it if it has not within Grace.
// done, if not nil, reports the leader's exit.
func (p *Process) killGroup(pid int, done <-chan error) {
	p.signal(pid, syscall.SIGTERM)
	if done != nil {
		select {
		case <-done:
		case <-time.After(p.Grace):
		}
	}
	// Whether or not the leader has gone, something it started may remain in the group.
	p.signal(pid, syscall.SIGKILL)
}

// signal sends sig to the process group led by pid. Work that runs as another account can
// only be signalled as that account.
func (p *Process) signal(pid int, sig syscall.Signal) {
	if p.User == "" {
		_ = syscall.Kill(-pid, sig)
		return
	}
	// The shell's own kill: what /bin/kill accepts for a process group differs between systems.
	_ = p.asUser(context.Background(), "bash", "-c", fmt.Sprintf("kill -%d -- -%d", int(sig), pid)).Run()
}

// Stop ends whatever the work left running and deletes its directory.
func (p *Process) Stop(ctx context.Context, h executor.Handle) error {
	// Start refused such an id, so there is nothing of ours under it, and its state file's
	// path would be some other file's.
	if !idPattern.MatchString(h.AssignmentID) {
		return nil
	}
	p.mu.Lock()
	j := p.jobs[h.AssignmentID]
	delete(p.jobs, h.AssignmentID)
	others := len(p.jobs)
	p.mu.Unlock()

	st, _ := p.readState(h.AssignmentID)
	dir := st.Dir
	if j != nil {
		dir = j.dir
		if j.cmd != nil && j.cmd.Process != nil {
			p.signal(j.cmd.Process.Pid, syscall.SIGKILL)
		}
	} else if st.PID > 0 && st.Started != "" && startedAt(st.PID) == st.Started {
		// Left by an agent that crashed: the process is still the one that was written down.
		p.killGroup(st.PID, nil)
	}
	if p.User != "" && others == 0 {
		// Nothing else of ours runs as this account, so whatever it still runs is this work's:
		// a daemon that left the process group, for one.
		_ = p.asUser(ctx, "bash", "-c", "kill -9 -1").Run()
	}
	var err error
	if j != nil {
		for _, k := range j.kept {
			if werr := p.wipe(ctx, k); werr != nil && err == nil {
				err = werr
			}
			p.mu.Lock()
			delete(p.lanes, k.lane)
			p.mu.Unlock()
		}
	}
	// The work's own directory goes; a kept directory is only linked from it, and stays.
	if dir != "" && filepath.Dir(dir) == filepath.Clean(p.Dir) {
		if rerr := p.remove(ctx, dir); rerr != nil && err == nil {
			err = rerr
		}
	}
	if rmErr := os.Remove(p.statePath(h.AssignmentID)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
		err = rmErr
	}
	return err
}

// List returns the work that was written down and not cleaned up.
func (p *Process) List(context.Context) ([]executor.Handle, error) {
	entries, err := os.ReadDir(p.StateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []executor.Handle
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		p.mu.Lock()
		_, running := p.jobs[id]
		p.mu.Unlock()
		if !running {
			out = append(out, executor.Handle{AssignmentID: id, Name: "job-" + id})
		}
	}
	return out, nil
}

func (p *Process) NetworkIsolationAvailable() bool { return false }

// NetworkIsolationMissing says why, for the agent's log.
func (p *Process) NetworkIsolationMissing() string {
	return "work that runs on the host itself uses the host's network"
}

func (p *Process) MaxVMs(protocol.ImageSpec) int { return 0 }
func (p *Process) GuestPlatforms() []string      { return []string{runtime.GOOS} }

// ----- helpers -----

// asUser is a command run as the work's account. -n never asks for a password: the owner
// must have allowed the agent's account to run commands as that one without.
func (p *Process) asUser(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "sudo", append([]string{"-n", "-H", "-u", p.User, "--"}, args...)...)
}

func (p *Process) mkdir(ctx context.Context, dir string) error {
	if p.User == "" {
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			return err
		}
		return os.Mkdir(dir, 0o700)
	}
	out, err := p.asUser(ctx, "mkdir", "-m", "700", "--", dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating %s as %s: %s", dir, p.User, executor.LastLines(string(out), 2))
	}
	return nil
}

func (p *Process) remove(ctx context.Context, dir string) error {
	if p.User == "" {
		// The work may have left directories it cannot itself enter.
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
		return os.RemoveAll(dir)
	}
	out, err := p.asUser(ctx, "rm", "-rf", "--", dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("deleting %s as %s: %s", dir, p.User, executor.LastLines(string(out), 2))
	}
	return nil
}

// copyIn copies a directory of the agent's into the work's directory. It goes through tar so
// that it works when the work's account cannot read the agent's files: the agent reads, that
// account writes.
func (p *Process) copyIn(ctx context.Context, from, to string) error {
	if err := p.mkdir(ctx, to); err != nil {
		return err
	}
	read := exec.CommandContext(ctx, "tar", "-C", from, "-cf", "-", ".")
	var write *exec.Cmd
	if p.User == "" {
		write = exec.CommandContext(ctx, "tar", "-C", to, "-xf", "-")
	} else {
		write = p.asUser(ctx, "tar", "-C", to, "-xf", "-")
	}
	pipe, err := read.StdoutPipe()
	if err != nil {
		return err
	}
	write.Stdin = pipe
	var werr, rerr strings.Builder
	write.Stderr, read.Stderr = &werr, &rerr
	if err := write.Start(); err != nil {
		return err
	}
	if err := read.Run(); err != nil {
		_ = write.Wait()
		return fmt.Errorf("reading %s: %s", from, executor.LastLines(rerr.String(), 2))
	}
	if err := write.Wait(); err != nil {
		return fmt.Errorf("writing %s: %s", to, executor.LastLines(werr.String(), 2))
	}
	return nil
}

func (p *Process) statePath(id string) string { return filepath.Join(p.StateDir, id+".json") }

func (p *Process) writeState(s state) error {
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	tmp := p.statePath(s.AssignmentID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.statePath(s.AssignmentID))
}

func (p *Process) readState(id string) (state, error) {
	var s state
	b, err := os.ReadFile(p.statePath(id))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}
