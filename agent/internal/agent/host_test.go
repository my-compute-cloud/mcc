package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/process"
	"github.com/my-compute-cloud/mcc/agent/internal/identity"
	"github.com/my-compute-cloud/mcc/agent/internal/tasklog"
)

// oneRunner is a RunnerSource with the same runner for every platform.
type oneRunner string

func (r oneRunner) Current(string) string { return string(r) }

const acme = "https://github.com/acme"

// hostPolicy is a policy under which GitHub jobs from acme may run on the host itself.
func hostPolicy() config.Policy {
	p := config.DefaultPolicy()
	p.AllowedGitHubURLs = []string{acme}
	p.HostTasks = config.HostTasks{Enabled: true, GitHubRunner: true, AllowSameUser: true}
	return p
}

// stubRunner is a directory holding a run.sh that behaves as a runner does, as far as the
// agent can tell: it says it took a job, and records how it was started.
func stubRunner(t *testing.T, record string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/bash\n" +
		`{ echo "jit=$ACTIONS_RUNNER_INPUT_JITCONFIG"; echo "args=$*"; echo "pwd=$PWD"; echo "cache=${MCC_CACHE_DIR:-}"; } > "` + record + `"` + "\n" +
		`echo "Running job: build"` + "\n" +
		`mkdir -p _work && touch _work/checkout` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func startHostAgent(t *testing.T, policy config.Policy, runner RunnerSource, with ...func(*Options)) (*controlPlane, *fake.Fake, *process.Process) {
	t.Helper()
	id := identity.FromSeed(make([]byte, 32))
	cp := newControlPlane(t, id)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	px := process.New(filepath.Join(root, "work"), filepath.Join(root, "state"), "", log)
	px.Grace = 200 * time.Millisecond
	f := fake.New()
	o := Options{
		ServerURL: cp.srv.URL, HostID: "host-1", Identity: id, Policy: policy, Executor: f, Runner: runner, Log: log,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
	}
	if policy.HostTasks.Enabled {
		o.HostExecutor = px
	}
	for _, f := range with {
		f(&o)
	}
	a := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = a.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cp, f, px
}

func hostStart(id, jit string) map[string]any {
	return map[string]any{
		"type": "start", "assignment_id": id, "isolation": "host",
		"workload": map[string]any{"kind": "github-runner", "runner_name": "mcc-" + id, "jit_config": jit},
	}
}

func TestAJobRunsOnTheHostItselfAndLeavesNothing(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	cp, f, px := startHostAgent(t, hostPolicy(), oneRunner(stubRunner(t, record)))
	<-cp.hellos
	capacity := cp.next(isType("capacity"))
	if capacity["host_slots_total"] != float64(1) || capacity["host_slots_free"] != float64(1) {
		t.Fatalf("capacity %v", capacity)
	}

	jit := jitFor(acme)
	cp.send(hostStart("h1", jit))
	cp.next(status("h1", "running"))
	cp.next(status("h1", "finished"))

	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	// The configuration reaches the runner in its environment, never on its command line.
	if !strings.Contains(got, "jit="+jit) || !strings.Contains(got, "args=\n") {
		t.Errorf("runner was started with:\n%s", got)
	}
	if !strings.Contains(got, filepath.Join("job-h1", "mcc-runner")) {
		t.Errorf("runner did not run from a copy in the job's directory:\n%s", got)
	}
	if started, _ := f.Snapshot(); len(started) != 0 {
		t.Errorf("a VM was started for work on the host: %v", started)
	}
	if entries, _ := os.ReadDir(px.Dir); len(entries) != 0 {
		t.Errorf("the job's directory was not deleted: %v", entries)
	}
}

func TestHostWorkIsRefusedUnlessTheOwnerAllowsIt(t *testing.T) {
	workFolder := func(folder string) string {
		runner := base64.StdEncoding.EncodeToString([]byte(`{"gitHubUrl":"` + acme + `","workFolder":"` + folder + `"}`))
		files, _ := json.Marshal(map[string]string{".runner": runner})
		return base64.StdEncoding.EncodeToString(files)
	}
	cases := []struct {
		name   string
		policy func(*config.Policy)
		runner RunnerSource
		jit    string
		want   string
		code   string
	}{
		{"not turned on", func(p *config.Policy) { p.HostTasks.Enabled = false }, oneRunner("x"), jitFor(acme), "does not run work without a VM", "refused_policy"},
		{"GitHub jobs not allowed", func(p *config.Policy) { p.HostTasks.GitHubRunner = false }, oneRunner("x"), jitFor(acme), "host_tasks.github_runner", "refused_policy"},
		{"someone else's repository", nil, oneRunner("x"), jitFor("https://github.com/other"), "not allowed by local policy", "refused_policy"},
		{"no list of whose workflows", func(p *config.Policy) { p.AllowedGitHubURLs = nil }, oneRunner("x"), jitFor(acme), "not allowed by local policy", "refused_policy"},
		{"a work folder outside the job", nil, oneRunner("x"), workFolder("/Users/someone"), "work folder", "refused_policy"},
		{"network isolation required", func(p *config.Policy) { p.RequireNetworkIsolation = true }, oneRunner("x"), jitFor(acme), "network isolation", "refused_policy"},
		{"no runner yet", nil, oneRunner(""), jitFor(acme), "no Actions runner", "refused_capacity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			policy := hostPolicy()
			if c.policy != nil {
				c.policy(&policy)
			}
			cp, _, px := startHostAgent(t, policy, c.runner)
			<-cp.hellos
			cp.send(hostStart("r1", c.jit))
			m := cp.next(status("r1", "failed"))
			if !strings.Contains(m["reason"].(string), c.want) || m["code"] != c.code {
				t.Fatalf("refusal %v", m)
			}
			if entries, _ := os.ReadDir(px.Dir); len(entries) != 0 {
				t.Errorf("refused work was given a directory: %v", entries)
			}
		})
	}
}

