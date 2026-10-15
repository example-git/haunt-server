# One-container installation

The canonical installation and credentials guide is in
[example-git/haunt](https://github.com/example-git/haunt/blob/main/docs/installation.md).
Its Compose setup uses the published image directly; this source checkout also
supports the local builds below.

The root `Dockerfile` builds an Alpine image with separate Nginx, PostgreSQL 16
and Haunt processes. Nginx serves the pinned client submodule's production web
files and proxies the API, event socket and WebSocket media. The Go binary does
not embed a second copy of the client. Node, Go, Electron, build caches and
private audio plugins are not part of the runtime image.

> **Prebuilt-container port mapping:** client and setup traffic enters bundled
> Nginx on container TCP 80/443. Compose maps those to host TCP 80/443 by default;
> `HAUNT_HTTP_PORT` and `HAUNT_HTTPS_PORT` override the host-side mappings. The
> supplied image runs the Haunt process at `127.0.0.1:8080` inside the container,
> behind Nginx, and the supplied Compose file does not publish that backend port.

## Start a new installation

```sh
git clone --recurse-submodules https://github.com/example-git/haunt-server.git
cd haunt-server
cp deploy/container.env.example .env
```

Edit `.env`: set `HAUNT_PUBLIC_URL` to your public HTTPS hostname and
`HAUNT_MEDIA_PUBLIC_IP` to the IP forwarded to this host. Choose the server name
in the setup wizard. Point DNS directly to that public IP and forward **TCP 80
and 443 to the default host mappings** (or the host ports you selected with
`HAUNT_HTTP_PORT`/`HAUNT_HTTPS_PORT`) and
**UDP 50000–50100**, including cloud and host firewall rules. UDP port numbers
must be preserved through every NAT layer. The Compose mappings track custom
`HAUNT_MEDIA_PORT_MIN`/`MAX` bounds if you change them.

```sh
docker compose up -d --build
docker compose logs --tail 100 -f haunt
```

Open the **private setup link printed in the startup log**. It opens the wizard
at your configured address, on the same public port used by the normal client
and API. Confirm the address, choose your server name and policies, then choose
an owner username and password. The container supplies its private database;
there is no generated owner password to retrieve. When setup finishes, open
Haunt and sign in with the credentials you chose. Desktop account addresses use
`username@your-server-hostname`.

Keep the setup link private: it authorizes creation of the first owner and stops
working after setup. The page at the ordinary address explains where to find it.
Environment-controlled settings are visibly locked in the wizard. Settings are
saved in `/data/haunt/configuration.json` with mode `0600`; the owner password is
stored as an Argon2id hash in PostgreSQL and never saved in that file or logged.

For unattended installs, explicitly supply both bootstrap username/password, or
use `HAUNT_BOOTSTRAP_PASSWORD_FILE` with a mounted secret. Those credentials never
reset an existing owner. Existing installations keep their accounts and bypass
the wizard. See [first-time setup](first-run.md).

The first HTTPS startup obtains a Let's Encrypt certificate using HTTP-01;
TCP 80 must reach the container without another service intercepting challenges.
ACME state and certificates persist, and renewal is checked every 12 hours with
Nginx reloaded after successful renewal. Optional `HAUNT_ACME_EMAIL` sets the
account contact. Failed initial issuance exits with an error and preserves data.
Correct DNS/forwarding before restarting; repeatedly retrying a broken setup can
hit the certificate authority's rate limits.

For local text testing, use `HAUNT_PUBLIC_URL=http://<private-LAN-IP>` and that
host's LAN IP for media. Browsers still require HTTPS for microphone/camera access
outside localhost. Do not expect an HTTP-only LAN page to provide full calls.

## Components and storage

| Component | Runtime location | Exposure |
| --- | --- | --- |
| Nginx + static web client | `/data/apps/current/web` | TCP 80/443 |
| Haunt API/signaling | `127.0.0.1:8080` inside container | Private backend; not published by the supplied Compose file |
| Haunt WebRTC SFU | Configured UDP range | Direct port mapping |
| PostgreSQL 16 | `/data/postgres` | Unix socket only; no TCP listener |
| Haunt settings, attachments, keys, plugins, update downloads | `/data/haunt` | Authenticated application routes |
| Installation ID | `/data/config` | Host/container operator only |
| ACME account and HTTPS certificate | `/data/acme`, `/data/tls` | Host/container operator only |

Compose keeps all mutable installation state in its `haunt-data` named volume.
Container replacement preserves identity, accounts, settings, sessions and files.
Never use `docker compose down -v` unless you intend to delete that installation.
Never mount another application's PostgreSQL directory here. PostgreSQL's major
version is checked before startup; a major upgrade requires an explicit database
dump/restore migration, not swapping binaries against an existing data directory.

Startup provisions PostgreSQL and its restricted application role before handing
the services to Supervisor. OS peer authentication separates the database superuser
from Haunt; there is no shared default database password. Haunt and PostgreSQL run
as separate unprivileged users. Nginx has a privileged master and unprivileged
workers. The supervisor starts as root to prepare volume ownership. On stop it
drains Nginx and Haunt before stopping PostgreSQL. Keep the 300-second stop grace
period so an update can finish recovery. Supervisor restarts failed services;
exhausting startup retries stops the container so its restart policy can recover it. Health checks pass through Nginx to the real
database readiness endpoint; they do not prove public UDP reachability.

One running container owns an installation. Do not scale replicas against the
same volume. This packaging does not alter existing host databases or firewall
rules. No database port is published, and the image never embeds credentials.

## Other HTTPS arrangements

- **Supplied certificates:** set `HAUNT_TLS_MODE=provided` and mount readable
  fullchain/private-key files at `/data/tls/fullchain.pem` and
  `/data/tls/privkey.pem`. Set an HTTPS public URL. Restart the container after
  renewing externally supplied certificates.
- **Existing reverse proxy:** set `HAUNT_TLS_MODE=proxy`, keep the public HTTPS
  URL and forward to this container's port 80. `HAUNT_HTTP_PORT` can change its
  host mapping, for example to 8088. Restrict that origin port to the proxy in the
  host firewall. Set `HAUNT_PROXY_CIDRS` to the exact trusted proxy IP ranges;
  Nginx otherwise ignores forwarding headers. No `0.0.0.0/0` or `::/0` trust.
- **Cloudflared:** use proxy mode, set `HAUNT_CLOUDFLARED_ENABLED=true` and trust
  only the connector CIDR using `HAUNT_PROXY_CIDRS`. The connector remains
  separately operated. Nginx accepts `CF-Connecting-IP` only from those peers.
  Without external TURN, Haunt selects WebSocket media automatically. See
  [media transports](media-transports.md) for TURN and WebRTC alternatives.

The internal listener/data directory/trusted loopback proxy values are owned by
this image. Other server configuration variables from `.env.example` remain
available through the container environment, including storage policy and
optional TURN. External storage providers can be configured in server settings.
The optional built-in TURN service needs its own TCP/UDP listener and relay range
published explicitly; the default Compose file exposes the SFU range only.

Advanced deployments may supply `HAUNT_DATABASE_URL` for a separately operated,
dedicated database. That completely skips the internal PostgreSQL process and
initialization; the operator must provision and back up the external database.
Do not set it for the normal one-container installation.

## Updating and backing up

The first startup copies the bundled binary and static client into `/data/apps`.
An atomic `current` link selects the installed release. Restarting or replacing
the runtime container preserves that selection instead of overwriting it.

Set one release source in `.env`:

```ini
HAUNT_APP_UPDATE_REPOSITORY=example-git/haunt
# Alternatively: HAUNT_APP_UPDATE_MANIFEST_URL=https://updates.example.com/haunt-app.json
HAUNT_APP_UPDATE_AUTO=true
HAUNT_APP_UPDATE_INTERVAL=21600
```

The repository source uses GitHub's latest published release. Private repositories
also need `HAUNT_APP_UPDATE_TOKEN_FILE` pointing to a read-only mounted file containing
a fine-grained token with repository contents read permission. Tokens are sent only
to GitHub's API (or the configured manifest origin), never across host redirects.
With a source configured but AUTO omitted, checks only report available updates.

```sh
docker compose exec haunt haunt-update check
docker compose exec haunt haunt-update apply
docker compose exec haunt supervisorctl status
docker compose exec haunt supervisorctl restart haunt
```

Supervisor uses a root-only Unix socket, with no management TCP port. The readiness
helper normally shows EXITED after successfully completing its one-time check.

Updates download a bounded HTTPS archive, verify its exact size/SHA-256, platform
and runtime contract, reject unsafe archive entries and stage it before stopping
service. The updater backs up the bundled database and local application files,
switches the binary/client together, then checks readiness. Failed application
startup restores the database, files and previous release. One previous release
and the latest successful update's recovery backup are retained; failed-recovery
backups remain for operator attention. External storage needs its own coordinated
backup. Automatic application updates require the bundled PostgreSQL database.

The configured release publisher is trusted to provide code: a checksum validates
bytes but is not an independent publisher signature. A host power loss or forced
kill during database migration may still require restoring the retained backup.

Tagged `server-vX.Y.Z` builds publish `haunt-app.json` and architecture-specific
application archives. Ordinary application updates do not rebuild the container.
A change to PostgreSQL, Nginx, system libraries or the runtime contract still needs
a new runtime image. Such a manifest is rejected rather than partially applied.
Update the checkout/submodule and rebuild the image for runtime updates:

```sh
git pull --ff-only
git submodule update --init --recursive
docker compose up -d --build
```

For a consistent full-installation backup, stop the stack and archive its volume
with the same image (including PostgreSQL data, local attachments and keys):

```sh
umask 077
docker compose stop
docker compose run --rm --no-deps -T --entrypoint tar haunt -C /data -czf - . > haunt-backup.tar.gz
docker compose up -d
```

Keep an off-host copy. External storage providers need their own coordinated
backup. Restore only to an **empty** volume, with the same PostgreSQL major
version, while the service is stopped:

```sh
docker compose run --rm --no-deps -T --entrypoint sh haunt -c \
  'test -z "$(ls -A /data)" && tar -xzf - -C /data' < haunt-backup.tar.gz
docker compose up -d
```

Do not roll back to an older server binary after a schema migration without also
restoring a compatible backup.

## Builds and verification

Native builds support Linux amd64 and arm64. Buildx can also produce both
platforms from one host when its ARM emulation is configured:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t YOUR_REGISTRY/haunt:VERSION --push .
```

The hub's `Publish coordinated container` GitHub workflow builds and checks each architecture on
its native Linux runner, using the exact pinned public client commit. After both
complete bundle tests pass, it publishes the tested images
to `ghcr.io/example-git/haunt`. The version tag and `latest` include Linux amd64
and arm64. Each image records the server revision, client revision and version
in its OCI labels. Manual builds default to the pinned client version; tagged
server builds use the server tag. An explicit workflow version must be
`major.minor.patch`.

To use the published image, set `HAUNT_IMAGE=ghcr.io/example-git/haunt:latest`
(or a specific version) in `.env`, authenticate to GHCR if the package is
private, then run:

```sh
docker compose pull haunt
docker compose up -d --no-build
```

The workflow also retains downloadable Docker image archives and checksums.
Import an archive using
`gzip -dc haunt-linux-amd64.docker.tar.gz | docker load`, then set
`HAUNT_IMAGE=haunt:check` and run Compose without `--build`.

The probe stage builds FFmpeg 8.0.1's `ffprobe` from its hash-checked official
source, retaining builtin decoders/parsers and file demuxers while excluding
network access, device capture, filters and encoders. Its LGPL notice is included
at `/usr/share/licenses/ffprobe`; the source URL, hash and configuration are in
the Dockerfile. The runtime needs no graphics stack.

Local integration verification:

```sh
docker build -t haunt:container-check .
python3 scripts/test-container.py haunt:container-check
```

The harness creates its own named container and volume, with loopback-only
temporary HTTP publishing. It tests the complete bundle including PostgreSQL;
it never mounts existing data, reads deployment secrets or connects to a host
database. It covers static/API routing, cache/security headers, login, messaging,
authenticated WebSockets, session/data persistence, graceful shutdown, child
failure, identity mismatch, supplied TLS certificates, checksum rejection,
application updates and failed-release database/files recovery. It does not request
real certificates or certify a host's public UDP/NAT path.
