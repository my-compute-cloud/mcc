// Package config holds the agent's on-disk state and the owner's local policy.
//
// Files live in one directory (MCC_HOME, default ~/Library/Application Support/mcc on macOS):
//
//	identity.key  Ed25519 private key, mode 0600, created by `mcc enroll`
//	state.json    server URL and host id, written by `mcc enroll`
//	policy.json   local policy, edited only by the host owner
//	runner/       cached GitHub Actions runner, shared read-only into VMs
//
// Job caches (policy cache_dir) live wherever the owner puts them, not here.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Dir returns the agent's state directory.
func Dir() (string, error) {
	if d := os.Getenv("MCC_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "mcc"), nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "mcc"), nil
	}
	return filepath.Join(home, ".config", "mcc"), nil
}

// State is what enrollment records.
type State struct {
	ServerURL string `json:"server_url"`
	HostID    string `json:"host_id"`
	TenantID  string `json:"tenant_id"`
}

func LoadState(dir string) (State, error) {
	var s State
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, fmt.Errorf("this host is not enrolled; run `mcc enroll --server URL TOKEN` first")
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func SaveState(dir string, s State) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return writeFile(filepath.Join(dir, "state.json"), b, 0o600)
}

func writeFile(p string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Policy is the host owner's local policy. The control plane cannot override it: a start that
// breaks it is refused with a reason.
type Policy struct {
	// MaxVMs caps concurrent VMs. Apple's licence allows two macOS VMs per host.
	MaxVMs int `json:"max_vms"`
	// AllowedImages are glob patterns (path.Match syntax, plus a trailing *) matched against
	// image sources. Empty means any image the control plane's pool configuration names.
	AllowedImages []string `json:"allowed_images"`
	// AllowedGitHubURLs restricts which repositories or organizations runners may register
	// with, for example "https://github.com/your-org". Empty allows any.
	AllowedGitHubURLs []string `json:"allowed_github_urls"`
	// CPUs and MemoryMB per VM. Zero means half the host's.
	CPUs     int `json:"cpus"`
	MemoryMB int `json:"memory_mb"`
	// ActiveHours like "22:00-07:00" in local time. Empty means always.
	ActiveHours string `json:"active_hours"`
	// StayAwake prevents idle sleep while any VM runs.
	StayAwake *bool `json:"stay_awake"`
	// IsolateNetwork restricts VMs to internet-only access using Tart's Softnet.
	IsolateNetwork *bool `json:"isolate_network"`
	// RequireNetworkIsolation refuses to run VMs when isolation is unavailable instead of
	// falling back to shared networking.
	RequireNetworkIsolation bool `json:"require_network_isolation"`
	// SharedRunner keeps a current GitHub Actions runner on the host and shares it into each
	// VM, instead of relying on the version baked into the image.
	SharedRunner *bool `json:"shared_runner"`
	// ImageRefreshHours re-pulls images this often. Zero disables refresh.
	ImageRefreshHours int `json:"image_refresh_hours"`
	// DiskGB is each VM's disk size. Zero, or less than the image's, keeps the image's size.
	DiskGB int `json:"disk_gb"`
	// Xcode is the path of an Xcode.app on this host to copy into macOS images, so that jobs
	// have Xcode although the image was published without it. Empty copies nothing.
	Xcode string `json:"xcode"`
	// PreloadImages are container images to pull into Linux images on this host, so that jobs
	// start their containers and services without pulling them: for example "mysql:8.4".
	PreloadImages []string `json:"preload_images"`
	// CacheDir is a host directory where jobs keep downloads between runs. Empty gives jobs
	// no cache: nothing a job writes then outlives it.
	CacheDir string `json:"cache_dir"`
	// CacheScope is who shares a cache: "runner" (the default) gives each repository or
	// organization that runners register with its own; "host" gives every job the same one.
	CacheScope string `json:"cache_scope"`
	// CacheMaxGB is the size above which the least recently used caches are deleted. Zero
	// means no limit.
	CacheMaxGB int `json:"cache_max_gb"`
	// HostTasks lets work run on this host itself, without a virtual machine. Off unless the
	// owner turns it on: such work is not isolated from the host.
	HostTasks HostTasks `json:"host_tasks"`
	// Tasks are commands the control plane may ask this host to run, by name. The owner writes
	// each command here; a request can only choose one and give values for the parameters it
	// declares. They run on the host itself, so they need host_tasks.
	Tasks map[string]Task `json:"tasks"`
	// AllowedWorkloads are the kinds of work this host takes from the control plane:
	// "github-runner", and "script", a shell script the caller wrote, which runs only inside a
	// VM. Empty means GitHub runners only, so that a newer agent never does more than the one
	// before it unless the owner says so. Declared tasks need no entry: declaring one is the yes.
	AllowedWorkloads []string `json:"allowed_workloads"`
	// ScriptTimeoutSeconds is the longest a script may run. Zero means an hour.
	ScriptTimeoutSeconds int `json:"script_timeout_seconds"`
	// LogLevel is "debug" to have the agent log everything, including each line a runner
	// prints before it takes a job. Empty or "info" is the usual amount.
	LogLevel string `json:"log_level,omitempty"`
	// UpgradeWaitMinutes is how long running work may hold back a switch to a newer agent
	// that has been installed. After it the agent switches anyway and what is left is
	// stopped. Absent means six hours; 0 means as long as it takes.
	UpgradeWaitMinutes *int `json:"upgrade_wait_minutes,omitempty"`
	// TaskOutput is how much of what tasks print is kept on this host, and for how long.
	TaskOutput TaskOutput `json:"task_output"`
}

// TaskOutput limits the output kept of tasks. Zero values mean the defaults.
type TaskOutput struct {
	// MaxMB is the most kept of one run. What a task prints beyond it is dropped. Default 64.
	MaxMB int `json:"max_mb"`
	// TotalMB is the most kept of all runs together; the oldest go first. Default 1024.
	TotalMB int `json:"total_mb"`
	// KeepDays is how long a run's output is kept after it ends. Default 7.
	KeepDays int `json:"keep_days"`
	// Upload lets this host send a finished task's output to the control plane, where the
	// tenant's plan keeps it for a time. Off by default: it is the one way output leaves the
	// host in a form the control plane can read.
	Upload bool `json:"upload"`
	// Listen is an address, such as "0.0.0.0:7443", on which this host gives a task's output
	// to whoever shows that task's secret. Empty, the default, means the host accepts no
	// connections at all and output is read on the host itself.
	Listen string `json:"listen"`
	// Address is the host:port callers are told to connect to, when it is not what Listen
	// says: a name others can resolve, or the outside of a port forward.
	Address string `json:"address"`
}

// Reachable is the host:port callers are told to connect to, or "" if the host does not listen.
func (o TaskOutput) Reachable() string {
	if o.Listen == "" {
		return ""
	}
	if o.Address != "" {
		return o.Address
	}
	host, port, err := net.SplitHostPort(o.Listen)
	if err != nil {
		return ""
	}
	// An address that means "every interface" is not one to connect to.
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		if name, err := os.Hostname(); err == nil {
			host = name
		}
	}
	return net.JoinHostPort(host, port)
}