func TestHostSlotsAreSeparateFromVMSlots(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	dir := stubRunner(t, record)
	// A runner that waits, so its slot stays taken.
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/bash\necho 'Running job: build'\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := hostPolicy()
	policy.MaxVMs = 1
	cp, f, _ := startHostAgent(t, policy, oneRunner(dir))
	f.Block = true
	<-cp.hellos
	cp.next(isType("capacity"))

	cp.send(hostStart("s1", jitFor(acme)))
	cp.next(status("s1", "running"))
	// The one host slot is taken; the one VM slot is not.
	cp.send(hostStart("s2", jitFor(acme)))
	if m := cp.next(status("s2", "failed")); !strings.Contains(m["reason"].(string), "no free host slot") || m["code"] != "refused_capacity" {
		t.Fatalf("refusal %v", m)
	}
	vm := startMsg("v1")
	vm["workload"].(map[string]any)["jit_config"] = jitFor(acme)
	cp.send(vm)
	cp.next(status("v1", "running"))

	cp.send(map[string]any{"type": "stop", "assignment_id": "s1"})
	cp.next(status("s1", "finished"))
}

func TestHostTasksPolicyNeedsAnAccountAndWhoseWorkflows(t *testing.T) {
	bad := []config.HostTasks{
		{Enabled: true}, // neither user nor allow_same_user
		{Enabled: true, User: "ci", AllowSameUser: true, Dir: "/w"}, // both
		{Enabled: true, User: "-rf", Dir: "/w"},                     // not an account name
		{Enabled: true, User: "ci"},                                 // no directory that user can write to
		{Enabled: true, AllowSameUser: true, Dir: "relative"},       // not a full path
		{Enabled: true, AllowSameUser: true, Workspace: "shared"},   // not a workspace
	}
	for _, h := range bad {
		p := config.DefaultPolicy()
		p.HostTasks = h
		if err := p.Validate(); err == nil {
			t.Errorf("policy %+v should be refused", h)
		}
	}
	p := config.DefaultPolicy()
	p.HostTasks = config.HostTasks{Enabled: true, AllowSameUser: true, GitHubRunner: true}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_github_urls") {
		t.Errorf("github_runner without allowed_github_urls: %v", err)
	}
	p.AllowedGitHubURLs = []string{acme}
	if err := p.Validate(); err != nil {
		t.Errorf("a complete policy was refused: %v", err)
	}
	if got := (config.HostTasks{}).Slots(); got != 0 {
		t.Errorf("host slots without host_tasks: %d", got)
	}
}

