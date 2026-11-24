package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Share names of the host's Nix store, when the owner shares it: the store itself, and a
// directory that holds a copy of its database.
const (
	nixStoreShare = "mcc-nix-store"
	nixStateShare = "mcc-nix-state"
)

// HostNix is the host's own Nix store, shared read-only with jobs so that what the host has
// already built or downloaded is not built or downloaded again.
//
// A job never uses the store as its own: it has one in its VM, and Nix there copies from the
// host's what it would otherwise fetch or build. For that Nix needs the store's database
// beside the store. The live one is being written by the host, so each job gets a copy made
// at its start, which also means it sees what the host built a minute ago.
type HostNix struct {
	// Dir is the host's Nix directory, /nix.
	Dir string
	// Work is where the copies of the database are kept while jobs run.
	Work string
	// Sqlite is the sqlite3 program that makes them.
	Sqlite string
}

// NewHostNix returns the host's store if it has one that can be shared, or why not.
func NewHostNix(work string) (*HostNix, error) {
	n := &HostNix{Dir: "/nix", Work: work}
	if _, err := os.Stat(n.database()); err != nil {
		return nil, fmt.Errorf("this host has no Nix store at %s", n.Dir)
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, fmt.Errorf("sharing the Nix store needs sqlite3, which is not installed")
	}
	n.Sqlite = sqlite
	// Copies a stopped agent left behind.
	_ = os.RemoveAll(work)
	return n, nil
}

func (n *HostNix) database() string { return filepath.Join(n.Dir, "var", "nix", "db", "db.sqlite") }

// Store is the directory to share as the store.
func (n *HostNix) Store() string { return filepath.Join(n.Dir, "store") }

// The directories Nix expects beside a database. It only reads the store it is given, but
// it will not open one whose state directory lacks them, and it cannot make them in a share
// it may not write to.
var nixStateDirs = []string{
	"db",
	"profiles/per-user/root",
	"gcroots/per-user",
	"gcroots/auto",
	"temproots",
	"userpool",
	"builds",
}

