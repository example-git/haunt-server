# Live server configuration and attachment storage

Server settings contains **Limits & access** and **Storage providers** for owners
and roles with **Manage server configuration & storage** (`16384`).
ManageServer permission (`32`) controls appearance and emojis.

Configuration is stored in PostgreSQL. Until the first save, environment settings
remain the defaults. After a save, the persisted policy wins across restarts.
GET/PUT `/api/v1/instance/configuration` use a revision number to reject stale edits.
Successful writes are audited and the gateway tells clients to refresh discovery.

The following settings apply without a restart:

| Setting | Effect |
| --- | --- |
| Image/video upload sizes | New attachment reservations; actual content is checked |
| Attachment storage budget | All providers combined, including in-flight reservations; cannot be set below used space |
| Attachment retention | Expiry for new uploads; existing expiry dates remain intact |
| Channel message budget and retention | Reads honor age immediately; writes and maintenance prune oldest retained channel data |
| AI channel and aggregate budgets | Include messages, provider replay, tool results, turn state and bounded active reservations; remove the oldest coherent history prefix to 80% of the exceeded budget |
| DM text budget and retention | Independent budget; full storage rejects writes; zero age retention keeps messages until deletion |
| Voice room and media-connection limits | New admissions/connections; existing calls remain connected |
| Invitation registration | Allows or closes new registrations; invitations are still required |
| Temporary session duration | New nonremembered sessions and devices that turn off Remember me; remembered logins remain indefinite |

An upload keeps one policy and one provider from reservation through completion.
Provider activation, reservations and budget changes share the existing short
storage-accounting transaction. No remote network operation runs under that lock.
The API is the authority for attachment upload sizes. Configure a reverse proxy's
attachment route without its own fixed body-size cap, with request buffering off;
see `deploy/nginx.conf.example`. Upload reception is bounded to five minutes and
completion to nine minutes. Ordinary API requests keep their shorter deadlines.

Listener addresses, database connections, TLS and service secrets remain host
configuration. They are not represented as editable settings in this menu.
Profile pictures, banners and emojis retain their existing separate validation
limits and live in the database, outside the chat-attachment budget.

## Providers

Supported providers are private Amazon S3 buckets, S3-compatible object services
(such as MinIO on a LAN or VPS), and server folders including mounted NFS/SMB
shares. The bucket must already exist; mounting shares and granting filesystem
access are host administration steps. The server process must have access to the
path. With the example systemd sandbox, use a mounted folder beneath the configured
data directory or explicitly allow another storage root with ReadWritePaths.

The menu provides presets for Amazon S3, Cloudflare R2, Backblaze B2,
DigitalOcean Spaces, MinIO/owner-hosted S3, custom S3 endpoints, and local folders.
R2 uses region `auto` and the account endpoint; B2 needs the bucket's regional
S3 endpoint and matching region; Spaces uses its regional endpoint and
`us-east-1` for signing. Presets populate protocol options; they do not create
buckets or provider accounts. Registering still checks the supplied credentials
and destination before saving.

Registration and connection checks write, read and delete a small random object.
AWS endpoints are derived from the region when Endpoint is empty. Compatible
services accept an explicit endpoint and optional path-style addressing. HTTP is
supported for trusted LAN endpoints; use HTTPS for remote services. Credentials
need GetObject, PutObject, DeleteObject and multipart-upload/abort permissions in
the provider's namespace. Credentials are explicit; the server does not borrow
unrelated host AWS credentials. An optional temporary session token is supported.
Replace credentials in the menu before temporary credentials expire.

Every provider uses its own `haunt/<installation-hash>/<provider-id>/` namespace
under the chosen folder or object-key prefix. Original attachments stay in the
built-in local store. Changing the active provider affects new uploads only and
never moves or deletes existing files. Remove is available only for an inactive
provider with no referenced files (including pending uploads and deletions).
Register a new provider to change its endpoint, bucket or root; this keeps old
references stable. Up to sixteen additional providers can be registered.

Downloads remain authenticated and proxy through Haunt, including byte ranges.
Provider credentials and object URLs are never exposed to clients. A provider
outage leaves its metadata and reserved-byte accounting intact; deletion retries
when storage recovers. Multipart transfers have bounded concurrency. Uploads are
staged locally for content inspection, so the server still needs temporary disk
space for the two allowed simultaneous uploads.

## Backup and recovery