func TestAPersistentWorkspaceIsKeptForTheSameRepositoryOnly(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	dir := t.TempDir()
	// A runner that reports what it found in its work folder, then leaves a checkout and a
	// downloaded action behind.
	script := "#!/bin/bash\n" +
		`ls -A _work > "` + record + `"` + "\n" +
		`echo "Running job: build"` + "\n" +
		`mkdir -p _work/_actions && touch _work/checkout _work/_actions/cached` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := hostPolicy()
	policy.AllowedGitHubURLs = []string{acme + "/app", acme + "/other"}
	policy.HostTasks.Workspace = "persistent"
	cp, _, px := startHostAgent(t, policy, oneRunner(dir))
	<-cp.hellos

	found := func(id, repo string) string {
		cp.send(hostStart(id, jitFor(repo)))
		cp.next(status(id, "finished"))
		b, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := found("p1", acme+"/app"); got != "" {
		t.Fatalf("the first job found %q", got)
	}
	if got := found("p2", acme+"/app"); got != "checkout" {
		t.Fatalf("the repository's next job found %q, want its checkout and no cached action", got)
	}
	if got := found("p3", acme+"/other"); got != "" {
		t.Fatalf("another repository's job found %q", got)
	}
	// Each job's own directory, with its copy of the runner, is still deleted.
	entries, _ := os.ReadDir(px.Dir)
	if len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatalf("left in the work directory: %v", entries)
	}
}

// taskPolicy is a policy that declares one task, "build", which records how it was run.
func taskPolicy(t *testing.T, record string, body string) config.Policy {
	t.Helper()
	script := filepath.Join(t.TempDir(), "build.sh")
	content := "#!/bin/bash\n" +
		`{ echo "args=$*"; echo "scheme=${MCC_PARAM_SCHEME:-}"; echo "task=$MCC_TASK"; echo "pwd=$PWD"; echo "key=${MCC_SECRET:-}"; } > "` + record + `"` + "\n" + body + "\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	p := config.DefaultPolicy()
	p.HostTasks = config.HostTasks{Enabled: true, AllowSameUser: true}
	p.Tasks = map[string]config.Task{"build": {
		Command: []string{script, "it's", "$HOME; echo no"},
		Params:  map[string]string{"SCHEME": "[A-Za-z0-9_-]{1,40}"},
	}}
	return p
}

func taskStart(id, task string, params map[string]string, timeout int) map[string]any {
	list := []map[string]any{}
	for k, v := range params {
		list = append(list, map[string]any{"key": k, "value": v})
	}
	w := map[string]any{"kind": "task", "task": task, "params": list}
	if timeout > 0 {
		w["timeout_seconds"] = timeout
	}
	return map[string]any{"type": "start", "assignment_id": id, "isolation": "host", "workload": w}
}

