// Package config holds the agent's on-disk state and the owner's local policy.
//
// Files live in one directory (MCC_HOME, default ~/Library/Application Support/mcc on macOS):
//
//	identity.key  Ed25519 private key, mode 0600, created by `mcc enroll`
//	state.json    server URL and host id, written by `mcc enroll`
//	policy.json   local policy, edited only by the host owner
//	runner/       cached GitHub Actions runner, shared read-only into VMs
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
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
	return nil
}

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