S3 credentials are encrypted using AES-GCM, bound to the provider ID. The 32-byte
`storage-provider.key` resides in the server data directory with mode 0600.
`scripts/backup.py` includes it in the checksummed offline backup and restores it
before the server starts. Restoring a database containing encrypted credentials
without this key fails startup rather than generating an unusable replacement.

AI account credentials use a separate `ai-provider.key`, also included in backup
and restore. Keep both keys with the matching database; missing AI keys prevent
startup when encrypted accounts or pending connections exist.

The backup includes the database and built-in local attachments. External provider
objects, including registered local folders, require their own snapshots/backups;
the backup manifest lists those providers without plaintext credentials. Restore
those stores at their configured locations as well as restoring Haunt's database
and key. Keep external snapshots consistent with the stopped-server database dump.
For versioned buckets, provider-side version retention and lifecycle rules control
older versions; Haunt's budget measures currently referenced objects, not historic
provider versions or bills.

The S3 transport uses the AWS SDK for Go v2 and its bounded multipart transfer
client; see [AWS S3 SDK documentation](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/go_s3_code_examples.html).

## AI channels

AI channels share a persistent text conversation. Server roles control visibility,
posting, **Use AI** (`262144`) and **Switch AI preset** (`524288`). A member allowed
to post but denied Use AI can send messages without starting generation. Mentioning
`<@ai:PRESET_UUID>` selects the channel-wide persona for that message and subsequent
messages. An already running response retains its original settings. Historical replies keep
the assistant name and immutable avatar version used when they were written.

Channel managers configure the account, model, instructions, assistant identity,
personas and storage limit. **AI connections** requires ManageConfiguration to
connect, reconnect or disconnect accounts. Initial OAuth connection runs in the
desktop app; configured channels and authorized settings also work on the web.
Claude uses a dynamic loopback callback. Codex requires localhost port 1455 and
reports a conflict if another application owns it. Antigravity uses the provider's
hosted callback and a pasted authorization code or callback URL. Permanent provider
credentials remain encrypted on the server, which refreshes them independently
of desktop connections. Provider request versions are refreshed at startup and every
six hours, with a persistent `ai-client-versions.json` cache and reference-version
fallbacks. Explicit `CLAUDE_CODE_VERSION`, `CODEX_VERSION` and
`ANTIGRAVITY_CLI_VERSION` overrides must be valid version strings; invalid values
fail startup. An active request keeps one identity snapshot.

The server commits the human message and generation turn atomically. A successful
message POST confirms durable acceptance. One worker generates at a time per
channel; status and response checkpoints arrive through authorized events. After
restart, ambiguous in-flight requests are interrupted and require an explicit retry.
Partial responses remain visible. A missing account, unsupported selected model,
permission loss or exhausted storage is reported without substituting providers.

The activity indicator above the composer is shared by channel viewers. A quiet
provider changes to “Waiting for provider” after two minutes and can still
complete. The requester or a channel manager can cancel a response. Tool calls
can continue across rounds for the life of the response. Intermediate results
and provider replay belong to that response.

Search uses the server’s bundled private SearXNG runtime. Operators can set
`HAUNT_DIFFBOT_KEY` for a fallback search when upstream engines fail.
`read_url` turns public pages into Markdown without executing JavaScript.
The reader tries a direct connection first and can use cached public proxy lists
for a failed route. An unavailable page returns an error for that tool call.

Add a personality to configure its name, description, instructions and avatar.
Members can mention it to select it for the channel. When a manager enables
agent switching, the current personality finishes its response before the
new selection takes effect. Removing the last personality restores the base identity.

Channels using Codex can generate images and edit images attached to a request
or its direct reply target. Both tools authenticate with the selected Codex
connection. Results appear as channel attachments. A later direct reply to
the image includes it in model input.

Defaults are 64 MiB per AI channel and 128 MiB across AI conversations, both
subordinate to the server text budget. Administrators can change the defaults in
Limits & access or set `HAUNT_AI_CHANNEL_MAX_BYTES` and `HAUNT_AI_TEXT_MAX_BYTES`
before the first configuration save. Existing age retention also applies. Pruning
removes the oldest coherent history prefix and dependent turn records; clients
evict that history from local caches, including after reconnect. Active work has a
finite reservation and can report a storage error. Images use attachment budgets;
pruned message attachments are reclaimed through the existing blob deletion path.
PostgreSQL deletion permits space reuse and does not promise immediate database,
index or WAL shrinkage.