func (o TaskOutput) validate() error {
	if o.MaxMB < 0 || o.TotalMB < 0 || o.KeepDays < 0 {
		return fmt.Errorf("policy: task_output values must not be negative")
	}
	for name, v := range map[string]string{"listen": o.Listen, "address": o.Address} {
		if v == "" {
			continue
		}
		if _, port, err := net.SplitHostPort(v); err != nil || port == "" {
			return fmt.Errorf("policy: task_output.%s must be host:port", name)
		}
	}
	if o.Address != "" && o.Listen == "" {
		return fmt.Errorf("policy: task_output.address means nothing without task_output.listen")
	}
	return nil
}

// Task is one command the owner lets the control plane ask for.
type Task struct {
	// Command is the program and its arguments, run as written: no shell reads it. The program
	// is named by its full path.
	Command []string `json:"command"`
	// Params are the parameters a request may set, each with the pattern its value must match
	// in full. A request that sets anything else is refused. The command finds a value in the
	// environment variable MCC_PARAM_<NAME>, and never on its command line.
	Params map[string]string `json:"params"`
	// TimeoutSeconds is the longest it may run. Zero means an hour.
	TimeoutSeconds int `json:"timeout_seconds"`
	// MaxConcurrent is how many runs of it there may be at once. Zero means as many as
	// host_tasks.max allows.
	MaxConcurrent int `json:"max_concurrent"`
}

