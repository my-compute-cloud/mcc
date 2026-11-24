package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func at(h, m int) time.Time { return time.Date(2026, 11, 16, h, m, 0, 0, time.Local) }

func TestActiveHours(t *testing.T) {
	p := Policy{ActiveHours: "22:00-07:00"}
	for _, c := range []struct {
		h, m int
		want bool
	}{{23, 0, true}, {3, 30, true}, {7, 0, false}, {12, 0, false}, {22, 0, true}} {
		if got := p.Active(at(c.h, c.m)); got != c.want {
			t.Errorf("%02d:%02d = %v, want %v", c.h, c.m, got, c.want)
		}
	}
	day := Policy{ActiveHours: "09:00-17:00"}
	if !day.Active(at(9, 0)) || day.Active(at(17, 0)) {
		t.Error("daytime window")
	}
	if !(Policy{}).Active(at(3, 0)) {
		t.Error("empty means always")
	}
	if err := (Policy{ActiveHours: "25:00-1"}).Validate(); err == nil {
		t.Error("expected validation error")
	}
}

func TestImageAllowed(t *testing.T) {
	p := Policy{AllowedImages: []string{"ghcr.io/cirruslabs/*", "ghcr.io/me/exact:1"}}
	for src, want := range map[string]bool{
		"ghcr.io/cirruslabs/macos-sequoia-xcode:latest": true,
		"ghcr.io/me/exact:1":                            true,
		"ghcr.io/me/exact:2":                            false,
		"docker.io/evil:latest":                         false,
	} {
		if p.ImageAllowed(src) != want {
			t.Errorf("%s: want %v", src, want)
		}
	}
	if !(Policy{}).ImageAllowed("ghcr.io/anyone/anything:latest") {
		t.Error("empty list allows")
	}
}

func TestAnImageOnTheHostNeedsAPatternThatNamesIt(t *testing.T) {
	for _, src := range []string{"my-own-vm", "/srv/images/ubuntu", "./images/ubuntu", "/ghcr.io/me/x", ""} {
		if !LocalImage(src) {
			t.Errorf("%q names something on the host", src)
		}
		if (Policy{}).ImageAllowed(src) {
			t.Errorf("%q: allowed with no pattern", src)
		}
		if (Policy{AllowedImages: []string{"ghcr.io/me/*"}}).ImageAllowed(src) {
			t.Errorf("%q: allowed by a pattern for a registry", src)
		}
	}
	if LocalImage("ghcr.io/me/ubuntu:latest") || LocalImage("localhost:5000/ubuntu") {
		t.Error("a registry reference was taken for something on the host")
	}
	p := Policy{AllowedImages: []string{"my-own-vm", "/srv/images/*"}, ExcludedImages: []string{"/srv/images/old"}}
	for src, want := range map[string]bool{
		"my-own-vm":                   true,
		"my-other-vm":                 false,
		"/srv/images/ubuntu":          true,
		"/srv/images/old":             false,
		"/srv/other/ubuntu":           false,
		"/srv/images/../../etc":       false,
		"/srv/images//ubuntu":         false,
		"ghcr.io/me/ubuntu:latest":    false,
		"/srv/images/ubuntu/../../..": false,
	} {
		if p.ImageAllowed(src) != want {
			t.Errorf("%s: want %v", src, want)
		}
	}
}

