#!/bin/sh
# Installs mcc, the My Compute Cloud agent.
#
#   curl -fsSL https://mycompute.cloud/install.sh | sh
#
# It downloads the latest release from github.com/my-compute-cloud/mcc, checks the file against
# the release's checksums and, on a Mac, Apple's signature, and installs it. On Linux it also
# leaves a systemd unit in /usr/local/share/mcc, which does nothing until you install it.
# Nothing else is changed on the machine. Set MCC_INSTALL_DIR to install somewhere other than
# /usr/local/bin.
set -eu

REPO="my-compute-cloud/mcc"
BASE="https://github.com/$REPO/releases/latest/download"
DIR="${MCC_INSTALL_DIR:-/usr/local/bin}"

say() { printf '%s\n' "$*"; }
fail() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

os=$(uname -s)
arch=$(uname -m)
case "$os/$arch" in
	Darwin/arm64) asset="mcc-darwin-arm64.tar.gz" ;;
	Darwin/*) fail "mcc needs an Apple Silicon Mac; this one is $arch." ;;
	Linux/x86_64) asset="mcc-linux-amd64.tar.gz" ;;
	Linux/aarch64 | Linux/arm64) asset="mcc-linux-arm64.tar.gz" ;;
	Linux/*) fail "mcc runs on x86_64 and arm64 Linux hosts; this one is $arch." ;;
	*) fail "mcc does not support $os yet. Windows hosts are planned." ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "Downloading $asset"
curl -fsSL "$BASE/$asset" -o "$tmp/$asset" || fail "could not download $BASE/$asset"
curl -fsSL "$BASE/checksums.txt" -o "$tmp/checksums.txt" || fail "could not download the checksums"

want=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "the release's checksums do not list $asset"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
else
	got=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
fi
[ "$want" = "$got" ] || fail "checksum mismatch for $asset: expected $want, got $got"
say "Checksum matches"

tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/mcc" ] || fail "the archive does not contain mcc"

if [ "$os" = Darwin ]; then
	codesign --verify --strict "$tmp/mcc" 2>/dev/null || fail "the binary's signature is not valid"
	signer=$(codesign -dvv "$tmp/mcc" 2>&1 | awk -F= '/^Authority=Developer ID Application/ { print $2; exit }')
	[ -n "$signer" ] || fail "the binary is not signed with an Apple Developer ID"
	say "Signed by: $signer"
fi

if [ -d "$DIR" ] && [ -w "$DIR" ]; then
	install -m 755 "$tmp/mcc" "$DIR/mcc"
else
	say "Installing to $DIR needs administrator rights"
	sudo mkdir -p "$DIR"
	sudo install -m 755 "$tmp/mcc" "$DIR/mcc"
fi

say "Installed mcc $("$DIR/mcc" version) to $DIR/mcc"
if [ "$os" = Darwin ]; then
	command -v tart >/dev/null 2>&1 || say "Note: mcc runs jobs with Tart, which is not installed. See https://tart.run"
else
	if [ -f "$tmp/mcc.service" ]; then
		share="${MCC_SHARE_DIR:-/usr/local/share/mcc}"
		if mkdir -p "$share" 2>/dev/null && [ -w "$share" ]; then
			install -m 644 "$tmp/mcc.service" "$share/mcc.service"
		else
			sudo mkdir -p "$share"
			sudo install -m 644 "$tmp/mcc.service" "$share/mcc.service"
		fi
		say "A systemd unit is in $share/mcc.service; its first lines say how to use it"
	fi
	say "Note: mcc runs jobs with QEMU and KVM. See https://github.com/$REPO/blob/main/docs/agent.md#linux-hosts"
fi
say "Next: add this host from https://mycompute.cloud/dashboard"