// WorkloadAllowed reports whether the owner lets this kind of work run here.
func (p Policy) WorkloadAllowed(kind string) bool {
	if len(p.AllowedWorkloads) == 0 {
		return kind == "github-runner"
	}
	for _, k := range p.AllowedWorkloads {
		if k == kind {
			return true
		}
	}
	return false
}

// ScriptTimeout is the longest a script may run.
// UpgradeWait is how long running work may hold back a switch to a newer agent. Zero means
// as long as it takes.
func (p Policy) UpgradeWait() time.Duration {
	if p.UpgradeWaitMinutes == nil {
		return 6 * time.Hour
	}
	return time.Duration(*p.UpgradeWaitMinutes) * time.Minute
}

func (p Policy) ScriptTimeout() time.Duration {
	if p.ScriptTimeoutSeconds <= 0 {
		return time.Hour
	}
	return time.Duration(p.ScriptTimeoutSeconds) * time.Second
}

// Timeout is the longest the task may run.
func (t Task) Timeout() time.Duration {
	if t.TimeoutSeconds <= 0 {
		return time.Hour
	}
	return time.Duration(t.TimeoutSeconds) * time.Second
}

var (
	taskName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	paramName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,39}$`)
)

// paramPattern compiles a parameter's pattern so that it must match the whole value.
func paramPattern(p string) (*regexp.Regexp, error) {
	return regexp.Compile(`\A(?:` + p + `)\z`)
}

// CheckParams says why a request's parameters are not ones the owner allows, or nil.
func (t Task) CheckParams(params map[string]string) error {
	for k, v := range params {
		pattern, ok := t.Params[k]
		if !ok {
			return fmt.Errorf("the task has no parameter %s", k)
		}
		re, err := paramPattern(pattern)
		if err != nil || !re.MatchString(v) {
			return fmt.Errorf("the value of %s is not one the task allows", k)
		}
	}
	return nil
}

func (p Policy) validateTasks() error {
	if len(p.Tasks) > 0 && !p.HostTasks.Enabled {
		return fmt.Errorf("policy: tasks run on the host itself, so they need host_tasks")
	}
	for name, t := range p.Tasks {
		if !taskName.MatchString(name) {
			return fmt.Errorf("policy: tasks: %q is not a task name", name)
		}
		if len(t.Command) == 0 || !filepath.IsAbs(t.Command[0]) {
			return fmt.Errorf("policy: tasks.%s.command must start with the full path of a program", name)
		}
		for _, arg := range t.Command {
			if strings.ContainsRune(arg, 0) {
				return fmt.Errorf("policy: tasks.%s.command has an argument that cannot be passed to a program", name)
			}
		}
		for k, pattern := range t.Params {
			if !paramName.MatchString(k) {
				return fmt.Errorf("policy: tasks.%s.params: %q must be upper-case letters, digits and _", name, k)
			}
			if _, err := paramPattern(pattern); err != nil {
				return fmt.Errorf("policy: tasks.%s.params.%s: %w", name, k, err)
			}
		}
		if t.TimeoutSeconds < 0 || t.MaxConcurrent < 0 {
			return fmt.Errorf("policy: tasks.%s: timeout_seconds and max_concurrent must not be negative", name)
		}
	}
	return nil
}

// HostTasks is the owner's say on work that runs on the host itself.
type HostTasks struct {
	Enabled bool `json:"enabled"`
	// Max is how much such work runs at once. Zero means one.
	Max int `json:"max"`
	// Dir is where each piece of work gets its directory. Empty means work/ in the agent's own
	// directory. With User set it must be a directory that user can write to.
	Dir string `json:"dir"`
	// Workspace is "fresh" (the default): a new directory for each piece of work, deleted
	// afterwards. "persistent" keeps a GitHub job's checkouts and tool downloads between jobs
	// of the same repository or organization, so that later jobs start warm. Jobs that share
	// them can affect one another.
	Workspace string `json:"workspace"`
	// GitHubRunner lets GitHub Actions jobs run on the host. A workflow is arbitrary code, so
	// this needs its own yes, and allowed_github_urls must say whose workflows.
	GitHubRunner bool `json:"github_runner"`
	// User is the account work runs as, entered with sudo. The agent's own account can read
	// the agent's key and edit this policy, so work only runs as it if AllowSameUser says so.
	User          string `json:"user"`
	AllowSameUser bool   `json:"allow_same_user"`
}

// Persistent reports whether work keeps its directories between runs.
func (h HostTasks) Persistent() bool { return h.Workspace == "persistent" }

// Slots is how much work the host runs directly at once: zero unless enabled.
func (h HostTasks) Slots() int {
	if !h.Enabled {
		return 0
	}
	return max(1, h.Max)
}

func boolOr(p *bool, d bool) bool {
	if p == nil {
		return d
	}
	return *p
}

func (p Policy) StayAwakeEnabled() bool      { return boolOr(p.StayAwake, true) }
func (p Policy) NetworkIsolation() bool      { return boolOr(p.IsolateNetwork, true) }
func (p Policy) SharedRunnerEnabled() bool   { return boolOr(p.SharedRunner, true) }
func (p Policy) ImageRefresh() time.Duration { return time.Duration(p.ImageRefreshHours) * time.Hour }

// DefaultPolicy is used when policy.json does not exist.
func DefaultPolicy() Policy {
	return Policy{MaxVMs: 2, ImageRefreshHours: 24}
}

// PolicyPath is policy.json in dir, unless MCC_POLICY_FILE names another file (the nix-darwin
// module points it at a file generated from the host configuration).
func PolicyPath(dir string) string {
	if p := os.Getenv("MCC_POLICY_FILE"); p != "" {
		return p
	}
	return filepath.Join(dir, "policy.json")
}

func LoadPolicy(dir string) (Policy, error) {
	p := DefaultPolicy()
	b, err := os.ReadFile(PolicyPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("policy.json: %w", err)
	}
	return p, p.Validate()
}

func (p Policy) Validate() error {
	if p.MaxVMs < 0 {
		return fmt.Errorf("policy: max_vms must not be negative")
	}
	if p.LogLevel != "" && p.LogLevel != "info" && p.LogLevel != "debug" {
		return fmt.Errorf("policy: log_level must be info or debug")
	}
	if p.UpgradeWaitMinutes != nil && *p.UpgradeWaitMinutes < 0 {
		return fmt.Errorf("policy: upgrade_wait_minutes must not be negative")
	}
	if p.ActiveHours != "" {
		if _, _, err := parseHours(p.ActiveHours); err != nil {
			return fmt.Errorf("policy: active_hours: %w", err)
		}
	}
	for _, g := range p.AllowedImages {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("policy: allowed_images %q: %w", g, err)
		}
	}
	if p.DiskGB < 0 || p.CacheMaxGB < 0 {
		return fmt.Errorf("policy: disk_gb and cache_max_gb must not be negative")
	}
	if p.Xcode != "" && (!filepath.IsAbs(p.Xcode) || !strings.HasSuffix(strings.TrimSuffix(p.Xcode, "/"), ".app")) {
		return fmt.Errorf("policy: xcode must be the full path of an Xcode.app")
	}
	for _, ref := range p.PreloadImages {
		if !containerImage.MatchString(ref) {
			return fmt.Errorf("policy: preload_images %q is not a container image reference", ref)
		}
	}
	if p.CacheDir != "" && !filepath.IsAbs(p.CacheDir) {
		return fmt.Errorf("policy: cache_dir must be a full path")
	}
	if p.CacheScope != "" && p.CacheScope != "runner" && p.CacheScope != "host" {
		return fmt.Errorf("policy: cache_scope must be runner or host")
	}
	if err := p.HostTasks.validate(p); err != nil {
		return err
	}
	if err := p.TaskOutput.validate(); err != nil {
		return err
	}
	for _, k := range p.AllowedWorkloads {
		if k != "github-runner" && k != "script" {
			return fmt.Errorf("policy: allowed_workloads %q is not github-runner or script", k)
		}
	}
	if p.ScriptTimeoutSeconds < 0 {
		return fmt.Errorf("policy: script_timeout_seconds must not be negative")
	}
	return p.validateTasks()
}

// userName is the shape of an account name: it starts with a letter or underscore, so it can
// never be read as an option to sudo.
var userName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,31}$`)

