#!/bin/sh
# Installs mcc, the My Compute Cloud agent.
#
#   curl -fsSL https://mycompute.cloud/install.sh | sh
#
# It downloads the latest release from github.com/my-compute-cloud/mcc, checks the file against
# the release's checksums and Apple's signature, and installs it. Nothing else is changed on
# the machine. Set MCC_INSTALL_DIR to install somewhere other than /usr/local/bin.
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
	*) fail "mcc does not support $os yet. Linux and Windows hosts are planned." ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "Downloading $asset"
curl -fsSL "$BASE/$asset" -o "$tmp/$asset" || fail "could not download $BASE/$asset"
curl -fsSL "$BASE/checksums.txt" -o "$tmp/checksums.txt" || fail "could not download the checksums"

want=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "the release's checksums do not list $asset"
got=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
[ "$want" = "$got" ] || fail "checksum mismatch for $asset: expected $want, got $got"
say "Checksum matches"

tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/mcc" ] || fail "the archive does not contain mcc"

codesign --verify --strict "$tmp/mcc" 2>/dev/null || fail "the binary's signature is not valid"
signer=$(codesign -dvv "$tmp/mcc" 2>&1 | awk -F= '/^Authority=Developer ID Application/ { print $2; exit }')
[ -n "$signer" ] || fail "the binary is not signed with an Apple Developer ID"
say "Signed by: $signer"

if [ -d "$DIR" ] && [ -w "$DIR" ]; then
	install -m 755 "$tmp/mcc" "$DIR/mcc"
else
	say "Installing to $DIR needs administrator rights"
	sudo mkdir -p "$DIR"
	sudo install -m 755 "$tmp/mcc" "$DIR/mcc"
fi

say "Installed mcc $("$DIR/mcc" version) to $DIR/mcc"
command -v tart >/dev/null 2>&1 || say "Note: mcc runs jobs with Tart, which is not installed. See https://tart.run"
say "Next: add this host from https://mycompute.cloud/dashboard"