// Snapshot makes a state directory for one job: a copy of the database as it is now, and
// the empty directories Nix looks for. The caller removes it with the returned function.
func (n *HostNix) Snapshot(ctx context.Context, id string) (string, func(), error) {
	// The directory is deleted afterwards, so it must be one of ours and nothing above it.
	if !idPattern.MatchString(id) {
		return "", nil, fmt.Errorf("id %q cannot name a directory", id)
	}
	dir := filepath.Join(n.Work, id)
	remove := func() { _ = os.RemoveAll(dir) }
	for _, d := range nixStateDirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return "", nil, err
		}
	}
	// The schema file says which version of the database this is.
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(n.database()), "schema")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "db", "schema"), b, 0o644); err != nil {
			remove()
			return "", nil, err
		}
	}
	// VACUUM INTO writes one consistent file, including what the host's Nix has not yet
	// moved out of its write-ahead log. A plain copy of the file could be torn, and would
	// miss the most recent paths.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out := filepath.Join(dir, "db", "db.sqlite")
	cmd := exec.CommandContext(ctx, n.Sqlite, "file:"+n.database()+"?mode=ro",
		"VACUUM INTO '"+strings.ReplaceAll(out, "'", "''")+"'")
	if b, err := cmd.CombinedOutput(); err != nil {
		remove()
		return "", nil, fmt.Errorf("copying the Nix database: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return dir, remove, nil
}

// nixSetup is the part of a runner's entry command that points Nix in the guest at what the
// host offers: the host's own store, read-only (HostNix), and a place in the job's cache
// where builds are kept for later jobs of the same repository. SHARES is the directory the
// guest has the host's shares in, and SHARES_URL the same as it is written in a URL.
//
// Both are sources Nix takes paths from instead of building or downloading them, tried
// before the public cache. Neither is signed. The host's store is the owner's; the cache
// holds what earlier jobs of the same repository built or fetched, which is who the owner
// said shares a cache.
//
// The cache is tried first. A path in it is one file, read in one go, while a path in the
// host's store is read file by file through the share, which is slow for a path of many
// small files: the source of nixpkgs, some ninety thousand of them, took 44 seconds from
// the host's store, 18 from GitHub and 6 from the cache.
//
// What a job builds itself goes into the cache as soon as it is built, by a hook Nix runs
// after each build, so a job that fails late still leaves its work.
//
// Everything else a job added to its store is written when the job has ended, by
// mcc_nix_keep, which the entry command runs then: what it fetched, such as the inputs of a
// flake, which no build hook sees, and what it copied from the host's store or downloaded.
// A system of 13 GiB took 126 seconds to copy from the host's store and 44 from the cache.
// So a repository's cache comes to hold what its jobs use, a second copy of part of the
// host's store; the agent deletes what no job has used for a while (cache.TrimNix), which
// it can tell because a job marks each path it used that the cache already had.
//
// The settings are also left in MCC_NIX_CONFIG, for an image that has no Nix and a workflow
// that installs it: adding them to /etc/nix/nix.conf after installing does the same.
const nixSetup = `mcc_nix() {
  local conf="" subs=""
  hook=/etc/nix/mcc-built
  if [ -r "$SHARES/` + nixStateShare + `/db/db.sqlite" ] && [ -d "$SHARES/` + nixStoreShare + `" ]; then
    subs="local?store=/nix/store&real=$SHARES_URL/` + nixStoreShare + `&state=$SHARES_URL/` + nixStateShare + `&read-only=true&trusted=true&priority=10"
    conf="extra-experimental-features = read-only-local-store
"
  fi
  if [ -n "${MCC_CACHE_DIR:-}" ] && [ -w "$MCC_CACHE_DIR" ] && sudo -n true 2>/dev/null; then
    sudo mkdir -p /etc/nix
    sudo tee "$hook" >/dev/null <<HOOK
#!/bin/bash
# Written by My Compute Cloud's agent. Nix runs this after each build; it keeps what was
# built in the job's cache, as a binary cache, for later jobs.
C="$MCC_CACHE_DIR/nix"
NS=/nix/var/nix/profiles/default/bin/nix-store
[ -x "\$NS" ] || NS=/run/current-system/sw/bin/nix-store
[ -x "\$NS" ] || NS=nix-store
mkdir -p "\$C/nar" || exit 0
[ -e "\$C/nix-cache-info" ] || printf 'StoreDir: /nix/store\n' > "\$C/nix-cache-info"
# The signatures each path carries, one line a path: the path, padded with spaces to line
# up, a tab, its signatures. Nix also lists there that it built a path itself ("ultimate")
# or that the path is addressed by its content ("ca:..."), which are not signatures. They
# are kept with the path, so that a later job can still tell which cache it came from, and
# a tool that publishes what a job built can leave out what a public cache already has.
SIGS=\$("\${NS%-store}" --extra-experimental-features nix-command path-info --sigs \$OUT_PATHS 2>/dev/null) || true
sigs_of() { printf '%s\n' "\$SIGS" | awk -F'\t' -v p="\$1" '{ k = \$1; sub(/ +\$/, "", k) } k == p { n = split(\$2, s, " "); for (i = 1; i <= n; i++) if (s[i] ~ /^[^:]+:[A-Za-z0-9+\/]+=*\$/ && s[i] !~ /^ca:/) print "Sig: " s[i]; exit }'; }
for p in \$OUT_PATHS; do
  b=\${p##*/}; h=\${b%%-*}
  # Already kept. It is marked as used, which is how the host knows not to delete it. One
  # kept before signatures were is given them now.
  if [ -e "\$C/\$h.narinfo" ]; then
    touch "\$C/\$h.narinfo"
    if ! grep -q '^Sig: ' "\$C/\$h.narinfo"; then
      sig=\$(sigs_of "\$p")
      if [ -n "\$sig" ]; then
        { cat "\$C/\$h.narinfo"; printf '%s\n' "\$sig"; } > "\$C/.\$h.\$\$" && mv "\$C/.\$h.\$\$" "\$C/\$h.narinfo"
      fi
    fi
    continue
  fi
  hash=\$("\$NS" -q --hash "\$p") || continue
  size=\$("\$NS" -q --size "\$p") || continue
  refs=\$("\$NS" -q --references "\$p" | sed 's#.*/##' | tr '\n' ' ')
  # Written under another name and then renamed, so that a job reading the cache never
  # finds half a file.
  t="\$C/nar/.\$h.\$\$"
  if "\$NS" --dump "\$p" > "\$t" && mv "\$t" "\$C/nar/\$h.nar"; then
    sig=\$(sigs_of "\$p")
    printf 'StorePath: %s\nURL: nar/%s.nar\nCompression: none\nNarHash: %s\nNarSize: %s\nReferences: %s\n%s' \
      "\$p" "\$h" "\$hash" "\$size" "\$refs" "\${sig:+\$sig
}" > "\$C/.\$h.\$\$" && mv "\$C/.\$h.\$\$" "\$C/\$h.narinfo"
  else
    rm -f "\$t"
  fi
done
exit 0
HOOK
    sudo chmod 755 "$hook"
    subs="file://$MCC_CACHE_DIR/nix?trusted=true&priority=5${subs:+ $subs}"
    # What the store holds now, so that what the job adds can be told apart at its end.
    MCC_NIX_BEFORE=$(mktemp)
    ls /nix/store > "$MCC_NIX_BEFORE" 2>/dev/null || true
    conf="${conf}post-build-hook = $hook
"
  fi
  [ -n "$subs" ] || return 0
  conf="${conf}extra-substituters = $subs
fallback = true
"
  export MCC_NIX_CONFIG="$conf"
  # Only an image that has Nix is set up here, and only where the job's user may: the
  # settings are the Nix daemon's, which reads them when it starts.
  [ -e /nix/var/nix/daemon-socket ] && sudo -n true 2>/dev/null || return 0
  local t
  t=$(mktemp)
  # The file may be a link into the store, as nix-darwin makes it; it is replaced by a copy
  # with the settings added.
  cat /etc/nix/nix.conf > "$t" 2>/dev/null || true
  printf '\n%s' "$conf" >> "$t"
  sudo rm -f /etc/nix/nix.conf
  sudo install -m 0644 "$t" /etc/nix/nix.conf
  rm -f "$t"
  if command -v launchctl >/dev/null 2>&1; then
    sudo launchctl kickstart -k system/org.nixos.nix-daemon 2>/dev/null ||
      sudo launchctl kickstart -k system/systems.determinate.nix-daemon 2>/dev/null || true
  else
    sudo systemctl restart nix-daemon 2>/dev/null || true
  fi
  # The daemon takes a moment to listen again, and a job's first step may be a build.
  local ns=/nix/var/nix/profiles/default/bin/nix-store
  [ -x "$ns" ] || ns=$(command -v nix-store) || return 0
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    "$ns" --gc --print-roots >/dev/null 2>&1 && break
    sleep 1
  done
}
# mcc_nix_keep writes to the job's cache what the job added to its store, with the same hook
# that writes what it built, a few paths at a time. It does nothing unless mcc_nix set the
# cache up. Derivations are left out: Nix makes those again from the source in no time.
mcc_nix_keep() {
  [ -n "${MCC_NIX_BEFORE:-}" ] && [ -x "${hook:-}" ] || return 0
  ls /nix/store 2>/dev/null | grep -v -e '^\.' -e '\.drv$' -e '\.lock$' -e '\.check$' |
    grep -vxFf "$MCC_NIX_BEFORE" | sed 's#^#/nix/store/#' |
    xargs -n 8 -P 4 sudo -n -H sh -c 'OUT_PATHS="$*" exec "$0"' "$hook" >/dev/null 2>&1 || true
  rm -f "$MCC_NIX_BEFORE"
}
mcc_nix || true
`