func (h HostTasks) validate(p Policy) error {
	if !h.Enabled {
		return nil
	}
	if h.Max < 0 {
		return fmt.Errorf("policy: host_tasks.max must not be negative")
	}
	if h.Dir != "" && !filepath.IsAbs(h.Dir) {
		return fmt.Errorf("policy: host_tasks.dir must be a full path")
	}
	if h.Workspace != "" && h.Workspace != "fresh" && h.Workspace != "persistent" {
		return fmt.Errorf("policy: host_tasks.workspace must be fresh or persistent")
	}
	switch {
	case h.User != "" && h.AllowSameUser:
		return fmt.Errorf("policy: host_tasks has both user and allow_same_user; choose one")
	case h.User == "" && !h.AllowSameUser:
		return fmt.Errorf("policy: host_tasks needs user, the account work runs as, or allow_same_user to run it as the agent's own account, which can read the agent's key and change this policy")
	case h.User != "" && !userName.MatchString(h.User):
		return fmt.Errorf("policy: host_tasks.user %q is not an account name", h.User)
	case h.User != "" && h.Dir == "":
		return fmt.Errorf("policy: host_tasks.dir is needed with host_tasks.user: a directory that user can write to")
	}
	if h.GitHubRunner && len(p.AllowedGitHubURLs) == 0 {
		return fmt.Errorf("policy: host_tasks.github_runner needs allowed_github_urls, to say whose workflows may run on this host")
	}
	return nil
}

