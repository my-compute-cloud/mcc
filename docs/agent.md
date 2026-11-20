# Agent

The agent (`mcc`) runs on the host. It opens one outbound WebSocket to the control plane,
reports capacity, prepares images, and runs each assignment in a fresh VM that it deletes
afterwards. It never accepts inbound connections.

A host is an Apple Silicon Mac, where the VMs are [Tart](https://tart.run)'s, or a Linux
machine, where they are QEMU's with KVM. What follows is for Macs unless it says otherwise;
[Linux hosts](#linux-hosts) has what differs.

## Install

Apple Silicon Mac with [Tart](https://tart.run) installed (the agent drives it; MCC does not
ship it).

- **Nix (nix-darwin):**

  ```nix
  inputs.mcc.url = "github:my-compute-cloud/mcc";
  # in the darwin configuration
  imports = [ inputs.mcc.darwinModules.default ];
  services.mcc-agent.enable = true;
  services.mcc-agent.policy = { allowed_github_urls = [ "https://github.com/your-org" ]; };
  ```

  Tart is unfree in nixpkgs, so the configuration needs `nixpkgs.config.allowUnfree = true`
  (or an `allowUnfreePredicate` that admits `tart`). The module runs `mcc run` as a launchd
  agent in the user's session and logs to `~/Library/Logs/mcc-agent.log`. Until the host is
  enrolled the service exits cleanly.

  The flake's package is the signed release binary; `packages.agent-source` builds from source.

- **Homebrew:** `brew install my-compute-cloud/tap/mcc`
- **Install script:** `curl -fsSL https://mycompute.cloud/install.sh | sh`. It downloads the
  latest release, checks it against the release's checksums and, on a Mac, Apple's signature,
  and installs it to `/usr/local/bin`. On a Linux host it also leaves the systemd unit in
  `/usr/local/share/mcc`; see [Linux hosts](#linux-hosts).
- **From source:** `cd agent && go build -o mcc ./cmd/mcc`

Every release file carries GitHub build provenance:
`gh attestation verify mcc-darwin-arm64.tar.gz --repo my-compute-cloud/mcc`. The Mac binary is
also signed with an Apple Developer ID and notarized. The Linux ones
(`mcc-linux-amd64.tar.gz`, `mcc-linux-arm64.tar.gz`) are static binaries with no signature of
their own.

## Enroll

Sign in on the host, then enroll it into a pool. Nothing is copied or pasted:

```bash
mcc auth login
```

```bash
mcc enroll --pool <pool>
```

`mcc auth login` prints a short code and opens <https://mycompute.cloud/cli>, where you approve
it while signed in. It works over SSH too: open the address it prints on any machine. The
sign-in is yours, not the host's. It is kept in `~/.config/mcc/credentials.json`, readable only
by you, lasts 90 days from its last use, and `mcc auth logout` ends it. `--read-only` asks for
a sign-in that can see everything and change nothing.

Without signing in, add the host from the dashboard at <https://mycompute.cloud/dashboard>,
which gives you a single-use command:

```bash
mcc enroll --server https://<control plane> <token>
```

The token is single use and expires after 15 minutes. Enrollment generates an Ed25519 key in
the state directory; only the public key is sent. Every connection is authenticated by signing
a fresh challenge. Revoking the host in the control plane deletes its public key.

State lives in `~/Library/Application Support/mcc` on a Mac and in `~/.config/mcc` (or
`$XDG_CONFIG_HOME/mcc`) on Linux; override with `MCC_HOME`:

| File | Holds |
|---|---|
| `identity.key` | The private key, mode 0600 |
| `state.json` | Server URL and host id |
| `policy.json` | Local policy (optional; `MCC_POLICY_FILE` points elsewhere) |
| `runner/`, `runner-linux/` | Cached GitHub Actions runner for macOS and for Linux guests, shared read-only into VMs |
| `kvm/` | On a Linux host: image bases, the files of running VMs, and the runner as a disk image |

## Checking a host

```bash
mcc check
```

says what is wrong with this host and what to do about it: whether it is enrolled, whether
the agent is running and connected, whether it can run VMs, which pools it is in, and whether
its own policy admits each pool's image and the GitHub accounts that send jobs. It exits with a
failure if anything stops work from running, and `--json` gives the same as a list of
findings, each with a `code` that does not change. Signed out, it looks at this machine only.

`mcc policy check [FILE]` says in words what a policy file permits, before the agent reads it.

One agent runs per state directory: it holds a lock on `agent.lock` there, which is how
`mcc check` knows whether it is running. It also says what the agent has in hand, which is
what an upgrade would wait for.

## Upgrading, and draining a host

Installing a newer agent does not stop the one that is running. The running agent notices
that a different one is installed where it was started from, and then:

1. It **drains**: it tells the control plane it takes no new work, and work that was already
   on its way to it is taken elsewhere.
2. It lets what it has finish: jobs, tasks, and an image that is being downloaded.
3. Once nothing is running it replaces itself with the installed agent, in place. The service
   manager sees nothing happen, so nothing is restarted and no job is lost to an update.

Work can hold this back for six hours, or for what `upgrade_wait_minutes` says in the host's
policy; after that the agent switches anyway and what is left is stopped. `0` waits as long
as it takes. `mcc check` and `mcc status` say when a host is draining and why.

This is the same however the agent was installed. The nix-darwin module starts the agent
through a launcher in the system profile and names it in `MCC_UPGRADE_PATH`, so a system
update that changes the agent or its policy is picked up this way and never restarts it.
Elsewhere the agent watches its own file, which the install script replaces. A rollback is
picked up like an upgrade.

To see which agent is running, compare the two:

```bash
mcc version
```

is the installed one, and the `Agent ...` line of `mcc check` is the one that is running.
A build from source carries the commit it was made from, such as `0.1.0+e2c4478`, so two
builds of the same number can be told apart; a release carries its number alone. When the
two differ, the running agent has not switched yet, and `mcc check` says what it is waiting
for. `mcc host show HOST` shows the same from any machine signed in to the account.

Stopping the service is different, and still immediate: `launchctl bootout`, `systemctl stop`
and a shutdown stop what is running. To empty a host first:

```bash
mcc drain --wait
```

takes no new work and returns once nothing is running; `mcc resume` undoes it. Only the
host's owner can drain it: the agent listens for this on `agent.sock` in its state directory,
which only its own account can open.

## Debugging, and running a build of your own

When a runner exits before it takes a job, the failure says what the runner said last
("runner exited with code 1 before taking a job; it said: ..."), in the agent's log, in
`mcc status` and in the dashboard.

For everything, turn on debug logging. In the host's policy:

```json
"log_level": "debug"
```

The agent then logs each line a runner prints before it takes a job, among much else. `mcc run
-v` does the same for one run. With the nix-darwin module, set
`services.mcc-agent.policy.log_level = "debug"`.

To try a build of your own, the service does not have to be stopped. The agent that is
running will become it:

```bash
mcc use ./mcc --debug
```

It finishes what it has, as for an upgrade, and then replaces itself with that build: same
service, same log file, nothing to restart and nothing for macOS to ask permission for.
`mcc check` says when a build that is not the installed one is running.

```bash
mcc use --installed
```

goes back. So does the next update that changes the installed agent. After changing the
policy file, `mcc use --installed` is also how to make the agent read it again.

## Local policy

Nothing outside the host can change its policy: not the API, not `mcc` signed in from another
machine, not an MCP client. Whoever can write the policy file decides what the host accepts.

The control plane cannot override these. A `start` that breaks the policy is answered with a
`failed` status and the reason. `mcc policy` prints the effective policy.

The agent tells the control plane what its policy is each time it connects, with every
default filled in. It reads the policy only when it starts, so what it last said is what
is in force. The control plane uses the report for two things and can do
nothing else with it:

- **Not sending what would be refused.** A job goes only to a host whose policy takes its
  image, its kind of work and, for a runner, the repository or organization it registers
  with. Work that no host of its pool takes waits, and `mcc status` says which host refuses
  it and why (`demand_refused_by_policy`).
- **Showing it.** `mcc host show HOST` prints what each host accepts, from any machine
  signed in to the account.

The report leaves out paths on the host (`cache_dir`, `xcode`) and the commands and
environment of declared tasks; of those the control plane learns only that a cache exists
and the tasks' names. The agent still checks every `start` itself: a control plane that
ignored the report would only be refused.

| Field | Default | Meaning |
|---|---|---|
| `max_vms` | 2 | Concurrent VMs of any kind. Of those, at most two can be macOS guests, which is Apple's rule; Linux guests are limited only by this number. |
| `allowed_images` | any image of the account | Glob patterns on image sources, for example `ghcr.io/cirruslabs/*`. A trailing `*` matches the rest of the source. |
| `excluded_images` | none | Patterns of the same kind for images this host never runs, even ones `allowed_images` matches |
| `allowed_github_urls` | any | Repository or organization URLs a runner may register with; read from the just-in-time config |
| `cpus`, `memory_mb` | an equal share | Per VM. By default the host divided by `max_vms`, and never more than half. |
| `active_hours` | always | Local time window such as `22:00-07:00`; outside it the host reports no capacity |
| `stay_awake` | true | Holds `caffeinate -i` while any VM runs. Mac hosts only. |
| `isolate_network` | true | Internet-only VM networking through Tart's Softnet, when available. Not available on Linux hosts yet. |
| `require_network_isolation` | false | Refuse VMs rather than fall back to shared networking |
| `shared_runner` | true | Keep a current Actions runner on the host and share it into each VM |
| `image_refresh_hours` | 24 | Re-pull images this often; 0 disables |
| `disk_gb` | the image's size | Disk size of each VM. A disk can grow but not shrink, so a value below the image's is ignored. The space is only used as a job fills it. |
| `xcode` | none | Path of an Xcode.app on this host to copy into macOS images; see below. Mac hosts only. |
| `preload_images` | none | Container images to pull into Linux images on this host; see below |
| `cache_dir` | none | Host directory where jobs keep downloads between runs; see below |
| `cache_scope` | `runner` | Who shares a cache: `runner` or `host` |
| `cache_max_gb` | no limit | Above this, the least recently used caches are deleted |
| `share_nix_store` | `false` | Share this host's Nix store with each job's VM, read-only; see [Nix](#nix) |
| `host_tasks` | off | Lets work run on the host itself, without a VM; see [below](#jobs-on-the-host-itself) |
| `tasks` | none | Commands the control plane may ask this host to run, by name; see [below](#tasks) |
| `allowed_workloads` | `["github-runner"]` | The kinds of work this host takes. Add `"script"` to let callers run their own shell scripts in its VMs; see [below](#scripts-in-vms) |
| `script_timeout_seconds` | 3600 | The longest a script may run. A request can ask for less, not more |
| `log_level` | `info` | `debug` logs everything; see [above](#debugging-and-running-a-build-of-your-own) |
| `upgrade_wait_minutes` | 360 | How long running work may hold back a switch to a newer agent that has been installed; 0 for as long as it takes. See [above](#upgrading-and-draining-a-host) |
| `task_output` | 64 MB a run, 1 GB in all, 7 days | How much of what tasks print is kept on this host; see [below](#task-output) |

## Xcode from the host

Images that anyone may download cannot contain Xcode, because Apple's licence does not allow
passing it on. With `"xcode": "/Applications/Xcode.app"` the agent adds your own copy instead:
when it prepares an image, it boots the image once, copies that Xcode onto the VM's disk,
selects it, accepts the licence, runs Xcode's first-launch setup, and shuts
the VM down. Jobs clone the result, so they pay nothing for it.

- It takes a few minutes per image, and again when the image or the host's Xcode changes. The
  host is not ready for the pool until it is done.
- The VM's disk is grown by Xcode's size plus 5 GB.
- Simulator runtimes are not copied. A job that needs one downloads it with
  `xcodebuild -downloadPlatform iOS`.
- The image must allow `sudo` without a password, as ours and Cirrus Labs' do, and its macOS
  must be new enough for that Xcode.
- If the path is wrong or the copy fails, the image is not prepared and the agent logs why.

## Linux guests

A Mac host runs Linux VMs as well as macOS ones, with the same isolation: one fresh VM per job.
A pool whose image has `"platform": "linux"` gets them. They are arm64, like the host, so a
job's own container images must be built for arm64.

Linux guests are not subject to Apple's limit of two, so raise `max_vms` to run more at once;
each VM then gets a smaller share of the host unless you set `cpus` and `memory_mb`.

The image needs the Tart guest agent, `sudo` without a password, and Docker if jobs use
`container:` or `services:`. Ours is `ghcr.io/my-compute-cloud/ubuntu-runner`.

### Container images that are already there

A job that starts `mysql:8.4` as a service normally pulls it first, every time, because its VM
is new. With

```json
{ "preload_images": ["mysql:8.4", "redis:8"] }
```

the agent pulls those into its copy of each Linux image once, when it prepares the image, and
again at every refresh so that the tags stay current. Jobs clone that copy, so their containers
start without a pull. It needs Docker in the image. An image a workflow builds anew for each
run cannot be preloaded; only its unchanged layers would help, and those are not shared yet.

## Linux hosts

On a Linux machine the agent starts Linux VMs with QEMU and KVM, for jobs of the machine's
own architecture: x86_64 or arm64. Each job gets a fresh VM, as on a Mac. Containers are not
used as the boundary between jobs.

### What the host needs

- Hardware virtualization, and the user that runs `mcc` in the `kvm` group (`/dev/kvm`).
- QEMU for the host's architecture, `qemu-img`, `virtiofsd`, an SSH client, `mke2fs` and
  `debugfs`, and `newuidmap`. On arm64 also the UEFI firmware. On Ubuntu 24.04:

  ```bash
  sudo apt-get install qemu-system-x86 qemu-utils virtiofsd openssh-client e2fsprogs uidmap
  ```

  with `qemu-system-arm qemu-efi-aarch64` in place of `qemu-system-x86` on arm64, and

  ```bash
  sudo usermod -aG kvm "$USER"
  ```

- No root. The agent, QEMU and `virtiofsd` all run as the user that runs `mcc`.

`mcc run` logs what is missing when it starts.

### As a service

The release archives for Linux and the install script include a systemd unit,
[`mcc.service`](../agent/packaging/mcc.service). It runs the agent as an unprivileged user
`mcc` whose home, `/var/lib/mcc`, holds the key, state, images and job cache:

```bash
sudo useradd --system --create-home --home-dir /var/lib/mcc --shell /usr/sbin/nologin mcc
```

```bash
sudo install -m 644 /usr/local/share/mcc/mcc.service /etc/systemd/system/mcc.service
```

```bash
sudo -u mcc env MCC_HOME=/var/lib/mcc mcc enroll --server https://<control plane> <token>
```

```bash
sudo systemctl enable --now mcc
```

The unit expects `mcc` in `/usr/local/bin` and does not start until the host is enrolled. It
adds the `kvm` group itself, so the user needs no other setup. Logs are in
`journalctl -u mcc`, and a `policy.json` goes in `/var/lib/mcc`. Stopping the service stops
and deletes its VMs. There is no deb or NixOS module yet.

### Images

A Linux host cannot use an image made for Tart. Its images are OCI artifacts that hold a qcow2
disk, built from the same configuration by the
[images repository](https://github.com/my-compute-cloud/images) (`scripts/build-kvm.sh`, or
`KVM_OUT` with `scripts/build.sh` on a Mac) and published with `scripts/push-kvm.sh`. A tag
can cover both architectures; the host takes the disk for its own. Only images that can be
pulled without a login are supported so far.

A pool may hold both Mac and Linux hosts. Its image then names the Tart image as `source`
and the KVM image under `host_sources.linux`, and the control plane sends each host the one
for its system (see [operations](operations.md)). `allowed_images` and `excluded_images` are checked against the
source the host is sent.

A pool's image source may also be the full path of a directory on the host that holds
`disk.qcow2` and `config.json`, which is what the build leaves: an image built on the host
needs no registry.

What an image must have: a user for jobs with `sudo` without a password, an SSH server, the
`mcc-guest-access` service that the images repository installs (it lets in the key the host
makes for each VM, and only that key), and Docker if jobs use `container:` or `services:`.

### What differs from a Mac host

| | Mac host | Linux host |
|---|---|---|
| Guests | macOS and Linux, arm64 | Linux, of the host's architecture |
| Limit | Two macOS guests; Linux guests up to `max_vms` | `max_vms` |
| `xcode` | Copies the host's Xcode into macOS images | Ignored |
| `preload_images`, `cache_dir`, `disk_gb` | As described here | The same |
| `stay_awake` | Holds off sleep | Does nothing |
| Network isolation | Tart's Softnet, when set up | Not available yet: see below |

A job's cache is shared through `virtiofsd`. What a job writes there belongs, on the host,
to the user that runs `mcc`. In the VM it is the job's user's; a process running as some other
user, as in some containers, may not be able to write to it. The runner reaches the VM as a
read-only disk, not as a directory, so no job can change it for the next.

**Network.** A VM reaches the internet through QEMU's user-mode network. It can also reach
the host's local network and services that listen on the host's loopback address, such as a
database you run for development. There is no setting that stops this yet, so
`isolate_network` has no effect on a Linux host, and with `require_network_isolation` the
host refuses jobs. Do not run jobs you do not trust on a Linux host whose local services
trust their network.

**Memory for shared caches.** With `cache_dir` set, a VM's memory is shared with `virtiofsd`.
If the host has enough free huge pages reserved (`vm.nr_hugepages`), the agent uses them.
That is optional on real hardware and required where the host is itself a VM.

## Job caches

A job's VM is deleted when the job ends, and its downloads with it. With `cache_dir` set, each
job gets one subdirectory of it as a writable share, and the entry command sets `MCC_CACHE_DIR`
to it. What uses it is up to the image: ours points the download caches of npm, Yarn, pip, uv,
Go modules, NuGet and Homebrew there, in `/etc/mcc/job-env`, which the entry command reads if
the image has it.

This is the one way a job can leave something for a later job, so decide who shares:

- `cache_scope: "runner"` (the default) keeps one cache per repository or organization that
  runners register with. Runners for a personal account register per repository; runners for
  an organization register with the organization, so all its repositories share one.
- `cache_scope: "host"` gives every job on the host the same cache. Only for hosts where you
  trust every job.

Within a scope, any job can write what a later job reads, including a job for a pull request
from a fork. Leave `cache_dir` unset on hosts that run code from people you do not trust.

Caches are plain directories under `cache_dir/data`; deleting one only costs the next job its
downloads. With `cache_max_gb`, the agent deletes whole caches, least recently used first, when
the total passes the limit, and never one a running job is using.

## Nix

A job that builds with Nix starts in a new VM with an empty store of its own, and would fetch
or build everything again. Two things spare it that. Both apply to images that have Nix, such
as ours, and need nothing in the workflow.

**The job's cache keeps what jobs use.** With `cache_dir` set, Nix in the VM writes each
thing it builds into `nix/` in the job's cache as soon as it is built, and takes from there
what an earlier job of the same repository had. A job that fails late still leaves what it
finished. When a job ends, everything else it added to its store is written there too: the
sources it fetched, and what it copied from the host's store or downloaded. A job whose VM
is stopped before it ends leaves only what it built.

- The cache holds a second copy of what a repository's jobs take from the host's store:
  about 13 GB for a job that builds a whole system. A path no job has used for 14 days is
  deleted; `cache_max_gb` still bounds all caches together.
- A path keeps the signatures it came with. So a path from `cache.nixos.org` is still
  known as such in a later job that takes it from the cache, and a step that publishes
  what a job built can tell it from what the job built itself.
- Writing it takes time after the job has ended, during which its VM still holds a slot:
  about two minutes for those 13 GB the first time, seconds once the cache has them.

Who shares a cache is who shares these builds, and nothing in it is signed, so
the warning under [Job caches](#job-caches) holds here with more weight: a job can leave a
build for a later one to run.

**The host's own store, read-only.** With `"share_nix_store": true`, each VM is given the
host's `/nix/store` as a share it cannot write to, and Nix in the VM copies from it what the
host already has instead of downloading or building it. A host that has built its own system
gives a job building the same system nearly all of it. The job still has its own store; it
never uses the host's in place, and cannot change it.

- Every job on the host can read everything in the host's store. Do not turn it on where the
  store holds what a job should not see.
- The agent needs `sqlite3` on the host (macOS has it) to give each job a consistent copy of
  the store's database, made when the job starts.
- It relies on Nix's `read-only-local-store` feature, which Nix calls experimental. If the
  Nix in an image refuses it, the job runs as if the setting were off.

Nix tries the cache first, then the host's store, then the caches the image or the workflow
configured. The cache comes first because a path in it is one file, while the host's store
is read file by file through the share. Copying a system of 13 GiB into a new VM took 94
seconds from the host's store and 25 from the cache; the source of nixpkgs, some ninety
thousand small files, took 44 seconds and 6. So the host's store serves the first job that
needs a path, and the cache every job after it.

An image without Nix, where a workflow installs it, gets the same from `MCC_NIX_CONFIG`: the
entry command leaves the settings there, and a step can add them after installing:

```yaml
- run: |
    if [ -n "${MCC_NIX_CONFIG:-}" ]; then
      printf '%s\n' "$MCC_NIX_CONFIG" | sudo tee -a /etc/nix/nix.conf
      sudo launchctl kickstart -k system/org.nixos.nix-daemon
    fi
```

## Jobs on the host itself

A VM for every job costs a boot and starts from nothing. Where you trust the work and the
machine's own state is the point (a warm checkout, installed toolchains, a GPU), a pool can
run its jobs on the host itself instead: create it with `"isolation": "host"` and no image.
A host only takes such work if its policy says so:

```json
"allowed_github_urls": ["https://github.com/your-org"],
"host_tasks": {
  "enabled": true,
  "github_runner": true,
  "user": "mcc-job",
  "dir": "/Users/mcc-job/work",
  "max": 2,
  "workspace": "fresh"
}
```

| Setting | Default | Meaning |
|---|---|---|
| `enabled` | false | Whether this host runs work without a VM at all |
| `github_runner` | false | Whether GitHub Actions jobs may. A workflow is arbitrary code, so this is its own yes, and it needs `allowed_github_urls`: without a list, nothing is allowed here |
| `user` | none | The account work runs as. The agent enters it with `sudo -n`, so the agent's account must be allowed to run commands as it without a password |
| `allow_same_user` | false | Run work as the agent's own account instead. One of this and `user` must be set |
| `dir` | `work/` in the agent's directory | Where each job gets its directory. Required with `user`, and that account must be able to write to it |
| `max` | 1 | Jobs on the host at once. Counted apart from `max_vms` |
| `workspace` | `fresh` | `fresh` or `persistent`; see below |

**This is not isolation.** A job on the host can do whatever its account can. Run as the
agent's own account, that includes reading the agent's key, editing this policy and changing
the runner that VM jobs are given, which is why `allow_same_user` has to be said out loud. A
dedicated account with nothing of yours in its reach is the better choice.

What happens to a job:

1. The agent makes `job-<assignment id>` in `dir` and copies the host's current runner into it.
   The runner is still registered for one job only.
2. The runner starts there, with its configuration in its environment (which the runner reads
   and clears), never on its command line. With `cache_dir` set, `MCC_CACHE_DIR` names the
   job's cache. A job run as `user` has that account's login environment; one run as the
   agent's account has the agent's `PATH` and `HOME` and nothing else of its environment.
3. When the runner exits, the agent kills whatever the job started that is still in its
   process group, and deletes the directory. With `user`, and no other job running as it, the
   agent also kills everything that account still runs.

What the agent cannot clean up: a process that left the job's group when work runs as the
agent's own account, and anything written outside the job's directory. `fresh` promises a
clean directory, not a clean machine.

`"workspace": "persistent"` keeps the runner's work folder between jobs, in `dir/keep`, one per
repository or organization that runners register with, so the next job finds its checkout and
tool downloads. Two jobs never use one at the same time; a second concurrent job gets its own.
Downloaded actions and temporary files are deleted before and after every job, because the
next job would run a cached action as it found it. The checkout is not: a job can leave a
change there, a git hook for one, for the next job of the same repository. Use it where every
job that shares a workspace is equally trusted, which for a repository that builds pull
requests from forks it is not.

There is no network isolation for such work, so a host with `require_network_isolation`
refuses it. `cpus`, `memory_mb` and `disk_gb` do not apply.

## Tasks

A task is a command you write in this host's policy and give a name. Someone with an API token
for your account can then ask for it by that name
([operations.md](operations.md#running-tasks)), and that is all they can ask for: a request
carries a task's name and values for its parameters, never a command. Tasks run on the host
itself, so they need `host_tasks`, and they run as the account it names.

```json
"host_tasks": { "enabled": true, "user": "mcc-job", "dir": "/Users/mcc-job/work" },
"tasks": {
  "test-ios": {
    "command": ["/usr/bin/make", "-C", "/Users/mcc-job/app", "test"],
    "params": { "SCHEME": "[A-Za-z0-9_-]{1,40}" },
    "timeout_seconds": 3600,
    "max_concurrent": 1
  }
}
```

| Setting | Default | Meaning |
|---|---|---|
| `command` | required | The program, by its full path, and its arguments. It is run as written; no shell reads it, so `$HOME` or `;` in an argument is just text |
| `params` | none | The parameters a request may set, each with a pattern its value must match from start to end. A request that sets any other parameter is refused. A request may leave one out |
| `timeout_seconds` | 3600 | The longest a run may last. A request can ask for less, not more |
| `max_concurrent` | no limit of its own | Runs of this task at once. `host_tasks.max` still limits everything on the host together |

A parameter reaches the command as the environment variable `MCC_PARAM_<NAME>`, and never
becomes part of a command line, so a value cannot add an argument or a second command. What
the command does with the value is yours to get right: a script that passes `$MCC_PARAM_REF`
to `git checkout` unquoted has let the caller choose its options. Keep patterns narrow.
Parameter values are not secrets: on a host with `user` set they can be seen in the process
list for a moment while the task starts.

A run gets `job-<assignment id>` in `host_tasks.dir` as its working directory, named in
`MCC_JOB_DIR` and deleted afterwards, and `MCC_TASK` holds the task's name. The command is
free to work anywhere else its account can, which is the point of a task: your checkout, your
toolchains, your devices. Cleanup is the same as for a job on the host.

The host tells the control plane the names of its tasks, their time limits and
`max_concurrent`, so that a task is only sent to a host that has it. It does not send the
commands or the patterns. A run ends in one of these ways:

| Outcome | What the agent reports |
|---|---|
| The command exits | `finished`, with its exit code |
| It runs past its time limit | `failed`, code `timeout`; the agent kills it |
| It is cancelled, or the agent shuts down | `failed`, code `stopped` |
| The task is not in the policy, or a parameter is not allowed | `failed`, code `refused_policy`; nothing runs |
| No free slot, or `max_concurrent` reached | `failed`, code `refused_capacity`; nothing runs |

### Scripts in VMs

A declared task is a command you wrote. A script is one the caller wrote: a shell script sent
with the request. It is the only free-form work the control plane can send, so it is fenced
twice. It runs only inside a VM, from a pool's image, deleted afterwards like a job's VM; a
request for one on the host itself is refused. And the host takes scripts only if you say so:

```json
"allowed_workloads": ["github-runner", "script"],
"script_timeout_seconds": 1800
```

`allowed_workloads` is the whole list of what the host accepts, and without it that is GitHub
runners only, so an agent upgrade never widens what the host does. Declared tasks are not in
the list: declaring one is the yes.

The script reaches the VM on stdin and is run with `bash`. It is never an argument of a
process on the host. It gets no Actions runner and no job cache, and the same VM limits
(`max_vms`, `cpus`, `memory_mb`, network isolation) as a job. Its output is kept and read as
a task's is, below, and it ends in the same ways.

### Task output

What a task prints, on stdout and stderr together, is kept on the host that ran it, in
`task-output/` in the agent's directory. None of it is sent to the control plane.

```json
"task_output": { "max_mb": 64, "total_mb": 1024, "keep_days": 7 }
```

| Setting | Default | Meaning |
|---|---|---|
| `max_mb` | 64 | The most kept of one run. A task that prints more keeps running; what is past the limit is dropped, and the run is marked as cut short |
| `total_mb` | 1024 | The most kept of all runs together. Past it, the runs that ended longest ago are deleted |
| `keep_days` | 7 | How long a run's output is kept after it ends |
| `upload` | false | Send finished output to the control plane to be kept, on plans that keep it; see [below](#letting-the-control-plane-keep-output) |

A task that is still running is never deleted. Read a task's output on the host with its id
from the API:

```bash
mcc logs -f 3f6c1f0e-5d0b-4a43-9a51-0c9f6f1f6a55
```

Without `-f` it prints what there is and returns; with it, it follows the task until it ends.
`--offset N` starts N bytes in, to pick up after an interrupted read. If the task ended with
an exit code other than 0, `mcc logs` exits with that code.

`mcc logs` talks to the running agent over a Unix socket, `logs.sock` in the agent's
directory, which only the agent's own account can open. The socket speaks HTTP, so another
program on the host, an agent harness for one, can read output without `mcc`:

```bash
curl --unix-socket "$MCC_HOME/logs.sock" "http://mcc/v1/tasks/$TASK/output?offset=0&follow=1"
```

| Request | Answer |
|---|---|
| `GET /v1/tasks/<id>` | The run's record as JSON: when it started and ended, exit code, size kept |
| `GET /v1/tasks/<id>/output?offset=N&follow=1` | The output from byte N on. With `follow=1` the response stays open until the task ends. It ends with trailers `X-Mcc-Exit-Code`, `X-Mcc-Code` (why there is no exit code) and `X-Mcc-Truncated` |

On the owner's socket no secret is needed: its owner can read the files anyway. A request
there that does show a secret, as `Authorization: Bearer <secret>` or in `MCC_TASK_SECRET`
for `mcc logs`, must show the right one.

### Reading output from another machine

By default a host accepts no connections at all. To let whoever asked for a task read its
output from where they are, without the output passing through the control plane, give the
host an address to listen on:

```json
"task_output": { "listen": "0.0.0.0:7443", "address": "studio-01.example.com:7443" }
```

| Setting | Default | Meaning |
|---|---|---|
| `listen` | none | The address and port to listen on. Without it nothing listens |
| `address` | the host's name and the port of `listen` | What callers are told to connect to, if that differs: a name they can resolve, or the outside of a port forward |

This is the one port the agent ever opens, and it serves one thing: a task's output, over
TLS, to a request that shows that task's secret. It answers the same way for a wrong secret
as for a task that never ran here. A task whose caller chose no secret cannot be read
through it by anyone. Reaching the port from outside your network is yours to arrange (a
firewall rule, a port forward, a VPN); the agent does not.

How a caller is recognised:

1. The caller makes up a secret and gives the control plane its SHA-256 when it asks for the
   task ([operations.md](operations.md#running-tasks)). The hash comes to this host with the
   task. The control plane never has the secret.
2. The listener has a key of its own, in `output.key` in the agent's directory, made the first
   time it is needed. The agent tells the control plane the key's pin (the SHA-256 of the
   public key), and the API passes the pin and `address` on to the caller.
3. The caller connects, checks that the server has the key with that pin, and only then sends
   the secret. There is no certificate authority involved; the certificate is self-signed
   and the pin is the whole of the trust.

From the other machine:

```bash
MCC_TASK_SECRET=$SECRET mcc logs -f --host studio-01.example.com:7443 --pin sha256//<pin> <task id>
```

```bash
curl -k --pinnedpubkey "sha256//<pin>" -H "Authorization: Bearer $SECRET" \
  "https://studio-01.example.com:7443/v1/tasks/<task id>/output?follow=1"
```

`-k` only tells curl not to look for a certificate authority; `--pinnedpubkey` is still
enforced, and curl sends nothing to a server with another key.

### Reading output through the control plane

A caller who cannot reach the host at all, because it is behind a NAT or you would rather open
no port, can read the same output through the control plane:

```bash
MCC_API_TOKEN=$API MCC_TASK_SECRET=$SECRET mcc logs -f --server https://api.mycompute.cloud <task id>
```

This needs no setting on the host: it uses the connection the agent already holds. What is
sent is sealed between the caller and the host, so the control plane carries it without
being able to read it:

1. The caller makes a one-time key and agrees a shared key with the host's output key, the
   one the pin is of. `output.key` exists on every host that keeps task output, whether or
   not it listens.
2. The caller sends the task's secret sealed under the shared key. The host checks it against
   the hash that came with the task, as the listener does.
3. The host sends the output in parts, each sealed under the shared key and tied to its place
   in the stream. `mcc logs` refuses a part that is out of order or does not open.

Parts are sent only while a caller is attached, no more than ten a second per stream, and at
most eight streams at once per host. Nothing is written down on the way. A task without a
secret cannot be read this way.

The control plane can still cut a stream short. It cannot read, change or reorder one unless
it also misled the caller about the host's key, which is what the pin guards against: pass
`--pin` with the value from `mcc pin` and `mcc logs` checks the key against it before
sending anything.

### Letting the control plane keep output

Everything above leaves output on the host. You can also let the host send a finished task's
output to the control plane, to be kept and read later with only an API token:

```json
"task_output": { "upload": true }
```

This is off by default, and it is different in kind: stored output is something we can read.
The host uploads only when the tenant's plan keeps output (the control plane says so with
each task), signs the upload with its host key, and sends at most 64 MB of a run. Plans that
keep none are never sent any.

The pin reaches a caller through the control plane, so a caller who takes it from there is
trusting the control plane to pass on the right one: a control plane that lied about the
address and the pin could collect the secret. A caller who would rather not rely on that
gets the pin from you instead. `mcc pin` prints the address and the pin on the host.

## Network isolation

By default a Tart VM shares the host's network and can reach other machines on it. With
`isolate_network` the agent passes `--net-softnet`, which limits VMs to internet access. Softnet
must run as root without a password prompt: make the `softnet` binary setuid root, as described
in Tart's FAQ. The agent checks for that at start-up and logs a warning, then falls back to
shared networking unless `require_network_isolation` is set.

## What a job looks like on the host

1. The image's base VM (`mcc-base-<hash>`, with a suffix when the host adds Xcode or container
   images to it) is cloned once per image; each job clones it to
   `mcc-<assignment id>` (copy-on-write on APFS). The agent writes down each job VM it makes,
   in `tart/jobs` in its state directory, and stops or deletes only those. Your own VMs are
   left alone whatever they are called, and after a crash it cleans up by that record, not by
   name.
2. The clone boots headless; the agent waits for the Tart guest agent (`tart exec`). Images must
   include the guest agent, which our images (`ghcr.io/my-compute-cloud/macos-runner`) and all
   non-vanilla Cirrus Labs images do. No SSH is used. A job's `PATH` and environment are the
   guest agent's, because the entry command's shell reads no profile.
3. The fixed entry command (one for macOS guests, one for Linux, which first mounts the
   host's shares) copies the shared runner into the guest (or uses `~/actions-runner`
   from the image), reads `/etc/mcc/job-env` if the image has one, and runs
   `run.sh --jitconfig`, with the configuration on stdin so it never appears in the host's
   process list.
4. When the runner exits, or the control plane sends `stop`, or the runner has not taken a job
   within the idle timeout, the VM is stopped and deleted, and only then is the final status sent.
5. On start-up the agent deletes any `mcc-*` job VMs left from a crash.

On a Linux host the steps are the same with other means:

1. The image's disk is fetched once to `kvm/images/mcc-base-<hash>` (with a suffix when the
   host adds container images to it). Each job gets a qcow2 overlay of it in
   `kvm/vms/mcc-<assignment id>`, and a new SSH key.
2. QEMU boots the overlay headless. The agent waits until the VM accepts the key, over a port
   forwarded to the host's loopback address, then mounts the runner disk and the cache in the
   guest under `/mnt/mcc`. A job's `PATH` and environment are those of a command run over SSH,
   plus what `/etc/mcc/job-env` sets.
3. The same entry command runs over SSH, with the configuration on stdin.
4. The VM is ended and its directory deleted, and only then is the final status sent.
5. On start-up the agent ends any QEMU left from a crash and deletes its directory.
