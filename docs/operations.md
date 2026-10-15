# Linux deployment, maintenance and recovery

For bundled Nginx, PostgreSQL and the web client in a single container, see the
[one-container installation](container.md). The instructions below describe
running the standalone server binary with separately managed dependencies.

The server targets Linux amd64 and arm64. Packages contain a static Go executable, generated API contracts, configuration examples, deployment templates and the Python backup tool. Install PostgreSQL 16+ client tools and `ffprobe` separately; the runtime host needs a reachable PostgreSQL database and persistent attachment storage. `ffmpeg`, Chrome, Go and QEMU are verification dependencies, not production server requirements.

## Provision one installation

1. Choose a dedicated application database and restricted database role with permission to manage that database's `haunt` schema. Provisioning is an operator action; the server does not create a database or configure PostgreSQL. Never point a test run at a deployed database.
2. Choose a persistent data directory and normal service listener. On a fresh installation, open the private first-time setup link printed at startup. The [wizard](first-run.md) saves a stable installation ID, walks through the public URL and dedicated database connection, and lets you choose the owner password. Explicit environment settings remain available through `.env.example` and are shown as locked fields; unattended installs may supply bootstrap credentials. Protect configuration files from other users. The program reads process environment variables, not `.env` files automatically. Preserve the installation ID and saved configuration with all backups.
3. Install the binary matching `uname -m` (`x86_64` → amd64, `aarch64` → arm64). Verify the archive's SHA-256 checksum. The default service example uses `/usr/local/bin/haunt`, a dedicated `haunt` account, `/etc/haunt.env`, and `/var/lib/haunt` for both its working directory and writable state. Set `HAUNT_DATA_DIR=/var/lib/haunt` in that environment.
4. Review `deploy/haunt.service` for the chosen paths/user, then install it through the host's normal service-management procedure. First startup migrates the application schema and creates the owner. Remove bootstrap credentials after successful provisioning. Later starts preserve the installation and owner password; incompatible identity/newer schema fails startup.
5. Place HTTPS in front of the loopback HTTP listener. Configure only explicit browser origins. `/healthz` is process liveness; `/readyz` checks the database/identity schema. Neither proves public media connectivity.

Only one application process may own the installation's attachment directory. Startup takes its filesystem lock before retention maintenance; a rejected second process cannot prune the active process's message history through a different policy. Changing policy requires restart. Lowering the text cap can immediately prune old history.

## HTTPS proxy or cloudflared container

`deploy/nginx.conf.example` supports request streaming and WebSocket upgrades. Adjust certificates, hostname and upload limits for the installation. The proxy must overwrite `X-Forwarded-For` with one client IP. Trust only its exact addresses with `HAUNT_TRUSTED_PROXIES`; requests from untrusted peers cannot supply client identity headers.

For a remotely managed Cloudflare Tunnel, use `deploy/compose.cloudflared.yml`. It runs only a connector using Linux host networking, a read-only filesystem, no capabilities, a file-mounted token and bounded Docker logs. It creates no PostgreSQL service, volume or network. The pinned image manifest includes Linux amd64 and arm64.

- In the Cloudflare tunnel dashboard, route the application hostname to `http://127.0.0.1:8080` on this connector's host. Enable WebSocket forwarding and avoid caching `/api/` routes. The origin remains bound to loopback.
- Set `HAUNT_PUBLIC_URL=https://your-chat-hostname`, `HAUNT_LISTEN_ADDR=127.0.0.1:8080`, `HAUNT_CLOUDFLARED_ENABLED=true`, and `HAUNT_TRUSTED_PROXIES=127.0.0.1/32,::1/128`.
- Put the tunnel token in a protected file readable by container UID 65532, with parent-directory access restricted appropriately. `CLOUDFLARED_TOKEN_FILE` is its host path; no token is passed on the command line. Compose file-backed secrets preserve host file ownership/permissions, so verify readability before starting.
- Start this dedicated Compose project using the reviewed file and explicit variables. For example, with the token-file path exported: `docker compose --env-file /dev/null -f deploy/compose.cloudflared.yml up -d cloudflared`. Do not reuse another service's Compose project or container.

In cloudflared mode, trusted client IP extraction uses `CF-Connecting-IP`, not caller-supplied forwarding chains. The connector uses QUIC where available, falling back to HTTP/2. The default does not require an inbound UDP media port: without external TURN, encoded media travels over WSS. Optional external/Cloudflare TURN keeps media on WebRTC; see [media-transports.md](media-transports.md). Live Cloudflare account/tunnel deployment still needs verification with the operator's actual hostname and credentials.

## Public VM and optional host relay

