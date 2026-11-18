# mcc — the My Compute Cloud agent

`mcc` is the program you run on a machine you own to make it part of your
[My Compute Cloud](https://mycompute.cloud) fleet. It runs each GitHub Actions job in a fresh
virtual machine and deletes the machine when the job ends.

This repository is the agent's complete source, so you can read what it does before you run it.

## What it does on your machine

- **Connects outbound only.** It opens one WebSocket to `api.mycompute.cloud`. It never listens
  for connections, so nothing needs a public address or an open port.
- **Keeps its key local.** Enrolling generates an Ed25519 key on the machine. Only the public
  key is sent; every connection is authenticated by signing a fresh challenge.
- **Runs jobs in throwaway VMs.** Each job gets a clone of a base image, started with
  [Tart](https://tart.run) on a Mac or QEMU and KVM on a Linux host, and deleted afterwards.
  Jobs do not run on the host itself.
- **Obeys a local policy the service cannot override.** You decide how many VMs may run, which
  images and which GitHub organizations are allowed, and during which hours. See
  [docs/agent.md](docs/agent.md).

It does not read your files, does not run as root, and does not update itself.

## Requirements

An Apple Silicon Mac with [Tart](https://tart.run) installed, or an x86_64 or arm64 Linux
host with KVM and QEMU; [docs/agent.md](docs/agent.md#linux-hosts) lists what a Linux host
needs and how it differs. A Linux host cannot yet keep a job's VM off the local network, so
use one only for jobs you trust. Windows hosts are planned.

## Install

Homebrew:

```bash
brew install my-compute-cloud/tap/mcc
```

Install script (downloads the latest release, checks its checksum and, on a Mac, its
signature, and puts `mcc` in `/usr/local/bin`):

```bash
curl -fsSL https://mycompute.cloud/install.sh | sh
```

Nix:

```bash
nix profile install github:my-compute-cloud/mcc
```

There is also a nix-darwin module that runs the agent as a launchd service, and a systemd
unit for Linux hosts; see [docs/agent.md](docs/agent.md).

Then add the host from the dashboard at <https://mycompute.cloud/dashboard>, which gives you a
single-use `mcc enroll` command, and start it with `mcc run`.

## Verify a download

The Mac binary is signed with an Apple Developer ID and notarized by Apple, and every release
file, including the Linux ones, carries GitHub build provenance.

```bash
codesign --verify --strict --verbose=2 "$(command -v mcc)"
```

```bash
gh attestation verify mcc-darwin-arm64.tar.gz --repo my-compute-cloud/mcc
```

`checksums.txt` on each release lists the SHA-256 of every file.

## Build from source

```bash
cd agent && go build -o mcc ./cmd/mcc
```

## Remove

Stop `mcc run`, delete the binary, and delete `~/Library/Application Support/mcc` (the key and
state; `~/.config/mcc` on Linux, or `/var/lib/mcc` and the `mcc` user if you used the systemd
unit). Revoke the host in the dashboard so its key is forgotten there too.

## Licence

[PolyForm Shield 1.0.0](LICENSE): you may use, modify and run this software for any purpose
except providing a product that competes with My Compute Cloud.

## Contributing

This repository is published from the project's main repository, so pull requests cannot be
merged here directly. Issues are welcome.
