package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runnerShare is the share name of the runner directory. macOS guests mount shares by
// themselves at /Volumes/My Shared Files/<name>; Linux guests do not, so the entry command
// mounts them at linuxShares/<name>.
const runnerShare = "mcc-runner"

const linuxShares = "/mnt/mcc"

// cacheShare is the share name of the job's cache directory, when the owner provides one.
const cacheShare = "mcc-cache"

// githubRunnerScript is the fixed entry command for the github-runner workload. The JIT
// configuration arrives on stdin so it never appears in a process list on the host.
const githubRunnerScript = `set -euo pipefail
IFS= read -r JIT
SHARED="/Volumes/My Shared Files/` + runnerShare + `"
RUNNER="$HOME/actions-runner"
if [ -x "$SHARED/run.sh" ]; then
  rm -rf "$RUNNER.mcc"
  cp -R "$SHARED" "$RUNNER.mcc"
  RUNNER="$RUNNER.mcc"
fi
# The owner's cache for this job, if any, and the image's own say on what to do with it:
# an image may set up the job's environment in /etc/mcc/job-env.
# The cache is reached through a link without spaces in its path, which some build tools
# cannot handle.
if [ -d "/Volumes/My Shared Files/` + cacheShare + `" ]; then
  ln -sfn "/Volumes/My Shared Files/` + cacheShare + `" "$HOME/.mcc-cache"
  export MCC_CACHE_DIR="$HOME/.mcc-cache"
fi
if [ -r /etc/mcc/job-env ]; then
  . /etc/mcc/job-env
fi
cd "$RUNNER"
exec ./run.sh --jitconfig "$JIT"
`

// githubRunnerScriptLinux is the same for Linux guests.
const githubRunnerScriptLinux = `set -euo pipefail
IFS= read -r JIT
SHARES="` + linuxShares + `"
if ! mountpoint -q "$SHARES"; then
  sudo mkdir -p "$SHARES"
  # Fails when the host shares nothing, which is fine.
  sudo mount -t virtiofs com.apple.virtio-fs.automount "$SHARES" 2>/dev/null || true
fi
RUNNER="$HOME/actions-runner"
if [ -x "$SHARES/` + runnerShare + `/run.sh" ]; then
  rm -rf "$RUNNER.mcc"
  cp -R "$SHARES/` + runnerShare + `" "$RUNNER.mcc"
  RUNNER="$RUNNER.mcc"
fi
if [ -d "$SHARES/` + cacheShare + `" ]; then
  ln -sfn "$SHARES/` + cacheShare + `" "$HOME/.mcc-cache"
  export MCC_CACHE_DIR="$HOME/.mcc-cache"
fi
if [ -r /etc/mcc/job-env ]; then
  . /etc/mcc/job-env
fi
cd "$RUNNER"
exec ./run.sh --jitconfig "$JIT"
`

// entryScript is the fixed entry command for an image's guest platform.
func entryScript(image protocol.ImageSpec) string {
	if guestPlatform(image) == "linux" {
		return githubRunnerScriptLinux
	}
	return githubRunnerScript
}

// The runner prints this when it accepts a job.
const jobTakenMarker = "Running job:"

