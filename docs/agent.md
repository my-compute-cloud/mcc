# Agent

The agent (`mcc`) runs on the host. It opens one outbound WebSocket to the control plane,
reports capacity, prepares images, and runs each assignment in a fresh Tart VM that it deletes
afterwards. It never accepts inbound connections.

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
  latest release, checks it against the release's checksums and Apple's signature, and installs
  it to `/usr/local/bin`.
- **From source:** `cd agent && go build -o mcc ./cmd/mcc`

Release binaries are signed with an Apple Developer ID, notarized, and carry GitHub build
provenance: `gh attestation verify mcc-darwin-arm64.tar.gz --repo my-compute-cloud/mcc`.

## Enroll

Add the host from the dashboard at <https://mycompute.cloud/dashboard>, which gives you a single-use command, then on
the host:

```bash
mcc enroll --server https://<control plane> <token>
```

The token is single use and expires after 15 minutes. Enrollment generates an Ed25519 key in
the state directory; only the public key is sent. Every connection is authenticated by signing
a fresh challenge. Revoking the host in the control plane deletes its public key.

State lives in `~/Library/Application Support/mcc` (override with `MCC_HOME`):

| File | Holds |
|---|---|
| `identity.key` | The private key, mode 0600 |
| `state.json` | Server URL and host id |
| `policy.json` | Local policy (optional; `MCC_POLICY_FILE` points elsewhere) |
| `runner/` | Cached GitHub Actions runner, shared read-only into VMs |

## Local policy

The control plane cannot override these. A `start` that breaks the policy is answered with a
`failed` status and the reason. `mcc policy` prints the effective policy.

| Field | Default | Meaning |
|---|---|---|
| `max_vms` | 2 | Concurrent VMs. macOS guests are also capped at two by Apple's licence. |
| `allowed_images` | any image in the pool configuration | Glob patterns on image sources, for example `ghcr.io/cirruslabs/*` |
| `allowed_github_urls` | any | Repository or organization URLs a runner may register with; read from the just-in-time config |
| `cpus`, `memory_mb` | half the host | Per VM |
| `active_hours` | always | Local time window such as `22:00-07:00`; outside it the host reports no capacity |
| `stay_awake` | true | Holds `caffeinate -i` while any VM runs |
| `isolate_network` | true | Internet-only VM networking through Tart's Softnet, when available |
| `require_network_isolation` | false | Refuse VMs rather than fall back to shared networking |
| `shared_runner` | true | Keep a current Actions runner on the host and share it into each VM |
| `image_refresh_hours` | 24 | Re-pull images this often; 0 disables |

## Network isolation

By default a Tart VM shares the host's network and can reach other machines on it. With
`isolate_network` the agent passes `--net-softnet`, which limits VMs to internet access. Softnet
must run as root without a password prompt: make the `softnet` binary setuid root, as described
in Tart's FAQ. The agent checks for that at start-up and logs a warning, then falls back to
shared networking unless `require_network_isolation` is set.

## What a job looks like on the host

1. The image's base VM (`mcc-base-<hash>`) is cloned once per image; each job clones it to
   `mcc-<assignment id>` (copy-on-write on APFS).
2. The clone boots headless; the agent waits for the Tart guest agent (`tart exec`). Images must
   include the guest agent, which all non-vanilla Cirrus Labs images do. No SSH is used.
3. The fixed entry command copies the shared runner into the guest (or uses `~/actions-runner`
   from the image) and runs `run.sh --jitconfig`, with the configuration on stdin so it never
   appears in the host's process list.
4. When the runner exits, or the control plane sends `stop`, or the runner has not taken a job
   within the idle timeout, the VM is stopped and deleted, and only then is the final status sent.
5. On start-up the agent deletes any `mcc-*` job VMs left from a crash.
