#!/bin/sh
set -eu
if [ "$(uname -s)" != Linux ] || [ "$(uname -m)" != x86_64 ]; then
    echo 'Packaging requires the authorized Linux x86_64 build host.' >&2
    exit 1
fi
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local
go run ./cmd/apidocs -check -client-out client/static/api
(cd client && npm ci && npm run build)
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags webclient -trimpath -o bin/haunt-linux-amd64 ./cmd/haunt
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags webclient -trimpath -o bin/haunt-linux-arm64 ./cmd/haunt
cp bin/haunt-linux-amd64 bin/haunt
package_version=$(bin/haunt-linux-amd64 -version)
package_dir=$(mktemp -d)
trap 'rm -rf "$package_dir"' EXIT HUP INT TERM
mkdir -p "$package_dir/haunt/scripts" "$package_dir/haunt/deploy" "$package_dir/haunt/docs"
cp README.md .env.example "$package_dir/haunt/"
cp scripts/install-client-release.py scripts/backup.py "$package_dir/haunt/scripts/"
cp -R deploy/. "$package_dir/haunt/deploy/"
# Package only reviewed documentation; local deployment records stay private.
for document in README.md operations.md testing.md media-transports.md runtime-settings.md moderation.md dm-security.md audio-plugins.md audio-plugin-authoring.md client-development.md client-updates.md client-audio.md client-native-capture.md api.md architecture.md concurrency.md server-design.md server-menus.md branding.md roadmap.md endpoints.md openapi.json websocket.json; do
    if [ -f "docs/$document" ]; then cp "docs/$document" "$package_dir/haunt/docs/"; fi
done
for package_arch in amd64 arm64; do
    cp "bin/haunt-linux-$package_arch" "$package_dir/haunt/haunt"
    tar -C "$package_dir" -czf "bin/haunt-${package_version}-linux-${package_arch}.tar.gz" haunt
    (cd bin && sha256sum "haunt-${package_version}-linux-${package_arch}.tar.gz") > "bin/haunt-${package_version}-linux-${package_arch}.tar.gz.sha256"
    printf '%s\n' "bin/haunt-${package_version}-linux-${package_arch}.tar.gz"
done
