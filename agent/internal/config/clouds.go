package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// One machine may be enrolled in several clouds (docs/agent.md). Each enrollment has a name its
// owner chose, a key and a host id of its own, and its own say on what that cloud may do here:
//
//	identity.key, state.json      the cloud "default": the first enrollment, where it always was
//	policy.json                   the machine's policy, and what "default" may do
//	clouds/<name>/identity.key    another cloud
//	clouds/<name>/state.json
//	clouds/<name>/policy.json     what that cloud may do here
//
// Nothing a cloud's control plane says can reach another cloud's files or work.

// DefaultCloud is the name of the enrollment kept in the state directory itself.
const DefaultCloud = "default"

var cloudName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// CheckCloudName says why name cannot be a cloud's name here, or nil.
func CheckCloudName(name string) error {
	if !cloudName.MatchString(name) {
		return fmt.Errorf("%q is not a name for a cloud: use lower-case letters, digits, '.', '_' and '-'", name)
	}
	return nil
}

// CloudDir is where one cloud's key and state are kept.
func CloudDir(dir, name string) string {
	if name == DefaultCloud {
		return dir
	}
	return filepath.Join(dir, "clouds", name)
}

// Clouds lists the clouds this machine is enrolled in: "default" first, then by name.
func Clouds(dir string) ([]string, error) {
	var out []string
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err == nil {
		out = append(out, DefaultCloud)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "clouds"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var named []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == DefaultCloud || CheckCloudName(e.Name()) != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "clouds", e.Name(), "state.json")); err == nil {
			named = append(named, e.Name())
		}
	}
	sort.Strings(named)
	return append(out, named...), nil
}

// CloudPolicy is what the owner lets one cloud do on this machine: the fields of
// clouds/<name>/policy.json. Each means what the field of the same name means in the machine's
// policy. Everything else about the machine is in the machine's policy and is the same for
// every cloud.
type CloudPolicy struct {
	// MaxVMs caps the VMs this cloud may have here at once. Zero, or more than the machine's
	// max_vms, means the machine's.
	MaxVMs               int             `json:"max_vms"`
	AllowedImages        []string        `json:"allowed_images"`
	ExcludedImages       []string        `json:"excluded_images"`
	AllowedGitHubURLs    []string        `json:"allowed_github_urls"`
	ActiveHours          *string         `json:"active_hours"`
	AllowedWorkloads     []string        `json:"allowed_workloads"`
	ScriptTimeoutSeconds int             `json:"script_timeout_seconds"`
	ShareNixStore        bool            `json:"share_nix_store"`
	HostTasks            CloudHostTasks  `json:"host_tasks"`
	Tasks                map[string]Task `json:"tasks"`
	TaskOutput           struct {
		Upload bool `json:"upload"`
	} `json:"task_output"`
}

// CloudHostTasks is a cloud's share of work on the host itself. Which account and directory
// such work runs in is the machine's to say (host_tasks in its policy), and must be said there
// before any cloud may have it.
type CloudHostTasks struct {
	Enabled bool `json:"enabled"`
	// Max is how much of it this cloud may have at once. Zero means one.
	Max          int  `json:"max"`
	GitHubRunner bool `json:"github_runner"`
}

// CloudPolicyPath is where a cloud's policy is read from. For "default" that is the machine's
// policy. For another cloud MCC_CLOUD_POLICY_DIR may name a directory holding <name>.json in
// place of clouds/<name>/policy.json (the nix-darwin module generates one).
func CloudPolicyPath(dir, name string) string {
	if name == DefaultCloud {
		return PolicyPath(dir)
	}
	if d := os.Getenv("MCC_CLOUD_POLICY_DIR"); d != "" {
		return filepath.Join(d, name+".json")
	}
	return filepath.Join(CloudDir(dir, name), "policy.json")
}

// LoadCloudPolicy is the policy that governs one cloud's work here: the machine's policy with
// that cloud's own say in place of the machine's.
//
// The cloud "default" is governed by the machine's policy as it stands, as a machine enrolled
// once always was. Any other cloud starts with nothing the owner did not give it by name: what
// "default" may do says nothing about what another cloud may. It runs GitHub Actions jobs in
// VMs and nothing else until its own policy says more. Only active_hours carries over, because
// it limits when the machine works at all.
func LoadCloudPolicy(dir, name string, machine Policy) (Policy, error) {
	if name == DefaultCloud {
		return machine, nil
	}
	var c CloudPolicy
	path := CloudPolicyPath(dir, name)
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return machine, err
	}
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		// A field of the machine's policy written here would be silently ignored otherwise.
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return machine, fmt.Errorf("%s: %w (a cloud's policy holds only what that cloud may do; the rest is in the machine's policy)", path, err)
		}
	}
	p, err := machine.ForCloud(c)
	if err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// ForCloud applies a cloud's policy to the machine's.
func (p Policy) ForCloud(c CloudPolicy) (Policy, error) {
	if c.MaxVMs < 0 || c.HostTasks.Max < 0 {
		return p, fmt.Errorf("policy: max_vms and host_tasks.max must not be negative")
	}
	if c.MaxVMs > 0 && c.MaxVMs < p.MaxVMs {
		p.MaxVMs = c.MaxVMs
	}
	p.AllowedImages, p.ExcludedImages, p.AllowedGitHubURLs = c.AllowedImages, c.ExcludedImages, c.AllowedGitHubURLs
	if c.ActiveHours != nil {
		p.ActiveHours = *c.ActiveHours
	}
	p.AllowedWorkloads, p.ScriptTimeoutSeconds = c.AllowedWorkloads, c.ScriptTimeoutSeconds
	p.ShareNixStore = c.ShareNixStore
	p.Tasks = c.Tasks
	p.TaskOutput.Upload = c.TaskOutput.Upload
	if c.HostTasks.Enabled && !p.HostTasks.Enabled {
		return p, fmt.Errorf("policy: host_tasks needs host_tasks in the machine's policy first, which says the account and directory such work runs in")
	}
	if c.HostTasks.Enabled {
		p.HostTasks.Max = min(max(1, c.HostTasks.Max), p.HostTasks.Slots())
		p.HostTasks.GitHubRunner = c.HostTasks.GitHubRunner
	} else {
		// Nothing of this cloud runs on the host itself.
		p.HostTasks = HostTasks{}
	}
	return p, p.Validate()
}
