#!/bin/sh
set -eu
if [ "$(uname -s)" != Linux ] || [ "$(uname -m)" != x86_64 ]; then
    echo 'ARM64 cross-checks require the authorized Linux x86_64 host.' >&2
    exit 1
fi
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o bin/haunt-linux-arm64 ./cmd/haunt
arm_runner=$(command -v qemu-aarch64)
export HAUNT_TEST_SERVER_BINARY="$PWD/bin/haunt-linux-arm64"
export HAUNT_TEST_SERVER_RUNNER="$arm_runner"
# QEMU exercises the ARM64 executable and tests; it is not native hardware
# performance validation. Race instrumentation is exercised on native amd64.
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -exec "$arm_runner" -count=1 -timeout=8m ./...
