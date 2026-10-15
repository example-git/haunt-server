#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
if [ ! -f client/package.json ]; then
    echo 'Initialize the client submodule: git submodule update --init --recursive' >&2
    exit 1
fi
(cd client && npm ci && npm run build)
mkdir -p bin
CGO_ENABLED=0 go build -tags webclient -trimpath -o bin/haunt-server ./cmd/haunt
echo 'Built bin/haunt-server with the web client embedded.'