func TestWhatTheAgentMadeOrAJobWroteIsNeverAnImage(t *testing.T) {
	cache := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Mkdir(filepath.Join(cache, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A way into the cache from outside it.
	link := filepath.Join(elsewhere, "link")
	if err := os.Symlink(filepath.Join(cache, "repo"), link); err != nil {
		t.Fatal(err)
	}
	p := Policy{AllowedImages: []string{"*", "/*", "mcc-*"}, CacheDir: cache}
	for _, src := range []string{"mcc-a1", "mcc-base-0123456789ab", "MCC-a1", cache, filepath.Join(cache, "repo"), filepath.Join(cache, "not", "there"), link} {
		if p.ImageAllowed(src) {
			t.Errorf("%s: allowed", src)
		}
	}
	if !p.ImageAllowed("my-own-vm") || !p.ImageAllowed(elsewhere) {
		t.Error("the owner's own images were refused")
	}
}

func TestAnExcludedImageIsRefusedWhateverIsAllowed(t *testing.T) {
	p := Policy{
		AllowedImages:  []string{"ghcr.io/me/*"},
		ExcludedImages: []string{"ghcr.io/me/macos-runner:*", "ghcr.io/me/big"},
	}
	for src, want := range map[string]bool{
		"ghcr.io/me/ubuntu-runner:latest": true,
		"ghcr.io/me/macos-runner:tahoe":   false,
		"ghcr.io/me/big":                  false,
		"ghcr.io/other/ubuntu:latest":     false,
	} {
		if p.ImageAllowed(src) != want {
			t.Errorf("%s: want %v", src, want)
		}
	}
	// With nothing allowed by name, everything is, except what is excluded.
	only := Policy{ExcludedImages: []string{"ghcr.io/me/big"}}
	if !only.ImageAllowed("ghcr.io/other/ubuntu:latest") || only.ImageAllowed("ghcr.io/me/big") {
		t.Error("an exclusion alone refuses only what it matches")
	}
	bad := DefaultPolicy()
	bad.ExcludedImages = []string{"["}
	if bad.Validate() == nil {
		t.Error("a malformed pattern is refused")
	}
}

func TestGitHubURLAllowed(t *testing.T) {
	p := Policy{AllowedGitHubURLs: []string{"https://github.com/Acme/Builds/"}}
	if !p.GitHubURLAllowed("https://github.com/acme/builds") || p.GitHubURLAllowed("https://github.com/other/nix") {
		t.Error("url matching")
	}
	// An account's /* takes its repositories and the account itself, and nothing else.
	p = Policy{AllowedGitHubURLs: []string{"https://github.com/Me/*"}}
	for u, want := range map[string]bool{
		"https://github.com/me/game":       true,
		"https://github.com/ME/Nix/":       true,
		"https://github.com/me":            true,
		"https://github.com/me2/game":      false,
		"https://github.com/other/me":      false,
		"https://github.com/me/game/extra": false,
		"https://github.com/me/":           true,
	} {
		if got := p.GitHubURLAllowed(u); got != want {
			t.Errorf("%s allowed = %v, want %v", u, got, want)
		}
	}
	for pattern, valid := range map[string]bool{
		"https://github.com/me/*":    true,
		"https://github.com/me/*/":   true,
		"https://github.com/me/repo": true,
		"https://github.com/*":       false,
		"https://github.com/*/*":     false,
		"https://github.com/me/re*":  false,
		"https://github.com/m*/repo": false,
		"*":                          false,
	} {
		p := DefaultPolicy()
		p.AllowedGitHubURLs = []string{pattern}
		if err := p.Validate(); (err == nil) != valid {
			t.Errorf("%s valid = %v, want %v (%v)", pattern, err == nil, valid, err)
		}
	}
}

func TestLoadPolicyDefaultsAndFile(t *testing.T) {
	dir := t.TempDir()
	p, err := LoadPolicy(dir)
	if err != nil || p.MaxVMs != 2 || !p.NetworkIsolation() || !p.StayAwakeEnabled() || !p.SharedRunnerEnabled() {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "policy.json"), []byte(`{"max_vms":1,"isolate_network":false}`), 0o600)
	p, err = LoadPolicy(dir)
	if err != nil || p.MaxVMs != 1 || p.NetworkIsolation() || p.ImageRefreshHours != 24 {
		t.Fatalf("file: %+v %v", p, err)
	}
}

func TestState(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadState(dir); err == nil {
		t.Fatal("expected not enrolled")
	}
	if err := SaveState(dir, State{ServerURL: "https://x", HostID: "h"}); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(dir)
	if err != nil || s.HostID != "h" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestDiskXcodeAndCachePolicy(t *testing.T) {
	ok := Policy{DiskGB: 120, Xcode: "/Applications/Xcode.app", CacheDir: "/Users/me/mcc-cache", CacheScope: "host", CacheMaxGB: 50,
		PreloadImages: []string{"mysql:8.4", "ghcr.io/acme/app@sha256:0123abcd", "localhost:5000/x/y:1"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]Policy{
		"negative disk":           {DiskGB: -1},
		"relative Xcode":          {Xcode: "Xcode.app"},
		"Xcode not an app":        {Xcode: "/Applications/Xcode"},
		"relative cache":          {CacheDir: "cache"},
		"unknown cache scope":     {CacheScope: "repository"},
		"negative cache size":     {CacheMaxGB: -5},
		"image that is an option": {PreloadImages: []string{"--privileged"}},
		"image with a space":      {PreloadImages: []string{"mysql:8.4 redis:8"}},
		"empty image":             {PreloadImages: []string{""}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