func TestADeclaredTaskRunsAsItsOwnerWroteIt(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	t.Setenv("MCC_SECRET", "the agent's own")
	// No Actions runner on this host: tasks do not need one.
	cp, _, px := startHostAgent(t, taskPolicy(t, record, "exit 0"), oneRunner(""))
	<-cp.hellos
	capacity := cp.next(isType("capacity"))
	if capacity["host_slots_free"] != float64(1) {
		t.Fatalf("capacity %v", capacity)
	}
	tasks, _ := capacity["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["name"] != "build" || tasks[0].(map[string]any)["timeout_seconds"] != float64(3600) {
		t.Fatalf("declared tasks %v", capacity["tasks"])
	}

	cp.send(taskStart("t1", "build", map[string]string{"SCHEME": "App"}, 0))
	cp.next(status("t1", "running"))
	m := cp.next(status("t1", "finished"))
	if _, has := m["exit_code"]; has || m["code"] != nil {
		t.Errorf("a task that exited 0 reported %v", m)
	}
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	// The arguments are the owner's, word for word; the parameter is in the environment only.
	for _, want := range []string{"args=it's $HOME; echo no\n", "scheme=App\n", "task=build\n", filepath.Join(px.Dir, "job-t1") + "\n", "key=\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
	if entries, _ := os.ReadDir(px.Dir); len(entries) != 0 {
		t.Errorf("the task's directory was not deleted: %v", entries)
	}
}

func TestATaskReportsItsExitCode(t *testing.T) {
	cp, _, _ := startHostAgent(t, taskPolicy(t, filepath.Join(t.TempDir(), "r"), "exit 3"), oneRunner(""))
	<-cp.hellos
	cp.send(taskStart("t1", "build", nil, 0))
	m := cp.next(status("t1", "finished"))
	if m["exit_code"] != float64(3) || m["reason"] != "exited with code 3" {
		t.Fatalf("status %v", m)
	}
}

func TestATaskIsRefusedUnlessItIsOneTheOwnerDeclared(t *testing.T) {
	cases := []struct {
		name   string
		policy func(*config.Policy)
		start  map[string]any
		want   string
	}{
		{"not declared", nil, taskStart("r1", "deploy", nil, 0), `task "deploy" is not declared`},
		{"a parameter it does not have", nil, taskStart("r1", "build", map[string]string{"TARGET": "x"}, 0), "no parameter TARGET"},
		{"a value outside the pattern", nil, taskStart("r1", "build", map[string]string{"SCHEME": "App; rm -rf /"}, 0), "value of SCHEME"},
		{"a value the pattern only partly matches", nil, taskStart("r1", "build", map[string]string{"SCHEME": "App\nevil"}, 0), "value of SCHEME"},
		{"host tasks turned off", func(p *config.Policy) { p.HostTasks.Enabled = false }, taskStart("r1", "build", nil, 0), "does not run work without a VM"},
		{"a command in the request", nil, func() map[string]any {
			m := taskStart("r1", "deploy", nil, 0)
			m["workload"].(map[string]any)["command"] = []string{"/bin/sh", "-c", "id"}
			m["workload"].(map[string]any)["script"] = "id"
			return m
		}(), "not declared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "record")
			policy := taskPolicy(t, record, "exit 0")
			if c.policy != nil {
				c.policy(&policy)
			}
			cp, _, px := startHostAgent(t, policy, oneRunner(""))
			<-cp.hellos
			cp.send(c.start)
			m := cp.next(status("r1", "failed"))
			if !strings.Contains(m["reason"].(string), c.want) || m["code"] != "refused_policy" {
				t.Fatalf("refusal %v", m)
			}
			if _, err := os.Stat(record); err == nil {
				t.Error("the refused task ran")
			}
			if entries, _ := os.ReadDir(px.Dir); len(entries) != 0 {
				t.Errorf("refused work was given a directory: %v", entries)
			}
		})
	}
}