// policyCheck returns why a start must be refused, or "".
func (a *Agent) policyCheck(m protocol.Start) string {
	p := a.o.Policy
	if !p.Active(a.o.Now()) {
		return "outside active hours"
	}
	if p.RequireNetworkIsolation && !a.isolationOK {
		return "network isolation required by policy but unavailable"
	}
	if !p.ImageAllowed(m.Image.Source) {
		return fmt.Sprintf("image %s not allowed by local policy", m.Image.Source)
	}
	a.mu.Lock()
	known, ok := a.images[m.Image.Name]
	a.mu.Unlock()
	if !ok || known.Source != m.Image.Source {
		return fmt.Sprintf("image %s is not in this host's pool configuration", m.Image.Name)
	}
	if m.Workload.Kind != "github-runner" {
		return fmt.Sprintf("unsupported workload %q", m.Workload.Kind)
	}
	if len(p.AllowedGitHubURLs) > 0 {
		u, err := jitGitHubURL(m.Workload.JITConfig)
		if err != nil {
			return "could not read runner configuration to check allowed_github_urls"
		}
		if !p.GitHubURLAllowed(u) {
			return fmt.Sprintf("runner for %s not allowed by local policy", u)
		}
	}
	if a.activeCount() >= a.slotsTotal() {
		return "no free slot"
	}
	platform := guestPlatform(m.Image)
	if !slices.Contains(a.o.Executor.GuestPlatforms(), platform) {
		return fmt.Sprintf("this host cannot run %s guests", platform)
	}
	if lim := a.guestLimit(platform); lim > 0 && a.activeOn(platform) >= lim {
		return fmt.Sprintf("this host already runs %d %s guests, the most allowed", lim, platform)
	}
	return ""
}

