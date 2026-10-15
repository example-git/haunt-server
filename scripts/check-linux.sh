#!/bin/sh
set -eu

# Keep build/runtime verification on a Linux amd64 execution host.
if [ "$(uname -s)" != Linux ] || [ "$(uname -m)" != x86_64 ]; then
    echo 'Checks require a Linux x86_64 execution host.' >&2
    exit 1
fi

cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local
go mod tidy
go mod verify
go vet ./...
go run ./cmd/apidocs -check -client-out client/static/api
python3 -m unittest scripts.tests.test_install
mkdir -p bin
go test -race -count=1 -timeout=5m -coverpkg=./internal/... -coverprofile=bin/coverage.out ./...
go build -o bin/haunt ./cmd/haunt
# PostgreSQL HTTP integration tests run when HAUNT_TEST_DATABASE_URL is set.
# They create/drop isolated databases and require CREATEDB on the test account.