func TestATaskEndsAtTheSoonerTimeLimitAndWhenStopped(t *testing.T) {
	policy := taskPolicy(t, filepath.Join(t.TempDir(), "r"), "sleep 30")
	policy.HostTasks.Max = 2
	task := policy.Tasks["build"]
	task.TimeoutSeconds = 600
	task.MaxConcurrent = 1
	policy.Tasks["build"] = task
	cp, _, px := startHostAgent(t, policy, oneRunner(""))
	<-cp.hellos

	// The request's limit is sooner than the owner's ten minutes.
	cp.send(taskStart("t1", "build", nil, 1))
	cp.next(status("t1", "running"))
	// The owner allows one run of it at once, though the host has a second slot.
	cp.send(taskStart("t2", "build", nil, 0))
	if m := cp.next(status("t2", "failed")); m["code"] != "refused_capacity" || !strings.Contains(m["reason"].(string), "the most allowed") {
		t.Fatalf("refusal %v", m)
	}
	if m := cp.next(status("t1", "failed")); m["code"] != "timeout" || !strings.Contains(m["reason"].(string), "time limit of 1s") {
		t.Fatalf("timeout %v", m)
	}

	cp.send(taskStart("t3", "build", nil, 0))
	cp.next(status("t3", "running"))
	cp.send(map[string]any{"type": "stop", "assignment_id": "t3", "reason": "cancelled"})
	if m := cp.next(status("t3", "failed")); m["code"] != "stopped" || !strings.Contains(m["reason"].(string), "cancelled") {
		t.Fatalf("stop %v", m)
	}
	if entries, _ := os.ReadDir(px.Dir); len(entries) != 0 {
		t.Errorf("left in the work directory: %v", entries)
	}
}

func TestTasksInPolicyAreChecked(t *testing.T) {
	ok := config.Task{Command: []string{"/usr/bin/make", "test"}, Params: map[string]string{"SCHEME": "[a-z]+"}}
	bad := map[string]config.Task{
		"no command":            {},
		"a program by name":     {Command: []string{"make"}},
		"a lower-case param":    {Command: ok.Command, Params: map[string]string{"scheme": ".*"}},
		"a pattern that is not": {Command: ok.Command, Params: map[string]string{"SCHEME": "("}},
		"a negative limit":      {Command: ok.Command, TimeoutSeconds: -1},
	}
	for name, task := range bad {
		p := config.DefaultPolicy()
		p.HostTasks = config.HostTasks{Enabled: true, AllowSameUser: true}
		p.Tasks = map[string]config.Task{"build": task}
		if err := p.Validate(); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	p := config.DefaultPolicy()
	p.Tasks = map[string]config.Task{"build": ok}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "host_tasks") {
		t.Errorf("tasks without host_tasks: %v", err)
	}
	p.HostTasks = config.HostTasks{Enabled: true, AllowSameUser: true}
	if err := p.Validate(); err != nil {
		t.Errorf("a complete policy was refused: %v", err)
	}
	p.Tasks = map[string]config.Task{"rm -rf": ok}
	if err := p.Validate(); err == nil {
		t.Error("a task name with spaces should be refused")
	}
	// A pattern matches the whole value or not at all.
	if err := ok.CheckParams(map[string]string{"SCHEME": "app1"}); err == nil {
		t.Error("a value the pattern only partly matches was allowed")
	}
}

