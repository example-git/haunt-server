# Concurrency and resource ownership

Concurrency is implemented across HTTP requests, device sessions, rooms and pooled PostgreSQL operations. These are structural guarantees, not an unlimited-capacity claim.

| State/work | Owner and bound |
| --- | --- |
| Configuration | Immutable after startup; changes require restart |
| HTTP | Request context; 128 active ordinary handlers; health and bounded WebSocket admission are separate |
| PostgreSQL | `pgxpool`, default 16 connections; transaction-local connection ownership |
| Passwords | Nonblocking semaphore, default four Argon2id jobs at 64 MiB/job |
| Authentication limits | 32 shards × 512 live keys; short locks, no database/hashing inside |
| Sessions/invitations | Per-account SQL row boundaries; 20 sessions and 100 active invitations per creator |
| Text/attachment accounting | Storage-row lock first, then conversation/attachment rows; no global process database mutex |
| Event sockets | 512 total, four/session; one writer; eight input commands; outbound 128 frames and 1 MiB |
| Event replay | At most 100 events and 768 KiB per snapshot batch; retained log ≤1 hour/10k rows/¼ text budget |
| Upload/probe | Two concurrent jobs; actual stream limit, 60-second receive context, five-second probe and bounded output |
| Files | One directory owner via `flock`; filesystem work outside accounting transactions |
| SFU registry/rooms | Registry and room locks protect state only; media/network/database operations occur after unlocking |
| SFU receivers | 128 packets per independent queue; full queues drop packets and increment metrics |
| Peers | Default 512 connections, 64 rooms, 16 participants per room |
| Encoded media sockets | One per active participant/device; 64 frames/2 MiB per receiver; overflow disconnects; network I/O outside room locks |
| Cloudflare credentials | Separate device/SFU cache keys; 1024 entries, eight in-flight requests, per-key request coalescing; no network I/O under cache locks |
| Optional relay | Port range and strict allocation cap; ≤64 allocations/device session; bounded TCP/TLS listeners and owned sockets |

## Database ordering

Migrations serialize only startup. Registration hashes first, then consumes an invitation and creates account/session atomically. Login locks only the account after verification to enforce the session cap and recheck current password/disabled state. Password changes/recovery revoke sessions transactionally. Role replacement locks the affected member.

All message and attachment accounting writes lock `chatxo.storage` first. Message order and attachment claims follow. Retention selects the strict oldest prefix, deletes in bounded batches, updates conversation boundaries and records events under the same transaction. It never uses `SKIP LOCKED` to evict newer content. Reads and unrelated identity operations continue through other pooled connections while that row is contended. Stats expose acquisition count, pending acquisitions and elapsed acquisition time (including database round trips).

No transaction spans an upload, ffprobe run, password hash, WebSocket write or RTP delivery. Cancellation rolls back uncommitted writes. Responses only acknowledge a successful commit. Deduplication does not survive pruning of the message row.

## Authorization and recovery

Each request reloads session/base roles; each conversation operation evaluates current overrides or DM membership. Account WebSockets recheck credentials before commands, committed changes, and a five-second idle fallback; permission hints invalidate the cached visible directory. Legacy explicit subscriptions recheck every second. Already-authorized in-flight work can finish during a concurrent change. Uploads recheck after receiving bytes.

Durable messages and replay commit together. Slow/disconnected sockets recover from the retained log or receive `sync.required` and fetch history. A byte limit protects queues from large escaped message bodies as well as a count limit. A replay page stopped by its byte bound returns the last delivered cursor, not a later head that would skip events.

Each room participant and publishing peer has a generation. Replacement closes obsolete subscriptions; late callbacks are checked against that generation. Send targets are snapshotted under room locks and delivered afterward. Header extensions are copied/mapped by negotiated URI; shared packet memory is never modified by one receiver.

## Shutdown

Stop HTTP admission and drain ordinary requests (15 seconds), cancel/wait for maintenance, cancel event handlers and wait for readers/writers, close peers and wait for media workers, close relay listeners/accepted sockets/allocations, close the attachment lock, then close PostgreSQL. Upgraded connections are explicitly owned; `http.Server.Shutdown` alone is insufficient. Relay socket ownership supplements library shutdown so allocations cannot remain live after listener closure.

The tests exercise real request cancellation behind a held quota row while other reads remain available, concurrent retries and writes, room isolation/replacement/revocation, bounded queues, physical deletion failure, process SIGTERM/SIGKILL recovery, and Linux race detection. See [testing.md](testing.md).

## Account event gateway

Account feeds cache authorized conversation IDs and refresh that cache on committed directory/permission hints. One dedicated PostgreSQL `LISTEN chatxo_gateway` connection per server wakes independent socket workers; it is additional to `HAUNT_DB_MAX_CONNS`. It never runs their queries or serializes database work. Each connection has a one-slot wake queue, coalesced flags and at most 100 pending read IDs; overflow requests a compact read snapshot. Work is capped at ten rounds per second per socket; an idle five-second check handles sessions/event recovery. Listener reconnect invalidates directory/read caches. Legacy explicit subscriptions remain compatible.

Durable replay batches contain at most 100 events and 768 KiB, with bounded output queues (128 frames/1 MiB). Slow clients disconnect and resume from persisted cursors. Read positions take only a user/conversation row lock, advance monotonically, and do not acquire the shared quota row. Read snapshots are paged in groups of 100. Message mention IDs are resolved once on write; unread queries use conversation/sequence indexes and exclude self/deleted/expired content. Presence watching is optional and can be disabled for background servers.
