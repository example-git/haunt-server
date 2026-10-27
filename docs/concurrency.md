# Concurrency and resource ownership

Concurrency is implemented across HTTP requests, device sessions, rooms and pooled PostgreSQL operations. These are structural guarantees, not an unlimited-capacity claim.

| State/work | Owner and bound |
| --- | --- |
| Configuration | Startup-only host settings; revisioned runtime policy is persisted and published without restart |
| HTTP | Independent FIFO queues for control, history, messages, embeds, media, realtime HTTP and maintenance; canceled waiters leave immediately |
| PostgreSQL | Independent feature pools, 38 connections in total by default; transactions stay on their acquired connection |
| Passwords | Nonblocking semaphore, default four Argon2id jobs at 64 MiB/job |
| Authentication limits | 32 shards × 512 live keys; short locks, no database/hashing inside |
| Sessions/invitations | Per-account SQL row boundaries; 20 sessions and 100 active invitations per creator |
| Text/attachment accounting | Storage-row lock first, then conversation/attachment rows; no global process database mutex |
| Event sockets | 512 total, four/session; one writer; eight input commands; outbound 128 frames and 1 MiB |
| Event replay | At most 100 events and 768 KiB per snapshot batch; retained log ≤1 hour/10k rows/¼ text budget |
| Upload/probe | Two concurrent jobs; actual stream limit, five-minute read deadline, nine-minute completion context, five-second probe and bounded output |
| Files | One directory owner via `flock`; filesystem work outside accounting transactions |
| SFU registry/rooms | Registry and room locks protect state only; media/network/database operations occur after unlocking |
| SFU receivers | 128 packets per independent queue; full queues drop packets and increment metrics |
| Peers | Default 512 connections, 64 rooms, 16 participants per room |
| Encoded media sockets | One per active participant/device; 64 frames/2 MiB per receiver; overflow disconnects; network I/O outside room locks |
| Cloudflare credentials | Separate device/SFU cache keys; 1024 entries, eight in-flight requests, per-key request coalescing; no network I/O under cache locks |
| Optional relay | Port range and strict allocation cap; ≤64 allocations/device session; bounded TCP/TLS listeners and owned sockets |

## Feature capacity

| Workload | Active HTTP handlers | Queued HTTP requests | PostgreSQL connections |
| --- | ---: | ---: | ---: |
| Control: accounts, directory, configuration | 64 | 64 | 16 |
| History: message pages, individual messages, read snapshots, encryption history | 128 | 128 | 8 |
| Messages: sends, edits, deletes, read acknowledgments, encryption writes | 32 | 64 | 4 |
| Embeds: metadata, video tickets, attachment metadata | 8 | 32 | 2 |
| Media: attachments, embed images/video, art, plugin files, client downloads | 32 | 32 | 2 |
| Realtime: event/media socket queries and voice control | 32 | 32 | 4 |
| Maintenance: scheduled and requested cleanup | 1 | 1 | 2 |

`HAUNT_HTTP_<WORKLOAD>_ACTIVE` and `HAUNT_HTTP_<WORKLOAD>_QUEUED` override each row, using uppercase workload names. Active limits accept 1–4096; queue limits accept 0–8192. `HAUNT_HTTP_QUEUE_WAIT_MS` defaults to 1000 and accepts 1–10000. Queue wait counts toward the request deadline. A full queue or expired wait returns 503 `request_capacity_reached` with `Retry-After: 1`. Successful requests release their slot directly to the oldest waiter. Cancellation removes a waiter without running its handler.

`HAUNT_DB_MAX_CONNS` controls the control pool (default 16). `HAUNT_DB_<WORKLOAD>_CONNS` controls each other row and accepts 1–1024. These are additive limits: PostgreSQL needs capacity for their sum plus the gateway listener and operator connections. Pools share database identity and TLS settings, but have independent connections and acquisition queues. Background maintenance and preview-cache jobs select their own pools. Authentication, moderation and authorization queries use the caller's workload pool; they do not funnel every feature through the control pool.

Health checks bypass HTTP queues. Event and media WebSockets use their existing socket bounds and the realtime database pool; an open socket never occupies an ordinary HTTP slot. Live calls, feeds, and HTTP handlers run concurrently. These boundaries isolate connection and handler exhaustion, while PostgreSQL CPU, disk, quota-row locks, network bandwidth and host resources remain shared.

Preview metadata and image fetches each allow eight active provider jobs and 32 waiting jobs, with a one-second wait and a six-second total job deadline. Duplicate keys share a job. Busy results are not cached. Media downloads use a separate outbound HTTP transport from metadata, so their per-host connection limits are independent. Upload/probe and video-stream limits still apply within the media workload.

`GET /api/v1/admin/stats` reports current HTTP activity, queue depth, admissions, full-queue rejections and queue timeouts under `traffic`. Its `pool` totals include all pools, and `pool.workloads` reports connection occupancy, acquisitions, canceled acquisitions and acquisition duration for each feature.

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

Account feeds cache authorized conversation IDs and refresh that cache on committed directory/permission hints. One dedicated PostgreSQL `LISTEN chatxo_gateway` connection per server wakes independent socket workers; it is additional to the sum of the feature pool limits. It never runs their queries or serializes database work. Each connection has a one-slot wake queue, coalesced flags and at most 100 pending read IDs; overflow requests a compact read snapshot. Work is capped at ten rounds per second per socket; an idle five-second check handles sessions/event recovery. Listener reconnect invalidates directory/read caches. Legacy explicit subscriptions remain compatible.

Durable replay batches contain at most 100 events and 768 KiB, with bounded output queues (128 frames/1 MiB). Slow clients disconnect and resume from persisted cursors. Read positions take only a user/conversation row lock, advance monotonically, and do not acquire the shared quota row. Read snapshots are paged in groups of 100. Message mention IDs are resolved once on write; unread queries use conversation/sequence indexes and exclude self/deleted/expired content. Presence watching is optional and can be disabled for background servers.