// containerImage is the shape of a container image reference: registry, path, tag or digest.
// It starts with a letter or digit, so a reference can never be read as an option.
var containerImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

// ImageAllowed reports whether policy permits an image source.
func (p Policy) ImageAllowed(source string) bool {
	if len(p.AllowedImages) == 0 {
		return true
	}
	for _, g := range p.AllowedImages {
		if strings.HasSuffix(g, "*") && strings.HasPrefix(source, strings.TrimSuffix(g, "*")) {
			return true
		}
		if ok, _ := path.Match(g, source); ok {
			return true
		}
	}
	return false
}

// GitHubURLAllowed reports whether a runner may register with this GitHub URL.
func (p Policy) GitHubURLAllowed(u string) bool {
	if len(p.AllowedGitHubURLs) == 0 {
		return true
	}
	u = strings.TrimSuffix(strings.ToLower(u), "/")
	for _, a := range p.AllowedGitHubURLs {
		if strings.TrimSuffix(strings.ToLower(a), "/") == u {
			return true
		}
	}
	return false
}

// Active reports whether t falls within the active hours.
func (p Policy) Active(t time.Time) bool {
	if p.ActiveHours == "" {
		return true
	}
	from, to, err := parseHours(p.ActiveHours)
	if err != nil {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if from <= to {
		return m >= from && m < to
	}
	return m >= from || m < to // spans midnight
}

func parseHours(s string) (int, int, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected HH:MM-HH:MM")
	}
	a, err := parseClock(parts[0])
	if err != nil {
		return 0, 0, err
	}
	b, err := parseClock(parts[1])
	return a, b, err
}

func parseClock(s string) (int, error) {
	hm := strings.Split(strings.TrimSpace(s), ":")
	if len(hm) != 2 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	h, err1 := strconv.Atoi(hm[0])
	m, err2 := strconv.Atoi(hm[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return h*60 + m, nil
}
