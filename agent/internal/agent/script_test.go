package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/executor/fake"
)

func scriptStart(id, script string) map[string]any {
	return map[string]any{
		"type": "start", "assignment_id": id, "image": image,
		"workload": map[string]any{"kind": "script", "script": script, "task_id": "task-" + id},
	}
}

func scriptPolicy() config.Policy {
	p := config.DefaultPolicy()
	p.AllowedWorkloads = []string{"github-runner", "script"}
	return p
}

func TestAScriptRunsInAVMAndArrivesOnStdin(t *testing.T) {
	f := fake.New()
	f.ExitCode = 2
	cp, _, _ := startAgent(t, scriptPolicy(), f)
	<-cp.hellos
	if capacity := cp.next(isType("capacity")); capacity["scripts"] != true {
		t.Fatalf("capacity %v", capacity)
	}

	const script = "set -e\necho 'it''s here'\nmake test\n"
	cp.send(scriptStart("s1", script))
	cp.next(status("s1", "running"))
	m := cp.next(status("s1", "finished"))
	if m["exit_code"] != float64(2) || m["reason"] != "exited with code 2" {
		t.Fatalf("status %v", m)
	}
	// The entry command is fixed. The script is what it reads, never part of a command line.
	entry, stdin, opts := f.Given("s1")
	if entry != scriptEntry || strings.Contains(entry, "make") {
		t.Errorf("entry command %q", entry)
	}
	if stdin != script {
		t.Errorf("stdin %q", stdin)
	}
	// It is not a GitHub job: no runner is shared into its VM.
	started, stopped := f.Snapshot()
	if len(started) != 1 || len(stopped) != 1 {
		t.Errorf("VMs started %v, stopped %v", started, stopped)
	}
	if shares := opts.SharedDirs; len(shares) != 0 {
		t.Errorf("shared into a script's VM: %v", shares)
	}
}

func TestAScriptIsRefusedUnlessTheOwnerAllowsScripts(t *testing.T) {
	cases := []struct {
		name   string
		policy config.Policy
		start  map[string]any
		want   string
	}{
		{"by default", config.DefaultPolicy(), scriptStart("r1", "id"), "scripts are not allowed"},
		{"empty", scriptPolicy(), scriptStart("r1", "  \n"), "script is empty"},
		{"a declared task sent to a VM", scriptPolicy(), func() map[string]any {
			m := scriptStart("r1", "id")
			m["workload"] = map[string]any{"kind": "task", "task": "build"}
			return m
		}(), "runs on the host itself"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fake.New()
			cp, _, _ := startAgent(t, c.policy, f)
			<-cp.hellos
			if c.name == "by default" {
				if capacity := cp.next(isType("capacity")); capacity["scripts"] == true {
					t.Fatalf("a host that does not allow scripts says it does: %v", capacity)
				}
			}
			cp.send(c.start)
			m := cp.next(status("r1", "failed"))
			if !strings.Contains(m["reason"].(string), c.want) || m["code"] != "refused_policy" {
				t.Fatalf("refusal %v", m)
			}
			if started, _ := f.Snapshot(); len(started) != 0 {
				t.Errorf("a VM was started for refused work: %v", started)
			}
		})
	}
}

func TestOnlyGitHubRunnersAreAllowedUnlessTheOwnerSaysMore(t *testing.T) {
	p := config.DefaultPolicy()
	if !p.WorkloadAllowed("github-runner") || p.WorkloadAllowed("script") {
		t.Error("the default is not GitHub runners only")
	}
	p.AllowedWorkloads = []string{"script"}
	if p.WorkloadAllowed("github-runner") || !p.WorkloadAllowed("script") {
		t.Error("a list is not taken as the whole of what is allowed")
	}
	p.AllowedWorkloads = []string{"shell"}
	if err := p.Validate(); err == nil {
		t.Error("an unknown workload was accepted")
	}
}

// Even a host that runs work on itself, and allows scripts, runs a script only in a VM.
func TestAScriptIsNeverRunOnTheHostItself(t *testing.T) {
	p := scriptPolicy()
	p.HostTasks = config.HostTasks{Enabled: true, AllowSameUser: true}
	cp, f, px := startHostAgent(t, p, oneRunner(""))
	<-cp.hellos
	m := scriptStart("r1", "id")
	m["isolation"] = "host"
	delete(m, "image")
	cp.send(m)
	got := cp.next(status("r1", "failed"))
	if !strings.Contains(got["reason"].(string), "only in a VM") || got["code"] != "refused_policy" {
		t.Fatalf("refusal %v", got)
	}
	if started, _ := f.Snapshot(); len(started) != 0 {
		t.Errorf("a VM was started: %v", started)
	}
	if entries, _ := os.ReadDir(px.Dir); len(entries) != 0 {
		t.Errorf("refused work was given a directory: %v", entries)
	}
}
