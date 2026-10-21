# Server design

The Go implementation targets Linux amd64 and arm64. One process owns one community, a dedicated PostgreSQL database, and a persistent attachment directory. Clients can independently authenticate to several installations; there is no central identity service or federation.

## Component boundaries

| Component | Responsibility |
| --- | --- |
| Configuration | Startup settings, revisioned runtime policy, stable installation identity, explicit origins and proxy trust |
| Identity | Owner bootstrap/recovery, invite-only accounts, passwords, independent device sessions, roles and channel permissions |
| Conversations | Channel permissions and DM membership, history, authorship, ordering and bounded retry deduplication |
| Storage | PostgreSQL pooling, embedded migrations, strict retained-text accounting and attachment reservations |
| Events | Committed replay, permission-scoped subscriptions, reconnect cursors, bounded queues |
| Media | Room-scoped publisher/receiver peers, negotiated codecs, RTP forwarding, RTCP feedback and cancellation |
| Connectivity relay | Optional host service with expiring ICE credentials, authenticated allocations, bounded ports/sockets and optional TLS |
| Operations | Maintenance, statistics, offline backup/restore, process shutdown and platform packaging |

Server media forwarding preserves encoded content without a capture or transcoding pipeline. The optional connectivity relay is configured by the host; client users do not choose transport modes.

## Persistence and transactions

A startup advisory lock serializes embedded migrations only. The database stores one immutable installation ID; an ID mismatch or newer schema fails startup. Owner provisioning never silently resets an existing password. Session/invitation tokens are random and stored as hashes. Password hashing is bounded and happens without holding a pool connection or database transaction.

Normal reads, authentication, rooms and independent requests remain concurrent. Message/attachment accounting takes the singleton storage row first, then conversation/attachment rows. Short transactions protect quota accounting while unrelated requests continue through pooled connections. No file upload, password computation, WebSocket send or media forwarding occurs inside an accounting transaction.

Message acceptance assigns a per-conversation sequence and server time, claims attachment IDs, inserts canonical data, records a durable event, applies the conversation's retention policy and commits before acknowledging success. A crash between commit and network delivery is recovered through replay/history. Editing preserves original age/order. Explicit deletion removes body/attachment references and older event copies, stores a tombstone and expires the attachments.

## Retention contract

Channel and DM message JSON each count `serialized bytes + 64`; replay events use the same rule. Channel history defaults to a 256,000,000-byte budget and 30-day age limit, with oldest-first capacity pruning. DM text has an independent 256 MiB default budget and no age expiry by default; a full DM budget rejects new writes. Configured age limits apply to reads and maintenance. Channel pruning selects victims by acceptance time then UUID in bounded batches. Runtime changes use revision checks and apply without a restart. See [runtime settings](runtime-settings.md) and [DM storage](dm-security.md).

The replay log is bounded by one hour, 10,000 records and a quarter of the text budget. Deleted/pruned replay ranges advance a conservative global floor; it can request a resync even when a particular conversation did not lose its own events. History and replay reads use repeatable-read snapshots. A cursor older than the retained boundary produces a visible gap. `history.pruned` is not an instruction to delete older client-held history.

Deduplication is unique by author/request UUID while the message row exists. It is not permanent deduplication after pruning. Conversation/member/configuration metadata and PostgreSQL physical overhead are outside the retained-text budget. Deleted channels become inaccessible; their retained message/file rows expire normally rather than bypassing accounting through cascading deletion.

## Attachment lifecycle

Reserve declared bytes transactionally, receive into a UUID `.part` file, enforce actual length, sniff and probe content, recheck authentication/access, fsync, rename, and mark ready. No request filename becomes a filesystem path. A ready attachment is claimable by one message in the same conversation. Failed uploads and expired files enter deletion; bytes remain reserved until physical removal succeeds.

One process owns the directory using a filesystem lock. Startup reconciles bounded file/database batches: interrupted uploads, missing/wrong-sized files and orphan UUID files. Unknown file types/paths fail visibly. Defaults are 2 MiB images, 10 MiB videos, seven days, and 1 GiB reserved capacity. Full storage rejects uploads rather than silently evicting unexpired files. Attachment state and text retention remain independently usable.

## Media and connections

An authenticated event socket owns at most one room participant and its peers. Admissions are short-lived and device-scoped. The registry protects membership with short locks; room locks own participant state and source/subscription maps. Packet and event delivery happen outside those locks. Every media receiver has a separate bounded queue. Publishing replacement invalidates previous source/subscription generations and peer IDs; old callbacks cannot remove a replacement peer.

The SFU forwards negotiated media and supports independent audio, camera and screen tracks. Clients supply compatible receiver offers. Mute/deafen and track-off state are enforced on the SFU forwarding path. Public host/UDP-range and ICE settings support VM deployment. The optional relay uses WebRTC-compatible TURN internally, provides expiring credentials through authorized call admission, and carries opaque client packets. Its existence does not create a user-facing direct/SFU/relay choice.

The cloudflared fallback forwards elementary encoded chunks over authenticated WebSockets using the same participant ownership. External TURN and Cloudflare Realtime TURN force relay-only WebRTC on clients and the SFU. Credentials are generated server-side and renewed through new admissions before expiry. See [media-transports.md](media-transports.md).

API schemas and the endpoint inventory are generated from shared wire types and real route registration; the server embeds Swagger UI for client developers.

## Disk and recovery

Retained-byte counters are not physical disk-size guarantees. Relation/index sizes, dead-tuple estimates, pool pressure, quota-lock acquisition time, queued event bytes, media drops, and relay allocation counts are exposed separately. PostgreSQL vacuum makes deleted space reusable; it does not generally shrink relation files immediately. WAL statistics are cluster-wide activity counters. Monitor filesystem/WAL usage with host tools and retain operational headroom.

Backups require a stopped server and the attachment lock. They include a custom PostgreSQL dump, built-in local attachment bytes, the storage-provider encryption key, stable instance ID and SHA-256 manifest. Registered external stores need their own coordinated snapshots; their configurations and attachment references are included in the database. Rotation touches only matching generated backups. Restore validates all files and requires empty database/file destinations; PostgreSQL restore is transactional. A file-finalization failure preserves staged files for recovery. Tests exercise corrupt-backup rejection, occupied-target refusal and a complete restore/restart.

See [API](api.md), [concurrency](concurrency.md), [operations](operations.md), and [testing](testing.md). PostgreSQL details: [vacuum](https://www.postgresql.org/docs/16/routine-vacuuming.html), [locking](https://www.postgresql.org/docs/16/explicit-locking.html), [restore](https://www.postgresql.org/docs/16/app-pgrestore.html).