// jitGitHubURL extracts the repository or organization URL from a just-in-time runner
// configuration: base64 JSON mapping runner file names to base64 file contents.
func jitGitHubURL(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	var files map[string]string
	if err := json.Unmarshal(raw, &files); err != nil {
		return "", err
	}
	runner, err := base64.StdEncoding.DecodeString(files[".runner"])
	if err != nil {
		return "", err
	}
	var settings struct {
		GitHubURL string `json:"gitHubUrl"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(runner), "\uFEFF")), &settings); err != nil {
		return "", err
	}
	if settings.GitHubURL == "" {
		return "", fmt.Errorf("no gitHubUrl")
	}
	return settings.GitHubURL, nil
}

// jobCache is the cache directory for the repository or organization this job's runner
// registers with.
func (a *Agent) jobCache(m protocol.Start) (string, func(), error) {
	u, err := jitGitHubURL(m.Workload.JITConfig)
	if err != nil {
		return "", nil, fmt.Errorf("could not read the runner configuration to choose a cache: %w", err)
	}
	return a.o.Cache.Acquire(u)
}

func (a *Agent) handleStart(ctx context.Context, m protocol.Start) {
	a.mu.Lock()
	if existing, ok := a.assignments[m.AssignmentID]; ok {
		// Repeated start: no further effect beyond restating where it is.
		state, reason := existing.state, existing.reason
		a.mu.Unlock()
		a.send(&protocol.Status{Type: protocol.TypeStatus, AssignmentID: m.AssignmentID, State: state, Reason: reason})
		return
	}
	a.mu.Unlock()

	if reason := a.policyCheck(m); reason != "" {
		a.log.Warn("refusing start", "assignment", m.AssignmentID, "reason", reason)
		a.send(&protocol.Status{Type: protocol.TypeStatus, AssignmentID: m.AssignmentID, State: protocol.AgentAssignmentStateFailed, Reason: "refused: " + reason})
		return
	}

	wctx, cancel := context.WithCancel(ctx)
	as := &assignment{id: m.AssignmentID, platform: guestPlatform(m.Image), state: protocol.AgentAssignmentStateStarting, cancel: cancel}
	a.mu.Lock()
	a.assignments[m.AssignmentID] = as
	a.mu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		a.runAssignment(wctx, as, m)
	}()
}

func (a *Agent) handleStop(m protocol.Stop) {
	a.mu.Lock()
	as := a.assignments[m.AssignmentID]
	if as != nil && as.cancel != nil {
		as.stopReason = "stopped by control plane"
		if m.Reason != "" {
			as.stopReason += ": " + m.Reason
		}
		as.cancel()
	}
	a.mu.Unlock()
	if as == nil {
		a.log.Info("stop for unknown assignment", "assignment", m.AssignmentID)
	}
}

func (a *Agent) stopReason(as *assignment) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return as.stopReason
}

// runAssignment runs one assignment to completion. The final status is sent only after the
// VM has been deleted.
func (a *Agent) runAssignment(ctx context.Context, as *assignment, m protocol.Start) {
	lg := a.log.With("assignment", m.AssignmentID, "image", m.Image.Name)
	a.setState(as.id, protocol.AgentAssignmentStateStarting, "")
	if a.o.Policy.StayAwakeEnabled() {
		a.awake.Acquire()
		defer a.awake.Release()
	}
	ok, reason := a.execute(ctx, as, m, lg)
	a.finish(as, lg, ok, reason)
}

func (a *Agent) execute(ctx context.Context, as *assignment, m protocol.Start, lg *slog.Logger) (bool, string) {

	if !a.o.Executor.Ready(ctx, m.Image) {
		lg.Info("preparing image before start")
		if err := a.o.Executor.Prepare(ctx, m.Image); err != nil {
			return false, fmt.Sprintf("preparing image: %v", err)
		}
		a.mu.Lock()
		a.ready[m.Image.Name] = true
		a.mu.Unlock()
	}

	opts := executor.StartOptions{
		Resources:      a.o.Resources,
		IsolateNetwork: a.o.Policy.NetworkIsolation() && a.isolationOK,
	}
	opts.SharedDirs = map[string]executor.SharedDir{}
	if a.o.Policy.SharedRunnerEnabled() && a.o.Runner != nil {
		if dir := a.o.Runner.Current(guestPlatform(m.Image)); dir != "" {
			opts.SharedDirs[runnerShare] = executor.SharedDir{Path: dir}
		}
	}
	if a.o.Cache != nil {
		// A job that cannot be placed in a cache runs without one rather than sharing
		// someone else's.
		if dir, release, err := a.jobCache(m); err != nil {
			lg.Warn("running without a cache", "err", err)
		} else {
			opts.SharedDirs[cacheShare] = executor.SharedDir{Path: dir, Writable: true}
			// Registered before the VM's own cleanup, so it runs after the VM is gone.
			defer func() {
				release()
				go a.o.Cache.Prune()
			}()
		}
	}
	started := time.Now()
	h, err := a.o.Executor.Start(ctx, m.AssignmentID, m.Image, opts)
	if err != nil {
		return false, fmt.Sprintf("starting VM: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := a.o.Executor.Stop(stopCtx, h); err != nil {
			lg.Error("deleting VM failed", "vm", h.Name, "err", err)
		} else {
			lg.Info("VM deleted", "vm", h.Name)
		}
	}()
	lg.Info("VM started", "vm", h.Name, "boot", time.Since(started).Round(time.Millisecond))

	var busy atomic.Bool
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, jobTakenMarker) && !busy.Swap(true) {
				lg.Info("runner took a job")
			}
			lg.Debug("runner", "line", line)
		}
	}()

	// Stop a runner that never gets a job, even if the control plane is unreachable.
	if t := time.Duration(m.Workload.IdleTimeoutSeconds) * time.Second; t > 0 {
		timer := time.AfterFunc(t, func() {
			if !busy.Load() {
				a.mu.Lock()
				as.stopReason = "idle timeout"
				a.mu.Unlock()
				as.cancel()
			}
		})
		defer timer.Stop()
	}

	a.setState(as.id, protocol.AgentAssignmentStateRunning, "")
	code, err := a.o.Executor.Exec(ctx, h, executor.Command{
		Script: entryScript(m.Image),
		Stdin:  strings.NewReader(m.Workload.JITConfig + "\n"),
		Output: pw,
	})
	pw.Close()

	switch {
	case a.stopReason(as) != "":
		return true, a.stopReason(as)
	case err != nil:
		return busy.Load(), fmt.Sprintf("running workload: %v", err)
	case code != 0 && !busy.Load():
		return false, fmt.Sprintf("runner exited with code %d before taking a job", code)
	default:
		return true, ""
	}
}

func (a *Agent) finish(as *assignment, lg interface{ Info(string, ...any) }, ok bool, reason string) {
	state := protocol.AgentAssignmentStateFinished
	if !ok {
		state = protocol.AgentAssignmentStateFailed
	}
	lg.Info("assignment ended", "state", state, "reason", reason)
	a.setState(as.id, state, reason)
}
