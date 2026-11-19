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

Add the host from the dashboard at <https://mycompute.cloud/dashboard>, which gives you a single-use command, then on
the host:

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

## Local policy

The control plane cannot override these. A `start` that breaks the policy is answered with a
`failed` status and the reason. `mcc policy` prints the effective policy.

| Field | Default | Meaning |
|---|---|---|
| `max_vms` | 2 | Concurrent VMs of any kind. Of those, at most two can be macOS guests, which is Apple's rule; Linux guests are limited only by this number. |
| `allowed_images` | any image in the pool configuration | Glob patterns on image sources, for example `ghcr.io/cirruslabs/*` |
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
| `host_tasks` | off | Lets work run on the host itself, without a VM; see [below](#jobs-on-the-host-itself) |
| `tasks` | none | Commands the control plane may ask this host to run, by name; see [below](#tasks) |

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
for its system (see [operations](operations.md)). `allowed_images` is checked against the
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

For now a task's output goes to the agent's log at debug level and nowhere else.

## Network isolation

By default a Tart VM shares the host's network and can reach other machines on it. With
`isolate_network` the agent passes `--net-softnet`, which limits VMs to internet access. Softnet
must run as root without a password prompt: make the `softnet` binary setuid root, as described
in Tart's FAQ. The agent checks for that at start-up and logs a warning, then falls back to
shared networking unless `require_network_isolation` is set.

## What a job looks like on the host

1. The image's base VM (`mcc-base-<hash>`, with a suffix when the host adds Xcode or container
   images to it) is cloned once per image; each job clones it to
   `mcc-<assignment id>` (copy-on-write on APFS).
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
