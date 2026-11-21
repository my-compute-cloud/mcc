package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAMachineListsTheCloudsItIsEnrolledIn(t *testing.T) {
	dir := t.TempDir()
	if names, err := Clouds(dir); err != nil || len(names) != 0 {
		t.Fatalf("a machine that is not enrolled: %v %v", names, err)
	}
	// An enrollment from before a machine could be in several is the cloud "default", where
	// it always was.
	if err := SaveState(dir, State{HostID: "h0"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"work", "acme"} {
		if err := SaveState(CloudDir(dir, name), State{HostID: "h-" + name}); err != nil {
			t.Fatal(err)
		}
	}
	// A directory without an enrollment in it is not a cloud.
	_ = os.MkdirAll(filepath.Join(dir, "clouds", "half"), 0o700)
	names, err := Clouds(dir)
	if err != nil || !reflect.DeepEqual(names, []string{"default", "acme", "work"}) {
		t.Fatalf("clouds: %v %v", names, err)
	}
	if s, err := LoadState(CloudDir(dir, "acme")); err != nil || s.HostID != "h-acme" {
		t.Fatalf("acme: %+v %v", s, err)
	}
	for _, bad := range []string{"", "Acme", "../x", "a/b", ".hidden"} {
		if CheckCloudName(bad) == nil {
			t.Errorf("%q was taken as a cloud's name", bad)
		}
	}
}

func TestWhatOneCloudMayDoSaysNothingOfAnother(t *testing.T) {
	dir := t.TempDir()
	machine := DefaultPolicy()
	machine.MaxVMs = 4
	machine.AllowedImages = []string{"ghcr.io/mine/*"}
	machine.AllowedGitHubURLs = []string{"https://github.com/me"}
	machine.AllowedWorkloads = []string{"github-runner", "script"}
	machine.ActiveHours = "22:00-07:00"
	machine.ShareNixStore = true
	machine.CacheDir = "/var/cache/mcc"
	machine.HostTasks = HostTasks{Enabled: true, User: "ci", Dir: "/var/ci", Max: 3}
	machine.Tasks = map[string]Task{"deploy": {Command: []string{"/usr/local/bin/deploy"}}}
	machine.TaskOutput.Upload = true
	if err := machine.Validate(); err != nil {
		t.Fatal(err)
	}

	// The first enrollment is governed by the machine's policy as it stands.
	if p, err := LoadCloudPolicy(dir, DefaultCloud, machine); err != nil || !reflect.DeepEqual(p, machine) {
		t.Fatalf("default: %+v %v", p, err)
	}

	// Another cloud, with no policy of its own, has nothing the owner gave the first: GitHub
	// jobs in VMs, and no more. What is true of the machine stays.
	p, err := LoadCloudPolicy(dir, "acme", machine)
	if err != nil {
		t.Fatal(err)
	}
	if p.WorkloadAllowed("script") || !p.WorkloadAllowed("github-runner") || len(p.Tasks) != 0 || p.HostTasks.Enabled ||
		p.ShareNixStore || p.TaskOutput.Upload || len(p.AllowedImages) != 0 || len(p.AllowedGitHubURLs) != 0 {
		t.Fatalf("a cloud was given what only another was: %+v", p)
	}
	if p.MaxVMs != 4 || p.CacheDir != machine.CacheDir || p.ActiveHours != machine.ActiveHours {
		t.Fatalf("the machine's own policy was lost: %+v", p)
	}

	write := func(body string) {
		t.Helper()
		path := CloudPolicyPath(dir, "acme")
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"max_vms": 1, "allowed_github_urls": ["https://github.com/acme"], "active_hours": "",
		"host_tasks": {"enabled": true, "max": 9, "github_runner": true},
		"tasks": {"lint": {"command": ["/usr/bin/true"]}}}`)
	p, err = LoadCloudPolicy(dir, "acme", machine)
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxVMs != 1 || p.ActiveHours != "" || !p.GitHubURLAllowed("https://github.com/acme") || p.GitHubURLAllowed("https://github.com/me") {
		t.Fatalf("the cloud's own policy was not applied: %+v", p)
	}
	// Its share of host work is never more than the machine's, in the machine's account.
	if !p.HostTasks.Enabled || p.HostTasks.Slots() != 3 || p.HostTasks.User != "ci" || !p.HostTasks.GitHubRunner {
		t.Fatalf("host work: %+v", p.HostTasks)
	}
	if _, ok := p.Tasks["lint"]; !ok || len(p.Tasks) != 1 {
		t.Fatalf("tasks: %+v", p.Tasks)
	}

	// A cap above the machine's is the machine's.
	write(`{"max_vms": 99}`)
	if p, err = LoadCloudPolicy(dir, "acme", machine); err != nil || p.MaxVMs != 4 {
		t.Fatalf("cap: %+v %v", p, err)
	}

	// What belongs in the machine's policy is refused here, not ignored.
	write(`{"cache_dir": "/elsewhere"}`)
	if _, err = LoadCloudPolicy(dir, "acme", machine); err == nil || !strings.Contains(err.Error(), "cache_dir") {
		t.Fatalf("a machine's field in a cloud's policy: %v", err)
	}

	// Host work needs the machine's say on where and as whom.
	machine.HostTasks, machine.Tasks = HostTasks{}, nil
	write(`{"host_tasks": {"enabled": true}}`)
	if _, err = LoadCloudPolicy(dir, "acme", machine); err == nil || !strings.Contains(err.Error(), "machine's policy") {
		t.Fatalf("host work the machine did not set up: %v", err)
	}
}
