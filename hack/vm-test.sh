#!/bin/sh
# Runs the integration tests as root inside the Lima VM used for development.
# The tests are compiled here and copied in, so the VM needs no Go toolchain.
set -eu

vm=${ZELIE_VM:-zelie}
arch=$(limactl shell "$vm" -- uname -m)
case $arch in
	aarch64) goarch=arm64 ;;
	x86_64) goarch=amd64 ;;
	*) echo "unsupported architecture $arch" >&2; exit 1 ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cd "$(dirname "$0")/.."
GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -o "$tmp/zelie" ./cmd/zelie
for pkg in engine core build backup; do
	GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go test -c -tags integration -o "$tmp/$pkg.test" "./internal/$pkg"
done

# /tmp in the VM is cleared on reboot.
limactl shell "$vm" -- mkdir -p /tmp/zelie-test
for f in "$tmp"/*; do
	limactl copy "$f" "$vm:/tmp/zelie-test/$(basename "$f")"
done

limactl shell "$vm" -- sudo /tmp/zelie-test/zelie engine install
for pkg in engine core build backup; do
	echo "== $pkg"
	limactl shell "$vm" -- sudo /tmp/zelie-test/$pkg.test -test.v -test.count=1 "$@"
done
