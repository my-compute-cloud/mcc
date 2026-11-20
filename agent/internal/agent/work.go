package agent

import (
	"bufio"
	"cmp"
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
	"sync"
	"sync/atomic"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
	"github.com/my-compute-cloud/mcc/agent/internal/tasklog"
)

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runnerShare is the share name of the runner directory. macOS guests have shares at
// /Volumes/My Shared Files/<name>, Linux guests at executor.LinuxSharesDir/<name>; how they
// get there is the executor's business.
const runnerShare = "mcc-runner"

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
  # Without extended attributes (-X): the share cannot give those of a symbolic link, and
  # cp fails on each one it cannot copy. The runner needs none of them.
  cp -RX "$SHARED" "$RUNNER.mcc"
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
SHARES="/Volumes/My Shared Files"
SHARES_URL="/Volumes/My%20Shared%20Files"
` + nixSetup + `if [ -r /etc/mcc/job-env ]; then
  . /etc/mcc/job-env
fi
cd "$RUNNER"
# Not exec: when the job has ended, what it fetched is kept for the next one.
rc=0
./run.sh --jitconfig "$JIT" || rc=$?
mcc_nix_keep || true
exit $rc
`

// githubRunnerScriptLinux is the same for Linux guests.
const githubRunnerScriptLinux = `set -euo pipefail
IFS= read -r JIT
SHARES="` + executor.LinuxSharesDir + `"
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
SHARES_URL="$SHARES"
` + nixSetup + `if [ -r /etc/mcc/job-env ]; then
  . /etc/mcc/job-env