func TestWhatATaskPrintsIsKeptOnTheHostUnderTheTasksID(t *testing.T) {
	store := tasklog.New(t.TempDir(), tasklog.Limits{})
	policy := taskPolicy(t, filepath.Join(t.TempDir(), "r"), `echo "building $MCC_PARAM_SCHEME"; echo "warning" >&2; exit 3`)
	cp, _, _ := startHostAgent(t, policy, oneRunner(""), func(o *Options) { o.TaskOutput = store })
	<-cp.hellos

	start := taskStart("a1", "build", map[string]string{"SCHEME": "App"}, 0)
	start["workload"].(map[string]any)["task_id"] = "task-1"
	start["workload"].(map[string]any)["output_secret_hash"] = "abc123"
	cp.send(start)
	m := cp.next(status("a1", "finished"))
	// None of the output travels with the status.
	for k, v := range m {
		if s, ok := v.(string); ok && strings.Contains(s, "building") {
			t.Errorf("status carries output in %s: %q", k, s)
		}
	}

	var out strings.Builder
	rec, err := store.Copy(&out, "task-1", 0, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "building App\n") || !strings.Contains(got, "warning\n") {
		t.Errorf("kept output %q", got)
	}
	if rec.Running() || rec.ExitCode == nil || *rec.ExitCode != 3 || rec.AssignmentID != "a1" || rec.Task != "build" || rec.SecretHash != "abc123" {
		t.Errorf("record %+v", rec)
	}

	// A task that is stopped keeps what it printed, and its record says why it has no exit code.
	slow := taskPolicy(t, filepath.Join(t.TempDir(), "r"), `echo started; sleep 30`)
	cp2, _, _ := startHostAgent(t, slow, oneRunner(""), func(o *Options) { o.TaskOutput = store })
	<-cp2.hellos
	cp2.send(taskStart("a2", "build", nil, 0))
	cp2.next(status("a2", "running"))
	deadline := time.Now().Add(5 * time.Second)
	for store.Size("a2") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cp2.send(map[string]any{"type": "stop", "assignment_id": "a2"})
	cp2.next(status("a2", "failed"))
	out.Reset()
	// Without a task id from the control plane, the assignment's id names the output.
	rec, err = store.Copy(&out, "a2", 0, false, nil, nil)
	if err != nil || out.String() != "started\n" || rec.Running() || rec.Code != "stopped" || rec.ExitCode != nil {
		t.Errorf("stopped task: %q %+v %v", out.String(), rec, err)
	}
}

func TestTheAgentSaysWhereItServesOutputOnlyIfItsOwnerTurnedThatOn(t *testing.T) {
	policy := taskPolicy(t, filepath.Join(t.TempDir(), "r"), "exit 0")
	cp, _, _ := startHostAgent(t, policy, oneRunner(""))
	<-cp.hellos
	capacity := cp.next(isType("capacity"))
	if _, has := capacity["output_address"]; has {
		t.Errorf("an address was reported: %v", capacity)
	}
	if _, has := capacity["output_pin"]; has {
		t.Errorf("a pin was reported: %v", capacity)
	}

	cp2, _, _ := startHostAgent(t, policy, oneRunner(""), func(o *Options) { o.OutputAddress, o.OutputPin = "studio-01.local:7443", "pin" })
	<-cp2.hellos
	capacity = cp2.next(isType("capacity"))
	if capacity["output_address"] != "studio-01.local:7443" || capacity["output_pin"] != "pin" {
		t.Errorf("capacity %v", capacity)
	}
}

func TestTaskOutputPolicyIsChecked(t *testing.T) {
	bad := []config.TaskOutput{
		{MaxMB: -1},
		{Listen: "7443"},
		{Listen: "0.0.0.0:7443", Address: "studio-01.local"},
		{Address: "studio-01.local:7443"}, // an address to tell callers, and nothing listening
	}
	for _, o := range bad {
		p := config.DefaultPolicy()
		p.TaskOutput = o
		if err := p.Validate(); err == nil {
			t.Errorf("task_output %+v should be refused", o)
		}
	}
	if got := (config.TaskOutput{}).Reachable(); got != "" {
		t.Errorf("a host that does not listen is reachable at %q", got)
	}
	if got := (config.TaskOutput{Listen: "192.168.1.20:7443"}).Reachable(); got != "192.168.1.20:7443" {
		t.Errorf("Reachable = %q", got)
	}
	if got := (config.TaskOutput{Listen: "0.0.0.0:7443", Address: "ci.example.com:443"}).Reachable(); got != "ci.example.com:443" {
		t.Errorf("Reachable = %q", got)
	}
	// "Every interface" is not somewhere to connect to: callers are told the host's name.
	if got := (config.TaskOutput{Listen: "0.0.0.0:7443"}).Reachable(); strings.HasPrefix(got, "0.0.0.0") || !strings.HasSuffix(got, ":7443") {
		t.Errorf("Reachable = %q", got)
	}
}