For direct WebRTC, set the mapped public IP with `HAUNT_MEDIA_PUBLIC_IP`, choose a `HAUNT_MEDIA_PORT_MIN`/`MAX` UDP range and open it in both cloud network rules and the host firewall. The public IP must actually map to this server. A proxy for HTTP alone does not expose the UDP media path.

The optional `HAUNT_RELAY_ENABLED=true` service accepts authenticated TURN over UDP/TCP on its configured listener. Open that listener and its relay UDP port range; optionally configure all three TLS settings (listener, certificate, key). Use a DNS-only hostname for a standalone relay, not an ordinary proxied HTTP hostname. The relay public IP, bind IP and advertised hostname must describe the real network mapping. Keep relay and SFU ranges distinct.

Private/link-local/metadata peer addresses are denied by default. If a deployment needs access to a private SFU, allow only the intended private peer CIDR; link-local metadata remains denied. Allocation counts and ports are bounded. Credentials are short-lived and supplied automatically to authorized callers. The built-in relay's signing secret is process-local; restart invalidates old credentials. There is no direct/relay setting in the client UI.

## Backup and restore

Stop the application and use the same installation identity and explicit database/data-directory environment with `scripts/backup.py`. The tool refuses the active attachment lock. It does a full database dump, so use a dedicated application database. Connection secrets are passed through a controlled environment rather than command arguments. A completed backup contains the database dump, built-in local attachments, storage-provider encryption key and SHA-256 manifest. Registered external stores require separate coordinated snapshots; see [runtime settings and storage](runtime-settings.md).

```sh
# HAUNT_DATABASE_URL, HAUNT_INSTANCE_ID and HAUNT_DATA_DIR must already
# identify this stopped installation. Keep backups outside its data directory.
python3 scripts/backup.py backup /srv/haunt-backups --keep 3
```

Rotation removes only matching generated backups for that installation. Keep off-host copies using the host's backup process. Do not assume the retained-text limit covers backup size.

To restore, provision a new empty target database and empty data directory, set the explicit target environment and the backed-up instance ID, then run:

```sh
python3 scripts/backup.py restore /srv/haunt-backups/SELECTED_BACKUP_DIRECTORY
```

Restore validates the manifest and file hashes before writes and refuses occupied targets. PostgreSQL restore uses one transaction. If database restoration succeeds but file finalization fails, the tool reports and preserves a staging directory; recover those files before starting. Successful startup reconciles attachment storage and applies current retention. Recovery tests also cover process termination during upload, normal restart, session persistence and owner recovery.

## Owner recovery

For host-authorized owner recovery, stop the server and put the new password in a protected local file readable by the service account. With the installation's normal environment, run:

```sh
haunt -owner-password-file /protected/path/new-owner-password
```

The command resets the existing owner password, revokes owner sessions and exits. Remove the temporary password file afterwards. Bootstrap variables never reset an existing owner's password. Member password recovery and account disabling are available through owner-authorized HTTP endpoints.

## Capacity, maintenance and logs

Owner-only `/api/v1/admin/stats` exposes logical quotas, database/relation/index sizes, dead-tuple estimates, pool pressure, quota-lock timing, event queues, SFU queues/peers, relay allocations and Cloudflare credential request/failure counts. `/api/v1/admin/maintenance` runs bounded cleanup; the regular maintenance interval is one minute.

The 256,000,000-byte text budget includes retained message and replay JSON accounting, not PostgreSQL indexes, metadata, WAL, old snapshots, backups or logs. Deletion allows PostgreSQL space reuse; it does not immediately shrink files. The 1 GiB attachment budget includes reserved uploads and failed physical deletions until cleanup succeeds. Monitor host disk usage and PostgreSQL vacuum/replication/WAL independently. WAL metrics in the API are cumulative cluster activity, not application-only bytes on disk.

The executable emits structured logs to stdout. The systemd example rate-limits service logs; configure the host journal's size/retention for an actual disk budget. The cloudflared example rotates three 10 MiB log files. Neither example changes another service's logging policy.

Default room/peer/queue limits are safety bounds, not measured call-capacity guarantees. Test intended concurrency and real network paths before deployment. See [testing.md](testing.md) for test setup and platform-coverage limits.

## Community appearance

The owner can update the server name, description, accent, icon and banner in the client's Server settings. These changes persist in PostgreSQL and take effect without a restart; a saved name overrides `HAUNT_NAME`. Avatars and server artwork do not expire with message attachments, and are included in the database backup. Schema version 5 is applied by the normal startup migration. Run only one process against an installation while migrating.

Schema version 6 adds mention IDs, persistent read positions, unread indexes and committed gateway wake triggers. Startup backfills mentions in existing retained messages and updates their JSON byte accounting. The gateway opens one dedicated LISTEN connection in addition to the request pool; include it in PostgreSQL connection capacity. No external broker or client transport setting is required. Run a single server process per installation during migration, as for earlier versions.