fi
cd "$RUNNER"
# Not exec: when the job has ended, what it fetched is kept for the next one.
rc=0
./run.sh --jitconfig "$JIT" || rc=$?
mcc_nix_keep || true
exit $rc
`

// hostRunnerScript is the fixed entry command for a runner on the host itself. The executor
// has copied the host's runner into the work's directory. The JIT configuration goes to the
// runner in its environment, which the runner reads and clears, and not on its command line,
// where every account on the host could read it.
const hostRunnerScript = `set -euo pipefail
IFS= read -r JIT
cd "$MCC_JOB_DIR/` + runnerShare + `"
export ACTIONS_RUNNER_INPUT_JITCONFIG="$JIT"
unset JIT
exec ./run.sh
`

// HostEntryScript is the fixed entry command of the github-runner workload on the host itself.
func HostEntryScript() string { return hostRunnerScript }

// EntryScript is the fixed entry command of the github-runner workload for an image's guest
// platform, for tests that run it in a real VM.
func EntryScript(image protocol.ImageSpec) string { return entryScript(image) }

// entryScript is the fixed entry command for an image's guest platform.
func entryScript(image protocol.ImageSpec) string {
	if guestPlatform(image) == "linux" {
		return githubRunnerScriptLinux
	}
	return githubRunnerScript
}

// The runner prints this when it accepts a job.
const jobTakenMarker = "Running job:"

// Codes that tell the control plane what kind of refusal a failed start was.
const (
	// The owner's rules forbid it; trying again will not help.
	codeRefusedPolicy = "refused_policy"
	// There is no room just now.
	codeRefusedCapacity = "refused_capacity"
	// A task ran past its time limit, or was stopped before it ended.
	codeTimeout = "timeout"
	codeStopped = "stopped"
)

const (
	workloadGitHubRunner = "github-runner"
	workloadTask         = "task"
	workloadScript       = "script"
)

// scriptEntry is the fixed entry command of a script. The script itself arrives on stdin, so
// that it is never an argument on the host: an argument can be read by every account there,
// and a caller's script may hold what the caller would not want read.
const scriptEntry = "exec /bin/bash -s\n"

// policyCheck returns why a start must be refused, with its code, or "".
func (a *Agent) policyCheck(m protocol.Start) (reason, code string) {
	if m.Isolation == protocol.IsolationHost {
		return a.hostPolicyCheck(m)
	}
	p := a.o.Policy
	if !p.Active(a.o.Now()) {
		return "outside active hours", codeRefusedCapacity
	}
	if p.RequireNetworkIsolation && !a.isolationOK {
		return "network isolation required by policy but unavailable", codeRefusedPolicy
	}
	if !p.ImageAllowed(m.Image.Source) {
		return fmt.Sprintf("image %s not allowed by local policy", m.Image.Source), codeRefusedPolicy
	}
	a.mu.Lock()
	known, ok := a.images[m.Image.Name]
	a.mu.Unlock()
	if !ok || known.Source != m.Image.Source {
		return fmt.Sprintf("image %s is not in this host's pool configuration", m.Image.Name), codeRefusedPolicy
	}
	switch m.Workload.Kind {
	case workloadGitHubRunner:
		if !p.WorkloadAllowed(workloadGitHubRunner) {
			return "GitHub Actions jobs are not allowed on this host (allowed_workloads in its policy)", codeRefusedPolicy
		}
		if len(p.AllowedGitHubURLs) > 0 {
			r, err := jitRunner(m.Workload.JITConfig)
			if err != nil {
				return "could not read runner configuration to check allowed_github_urls", codeRefusedPolicy
			}
			if !p.GitHubURLAllowed(r.GitHubURL) {
				return fmt.Sprintf("runner for %s not allowed by local policy", r.GitHubURL), codeRefusedPolicy
			}
		}
	case workloadScript:
		// The one kind of work whose command the owner did not write. It is confined to a VM,
		// and still needs the owner's yes.
		if !p.WorkloadAllowed(workloadScript) {
			return "scripts are not allowed on this host (allowed_workloads in its policy)", codeRefusedPolicy
		}
		if strings.TrimSpace(m.Workload.Script) == "" {
			return "the script is empty", codeRefusedPolicy
		}
	case workloadTask:
		return "a declared task runs on the host itself, not in a VM", codeRefusedPolicy
	default:
		return fmt.Sprintf("unsupported workload %q", m.Workload.Kind), codeRefusedPolicy
	}
	if a.activeCount() >= a.slotsTotal() {
		return "no free slot", codeRefusedCapacity
	}
	platform := guestPlatform(m.Image)
	if !slices.Contains(a.o.Executor.GuestPlatforms(), platform) {
		return fmt.Sprintf("this host cannot run %s guests", platform), codeRefusedPolicy
	}
	if lim := a.guestLimit(platform); lim > 0 && a.activeOn(platform) >= lim {
		return fmt.Sprintf("this host already runs %d %s guests, the most allowed", lim, platform), codeRefusedCapacity
	}
	return "", ""
}

// hostPolicyCheck is policyCheck for work that runs on the host itself. Such work is not
// isolated from the host, so each thing it needs is something the owner said yes to.
func (a *Agent) hostPolicyCheck(m protocol.Start) (reason, code string) {
	p := a.o.Policy
	if a.o.HostExecutor == nil || !p.HostTasks.Enabled {
		return "this host does not run work without a VM (host_tasks in its policy)", codeRefusedPolicy
	}
	if p.RequireNetworkIsolation {
		return "network isolation is required by policy, and work on the host itself has none", codeRefusedPolicy
	}
	if !p.Active(a.o.Now()) {
		return "outside active hours", codeRefusedCapacity
	}
	if m.Workload.Kind == workloadTask {
		return a.taskPolicyCheck(m.Workload)
	}
	if m.Workload.Kind == workloadScript {
		return "a script runs only in a VM, never on the host itself", codeRefusedPolicy
	}
	if m.Workload.Kind != workloadGitHubRunner {
		return fmt.Sprintf("unsupported workload %q", m.Workload.Kind), codeRefusedPolicy
	}
	if !p.HostTasks.GitHubRunner {
		return "GitHub Actions jobs may not run on this host itself (host_tasks.github_runner in its policy)", codeRefusedPolicy
	}
	r, err := jitRunner(m.Workload.JITConfig)
	if err != nil {
		return "could not read runner configuration to check allowed_github_urls", codeRefusedPolicy
	}
	// An empty list allows anything in a VM. Here it allows nothing.
	if len(p.AllowedGitHubURLs) == 0 || !p.GitHubURLAllowed(r.GitHubURL) {
		return fmt.Sprintf("runner for %s not allowed by local policy", r.GitHubURL), codeRefusedPolicy
	}
	// The control plane chooses the runner's work folder. On the host it stays inside the
	// work's own directory.
	if r.WorkFolder != "" && r.WorkFolder != "_work" {
		return fmt.Sprintf("runner work folder %q is not allowed on the host", r.WorkFolder), codeRefusedPolicy
	}
	if a.hostRunner() == "" {
		return "this host has no Actions runner yet", codeRefusedCapacity
	}
	if a.hostActive() >= a.hostSlots() {
		return "no free host slot", codeRefusedCapacity
	}
	return "", ""
}

// taskPolicyCheck is the rest of hostPolicyCheck for a task. The control plane sends a name
// and parameters; whether there is such a task, and what its parameters may be, is the owner's
// policy alone.
func (a *Agent) taskPolicyCheck(w protocol.Workload) (reason, code string) {
	spec, ok := a.o.Policy.Tasks[w.Task]
	if !ok {
		return fmt.Sprintf("task %q is not declared in this host's policy", w.Task), codeRefusedPolicy
	}
	params, err := taskParams(w.Params)
	if err == nil {
		err = spec.CheckParams(params)
	}
	if err != nil {
		return fmt.Sprintf("task %s: %v", w.Task, err), codeRefusedPolicy
	}
	if a.hostActive() >= a.hostSlots() {
		return "no free host slot", codeRefusedCapacity
	}
	if spec.MaxConcurrent > 0 && a.activeTask(w.Task) >= spec.MaxConcurrent {
		return fmt.Sprintf("task %s already has %d runs, the most allowed", w.Task, spec.MaxConcurrent), codeRefusedCapacity
	}
	return "", ""
}

// taskParams turns the parameters of a request into a map, refusing one named twice.
func taskParams(list []protocol.TaskParam) (map[string]string, error) {
	out := make(map[string]string, len(list))
	for _, p := range list {
		if _, dup := out[p.Key]; dup {
			return nil, fmt.Errorf("parameter %s is given twice", p.Key)
		}
		out[p.Key] = p.Value
	}
	return out, nil
}

// taskScript is the entry command of a task: the owner's command, each word quoted so that
// the shell the executor runs passes it on exactly as written.
func taskScript(command []string) string {
	words := make([]string, len(command))
	for i, w := range command {
		words[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return "exec " + strings.Join(words, " ") + "\n"
}

// executeTask runs a declared task on the host itself, in a directory made for it and gone
// afterwards. The run ends at the sooner of the owner's time limit and the request's.
func (a *Agent) executeTask(ctx context.Context, as *assignment, m protocol.Start, lg *slog.Logger) (bool, string) {
	spec := a.o.Policy.Tasks[m.Workload.Task]
	params, _ := taskParams(m.Workload.Params)
	env := []string{"MCC_TASK=" + m.Workload.Task}
	for k, v := range params {
		env = append(env, "MCC_PARAM_"+k+"="+v)
	}
	slices.Sort(env)

	ex := a.o.HostExecutor
	h, err := ex.Start(ctx, m.AssignmentID, protocol.ImageSpec{}, executor.StartOptions{})
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := ex.Stop(stopCtx, h); err != nil {
			lg.Error("cleaning up after the task failed", "name", h.Name, "err", err)
		}
	}()
	if err != nil {
		return false, fmt.Sprintf("preparing the task's directory: %v", err)
	}

	return a.runToExit(ctx, as, m, lg, ex, h, executor.Command{Script: taskScript(spec.Command), Env: env}, spec.Timeout())
}

// runToExit runs a task's or a script's command where the executor has made room for it, until
// it exits, is stopped, or reaches the sooner of the owner's time limit and the request's.
func (a *Agent) runToExit(ctx context.Context, as *assignment, m protocol.Start, lg *slog.Logger, ex executor.Executor, h executor.Handle, cmd executor.Command, limit time.Duration) (bool, string) {
	if t := time.Duration(m.Workload.TimeoutSeconds) * time.Second; t > 0 && t < limit {
		limit = t
	}
	tctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	// What the task prints stays on this host: in a file a caller can read from here, if the
	// owner keeps any. None of it goes to the control plane.
	var out io.Writer = io.Discard
	var kept *tasklog.Log
	if a.o.TaskOutput != nil {
		id := m.Workload.TaskID
		if id == "" {
			id = m.AssignmentID
		}
		l, err := a.o.TaskOutput.Create(tasklog.Meta{ID: id, AssignmentID: m.AssignmentID, Task: cmp.Or(m.Workload.Task, m.Workload.Kind), SecretHash: m.Workload.OutputSecretHash})
		if err != nil {
			lg.Warn("the task's output will not be kept", "err", err)
		} else {
			out, kept = l, l
			go a.o.TaskOutput.Prune()
		}
	}
	end := func(exitCode *int, code string) {
		if kept == nil {
			return
		}
		kept.Close(exitCode, code)
		// Kept by the control plane too, if the tenant's plan asks and this host's owner agrees.
		if m.Workload.StoreOutput && a.o.Policy.TaskOutput.Upload && m.Workload.TaskID != "" {
			a.wg.Add(1)
			go func() {
				defer a.wg.Done()
				a.uploadOutput(m.Workload.TaskID, lg)
			}()
		}
	}

	a.setState(as.id, protocol.AgentAssignmentStateRunning, "")
	cmd.Output = out
	code, err := ex.Exec(tctx, h, cmd)

	switch {
	case a.stopReason(as) != "":
		a.setOutcome(as, codeStopped, 0)
		end(nil, codeStopped)
		return false, a.stopReason(as)
	case tctx.Err() == context.DeadlineExceeded:
		a.setOutcome(as, codeTimeout, 0)
		end(nil, codeTimeout)
		return false, fmt.Sprintf("stopped after its time limit of %s", limit)
	case err != nil:
		end(nil, "failed")
		return false, fmt.Sprintf("running task: %v", err)
	}
	// It ran to an exit. That is "finished" whatever the exit code, which goes with it.
	a.setOutcome(as, "", code)
	end(&code, "")
	if code != 0 {
		return true, fmt.Sprintf("exited with code %d", code)
	}
	return true, ""
}

// setOutcome records how a task ended, to go out with its last status.
func (a *Agent) setOutcome(as *assignment, code string, exitCode int) {
	a.mu.Lock()
	as.code, as.exitCode = code, exitCode
	a.mu.Unlock()
}

// runnerSettings is what the agent reads of a just-in-time runner configuration.
type runnerSettings struct {
	// GitHubURL is the repository or organization the runner registers with.
	GitHubURL  string `json:"gitHubUrl"`
	WorkFolder string `json:"workFolder"`
}

// jitRunner reads the runner's settings from a just-in-time runner configuration: base64 JSON
// mapping runner file names to base64 file contents.
func jitRunner(encoded string) (runnerSettings, error) {
	var settings runnerSettings
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return settings, err
	}
	var files map[string]string
	if err := json.Unmarshal(raw, &files); err != nil {
		return settings, err
	}
	runner, err := base64.StdEncoding.DecodeString(files[".runner"])
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(runner), "\uFEFF")), &settings); err != nil {
		return settings, err
	}
	if settings.GitHubURL == "" {
		return settings, fmt.Errorf("no gitHubUrl")
	}
	return settings, nil
}

// jobCache is the cache directory for the repository or organization this job's runner
// registers with.
func (a *Agent) jobCache(m protocol.Start) (string, func(), error) {
	r, err := jitRunner(m.Workload.JITConfig)
	if err != nil {
		return "", nil, fmt.Errorf("could not read the runner configuration to choose a cache: %w", err)
	}
	return a.o.Cache.Acquire(r.GitHubURL)
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

	// A start that was already on its way when the agent began to drain. Refused for want of
	// capacity, which is the refusal the control plane takes elsewhere.
	if why := a.Draining(); why != "" {
		a.log.Info("refusing start: draining", "assignment", m.AssignmentID, "why", why)
		a.send(&protocol.Status{Type: protocol.TypeStatus, AssignmentID: m.AssignmentID, State: protocol.AgentAssignmentStateFailed, Reason: "refused: this host is draining (" + why + ")", Code: codeRefusedCapacity})
		return
	}

	if reason, code := a.policyCheck(m); reason != "" {
		a.log.Warn("refusing start", "assignment", m.AssignmentID, "reason", reason)
		a.send(&protocol.Status{Type: protocol.TypeStatus, AssignmentID: m.AssignmentID, State: protocol.AgentAssignmentStateFailed, Reason: "refused: " + reason, Code: code})
		return
	}

	wctx, cancel := context.WithCancel(ctx)
	as := &assignment{id: m.AssignmentID, state: protocol.AgentAssignmentStateStarting, cancel: cancel}
	if m.Isolation == protocol.IsolationHost {
		as.host = true
		if m.Workload.Kind == workloadTask {
			as.task = m.Workload.Task
		}
	} else {
		as.platform = guestPlatform(m.Image)
		as.script = m.Workload.Kind == workloadScript
	}
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
// VM has been deleted, or the work's directory on the host.
func (a *Agent) runAssignment(ctx context.Context, as *assignment, m protocol.Start) {
	lg := a.log.With("assignment", m.AssignmentID)
	if as.host {
		lg = lg.With("isolation", "host")
	} else {
		lg = lg.With("image", m.Image.Name)
	}
	a.setState(as.id, protocol.AgentAssignmentStateStarting, "")
	if a.o.Policy.StayAwakeEnabled() {
		a.awake.Acquire()
		defer a.awake.Release()
	}
	var ok bool
	var reason string
	if as.task != "" {
		lg = lg.With("task", as.task)
		ok, reason = a.executeTask(ctx, as, m, lg)
	} else if as.host {
		ok, reason = a.executeOnHost(ctx, as, m, lg)
	} else {
		ok, reason = a.execute(ctx, as, m, lg)
	}
	a.finish(as, lg, ok, reason)
}

// executeOnHost runs a runner on the host itself: in a directory made for it, holding a copy
// of the host's runner, both gone afterwards.
func (a *Agent) executeOnHost(ctx context.Context, as *assignment, m protocol.Start, lg *slog.Logger) (bool, string) {
	ex := a.o.HostExecutor
	runner := a.hostRunner()
	if runner == "" {
		return false, "this host has no Actions runner yet"
	}
	opts := executor.StartOptions{SharedDirs: map[string]executor.SharedDir{runnerShare: {Path: runner}}}
	if a.o.Policy.HostTasks.Persistent() {
		// The runner is new for every job; only its work folder is kept, for the repository or
		// organization the runner registers with and no one else. Downloaded actions and
		// temporary files are not kept: the next job would run a cached action as it found it.
		r, err := jitRunner(m.Workload.JITConfig)
		if err != nil {
			return false, "could not read the runner configuration to choose a workspace"
		}
		opts.KeptDirs = []executor.KeptDir{{
			Link: runnerShare + "/_work",
			Key:  strings.TrimPrefix(strings.ToLower(r.GitHubURL), "https://"),
			Wipe: []string{"_actions", "_temp"},
		}}
	}
	var env []string
	if a.o.Cache != nil {
		if dir, release, err := a.jobCache(m); err != nil {
			lg.Warn("running without a cache", "err", err)
		} else {
			env = append(env, "MCC_CACHE_DIR="+dir)
			defer func() {
				release()
				go a.o.Cache.Prune()
			}()
		}
	}
	h, err := ex.Start(ctx, m.AssignmentID, protocol.ImageSpec{}, opts)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := ex.Stop(stopCtx, h); err != nil {
			lg.Error("cleaning up after the work failed", "name", h.Name, "err", err)
		} else {
			lg.Info("work directory deleted", "name", h.Name)
		}
	}()
	if err != nil {
		return false, fmt.Sprintf("preparing the work's directory: %v", err)
	}
	return a.runRunner(ctx, as, m, lg, ex, h, hostRunnerScript, env)
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
	// A script is not a GitHub job: it gets no runner and no job cache, only the VM.
	runner := m.Workload.Kind == workloadGitHubRunner
	if runner && a.o.Policy.SharedRunnerEnabled() && a.o.Runner != nil {
		if dir := a.o.Runner.Current(guestPlatform(m.Image)); dir != "" {
			opts.SharedDirs[runnerShare] = executor.SharedDir{Path: dir}
		}
	}
	if runner && a.o.Cache != nil {
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
	if runner && a.o.HostNix != nil {
		// A job that cannot have the host's store runs without it, as on any other host.
		if dir, remove, err := a.o.HostNix.Snapshot(ctx, m.AssignmentID); err != nil {
			lg.Warn("running without the host's Nix store", "err", err)
		} else {
			opts.SharedDirs[nixStoreShare] = executor.SharedDir{Path: a.o.HostNix.Store()}
			opts.SharedDirs[nixStateShare] = executor.SharedDir{Path: dir}
			defer remove()
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

	if !runner {
		return a.runToExit(ctx, as, m, lg, a.o.Executor, h,
			executor.Command{Script: scriptEntry, Stdin: strings.NewReader(m.Workload.Script)}, a.o.Policy.ScriptTimeout())
	}
	return a.runRunner(ctx, as, m, lg, a.o.Executor, h, entryScript(m.Image), nil)
}

// runRunner runs the runner's entry command where the executor has made room for it, and
// watches its output for the job being taken.
func (a *Agent) runRunner(ctx context.Context, as *assignment, m protocol.Start, lg *slog.Logger, ex executor.Executor, h executor.Handle, script string, env []string) (bool, string) {
	var busy atomic.Bool
	// What the runner said last before any job, kept to explain a runner that never took one.
	var last lastLines
	read := make(chan struct{})
	pr, pw := io.Pipe()
	go func() {
		defer close(read)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, jobTakenMarker) && !busy.Swap(true) {
				lg.Info("runner took a job")
			}
			if !busy.Load() {
				last.add(line)
			}
			lg.Debug("runner", "line", line)
		}
		// A line too long to scan must not leave the workload blocked on a full pipe.
		_, _ = io.Copy(io.Discard, pr)
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
	code, err := ex.Exec(ctx, h, executor.Command{
		Script: script,
		Env:    env,
		Stdin:  strings.NewReader(m.Workload.JITConfig + "\n"),
		Output: pw,
	})
	pw.Close()
	<-read

	switch {
	case a.stopReason(as) != "":
		return true, a.stopReason(as)
	case err != nil:
		return busy.Load(), fmt.Sprintf("running workload: %v", err)
	case code != 0 && !busy.Load():
		// Nothing of a job's is in this: no job was taken. It is the runner's own words, and
		// without them the failure says nothing anyone can act on.
		said := last.String()
		lg.Warn("the runner exited before taking a job", "code", code, "said", said)
		if said == "" {
			return false, fmt.Sprintf("runner exited with code %d before taking a job", code)
		}
		return false, fmt.Sprintf("runner exited with code %d before taking a job; it said: %s", code, said)
	default:
		return true, ""
	}
}

// lastLines keeps the last few lines of output that say anything.
type lastLines struct {
	mu    sync.Mutex
	lines []string
}

const (
	keptLines     = 4
	keptLineBytes = 240
)

func (l *lastLines) add(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if len(line) > keptLineBytes {
		line = line[:keptLineBytes] + "..."
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
	if len(l.lines) > keptLines {
		l.lines = l.lines[len(l.lines)-keptLines:]
	}
}

func (l *lastLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, " | ")
}

func (a *Agent) finish(as *assignment, lg interface{ Info(string, ...any) }, ok bool, reason string) {
	state := protocol.AgentAssignmentStateFinished
	if !ok {
		state = protocol.AgentAssignmentStateFailed
	}
	lg.Info("assignment ended", "state", state, "reason", reason)
	a.setState(as.id, state, reason)
}
