#!/bin/sh
# Installs Zelie on a Debian or Ubuntu server:
#
#   curl -fsSL https://github.com/Caria-Core/zelie/releases/latest/download/install.sh | sh
#
# It downloads the release for this machine, checks that the release was
# signed with Zelie's key, and runs the setup, which asks a few questions.
# Set ZELIE_VERSION=v0.1.0 to install a given release instead of the latest.
set -eu

public_key='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAnD7qS2H0VY9ZdJfeHbXlvhbEEkTNQNuFCDuz9XLonUk=
-----END PUBLIC KEY-----'

fail() {
	echo "zelie: $*" >&2
	exit 1
}

[ "$(id -u)" = 0 ] || fail "run this as root"
case $(uname -m) in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "Zelie runs on amd64 and arm64, and this is $(uname -m)" ;;
esac
for tool in curl openssl sha256sum; do
	command -v "$tool" >/dev/null || fail "$tool is missing; install it with: apt install $tool"
done

if [ -n "${ZELIE_VERSION:-}" ]; then
	base="https://github.com/Caria-Core/zelie/releases/download/$ZELIE_VERSION"
else
	base="https://github.com/Caria-Core/zelie/releases/latest/download"
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading Zelie for $arch…"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
curl -fsSL -o "$tmp/SHA256SUMS.sig" "$base/SHA256SUMS.sig"
curl -fsSL -o "$tmp/zelie-linux-$arch" "$base/zelie-linux-$arch"

# The list of checksums carries the signature, and the binary must match
# its line in the list.
printf '%s\n' "$public_key" >"$tmp/key.pem"
openssl pkeyutl -verify -pubin -inkey "$tmp/key.pem" -rawin \
	-in "$tmp/SHA256SUMS" -sigfile "$tmp/SHA256SUMS.sig" >/dev/null 2>&1 ||
	fail "the release's signature does not match Zelie's key; nothing was installed"
(cd "$tmp" && grep " zelie-linux-$arch\$" SHA256SUMS | sha256sum -c --status) ||
	fail "the download does not match the signed checksum; nothing was installed"
echo "Signature checked."

install -m 755 "$tmp/zelie-linux-$arch" /usr/local/bin/zelie
# The setup asks questions, and this script's own input is the pipe from
# curl. Without a terminal, as from automation, the flags must say it all.
if (: </dev/tty) 2>/dev/null; then
	exec /usr/local/bin/zelie install "$@" </dev/tty
fi
exec /usr/local/bin/zelie install "$@" </dev/null
