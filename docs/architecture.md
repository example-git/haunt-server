> Historical design: the client is now implemented. The October 12 DM update supersedes the original server-readable DM and shared-retention decisions below. See [DM encryption, independent limits and recovery](dm-security.md) for current DM behavior.

> Implementation status: the server milestones are implemented for Linux amd64/arm64. This document preserves the original product decisions; see [server-design.md](server-design.md), [api.md](api.md), and [testing.md](testing.md) for current behavior and checks.

# Haunt architecture

Status: **server implemented**, 2026-10-09. Targets Linux amd64 and arm64. Browser and desktop clients live in the client submodule. Current behavior and limits are described in [api.md](api.md), [media-transports.md](media-transports.md) and [testing.md](testing.md).

## Confirmed choices and working constraints

- One community per self-hosted installation.
- Accounts belong to each installation. The client can choose login servers and maintain simultaneous authenticated connections to multiple independent installations. Saved logins and server switching alone are insufficient. Portable identity remains a possible future direction, not a prerequisite or selected implementation.
- Built-in media forwarding on the owner's host.
- Go is the selected backend language.
- Concurrent design is a baseline. PostgreSQL with a concurrent connection pool is the chosen implementation of that requirement; shared state uses narrow ownership/transaction boundaries.
- Accounts use invitation-only registration with local username/password authentication. DMs are server-readable and encrypted in transit. HTTP handles actions/history/uploads; WebSocket handles updates and signaling.
- First usable release includes voice, camera video, and screen sharing.
- Initial target: a friend group, corresponding to the offered 10-50 account / 5-15 simultaneous voice-participant planning range, not a capacity claim.
- This checkout's development/verification host and deployment target are Linux amd64 and arm64. Local builds, tests, and runtime checks are authorized here.
- DMs remain within the host instance. Both DMs and community channels use rolling history: 30 days of recent messages on the server, older downloaded history on clients. At the 256 MB (256,000,000-byte) retained-text ceiling, prune oldest messages.
- Image attachments are limited to 2 MiB/file and videos to 10 MiB/file. Server attachment retention is 7 days with a 1 GiB instance budget. Attachment limits are separate from message-text retention; full attachment storage rejects new uploads until capacity is available.
- Design and implement the server first. Client behavior remains theoretical; define only the server contracts necessary to support it.

## Product boundaries

- An **instance** is one self-hosted installation and its data. It has its own administrator, accounts, configuration, and storage.
- A **server** is the community hosted by that instance, with roles, categories, text channels, and voice channels. One installation hosts one community.
- Account identity and authorization belong to the instance. A server membership and channel permissions decide which events and media a client can access.
- There is no federation or central service in the initial design. Each host controls its copy.

### Concurrent connections to independent servers

The owner explicitly requires selectable login servers and multiple active server connections at once. One installation still hosts one community; a client can participate in several installations concurrently.

- Server discovery and login are scoped to the selected instance URL, with no hard-coded central authentication server.
- A stable instance identifier namespaces account, conversation, message, and attachment identifiers. Matching usernames on different instances do not establish the same identity.
- Each instance authenticates its own account and issues its own session. Credentials and session tokens must not be forwarded to another instance.
- Event connections, subscriptions, synchronization cursors, and reconnect behavior are independent per instance. A failed or logged-out connection does not disconnect other instances.
- The server protocol exposes its API version, supported features, and applicable upload/retention limits so one client can interact with differently configured hosts.
- Browser-client hosting/origin policy must support explicitly configured client origins. Do not assume the client is always served from the connected server's origin; do not globally disable origin checks.
- Simultaneous text/session connections do not by themselves select participation in several voice calls at once. Cross-instance DMs, federation, and portable identity remain separate decisions.

## Selected transport paths

```text
client -- HTTPS REST --> instance API --> storage
client <-- WebSocket --> authenticated event gateway --> channel subscribers
client <-- WebRTC/UDP --> instance SFU --> other voice participants
                       ^
                       |
              signaling over WebSocket
```

HTTP handles state snapshots, mutations, and uploads; the WebSocket carries update notifications, presence, and WebRTC signaling. Both DM and community-channel reconnect recovery use retained recent server history plus the theoretical client-local history described below. Connections are scoped independently to each selected instance. These server paths are implemented. Client persistence and multi-instance UI remain future client work.

For voice, camera video, and screen sharing, each client connects by WebRTC to a **selective forwarding unit (SFU)** on its own instance. The SFU forwards participant media to permitted listeners/viewers without mixing streams. This keeps the host on the media path, as requested, while clients can control volume and mute for each speaker. WebSocket messages carry channel join/leave, SDP offer/answer, and ICE candidates. ICE/public address configuration and TURN support must be considered in self-hosting setup. All three media features belong to the first usable release. Cloudflared hosting selects external/Cloudflare TURN when configured, otherwise the encoded WebSocket fallback. See [media-transports.md](media-transports.md).

## Rolling message history

This replaces the earlier delete-on-delivery queue proposal. The server retains a recent window even after messages are delivered; clients retain older downloaded history.

Server behavior and the future client contract:

1. Authenticate and authorize the conversation access, then commit an accepted message to durable recent-history storage with a stable ID and ordering key before acknowledgement and fan-out.
2. Authorized clients persist received messages locally and keep a synchronization cursor. Repeated delivery is deduplicated by stable ID.
3. Reconnecting clients fetch missing updates within the retained server window and merge those updates into their local history.
4. Server retention cleanup removes messages older than 30 days. If the configured text budget is exceeded, remove the oldest messages first until usage is within budget. Retention removal is distinct from a user-deletion event and must not instruct clients to erase historical copies.
5. Clients obtain older messages from their own local history. New devices or users offline beyond the retained window cannot automatically recover history that neither their device nor the server holds. Device transfer/backup or authorized peer-assisted recovery would require an explicit additional design.

The 256 MB retained-text ceiling is shared across the instance's DMs and channels, with deterministic ordering by server acceptance time and a stable tie-breaker. Unread or pinned messages do not silently bypass retention. APIs expose the earliest retained history and indicate when a requested cursor predates it. Client storage and recovery behavior remains theoretical in this server-first phase.

The owner selected server-readable DMs with encrypted transport, not end-to-end message encryption. Application reads remain restricted to conversation participants; host administrators retain technical access to stored content.

### Attachments

Selected limits: 2 MiB per image, 10 MiB per video, 7-day server retention, and 1 GiB of attachment storage per instance. Enforce byte limits while streaming uploads, not only using the caller's declared length. Reserve capacity atomically so concurrent uploads and temporary files are included. Reject oversized files or uploads without capacity explicitly; do not silently shorten an existing attachment's retention to make room. Once media expires, retained message metadata reports the attachment as expired. Upload concurrency and per-account rate limits are enforced. Per-user storage quotas and client attachment caching remain future work. No automatic expensive video transcoding is selected.

## Request and event flow

1. A client authenticates to its instance. The server owns the session and user ID.
2. A client requests a server/channel snapshot over HTTPS. The API checks membership and channel permissions.
3. A client connects to the WebSocket with its session. The gateway subscribes it only to channels it may read.
4. A message creation request is authorized, committed to recent-history storage with necessary oldest-first pruning, assigned a server sequence, and then emitted to authorized conversation subscribers. The sender receives the same committed event. Both DM and channel history follow the selected retention policy.
5. A voice join checks the voice channel permission before creating a WebRTC peer connection. The SFU maps tracks to that voice channel and forwards only to members currently permitted to listen.
6. On disconnect or permission loss, subscriptions and media tracks are removed. Reconnect reads stored state before continuing events.

These server flows are implemented and covered by HTTP, PostgreSQL, WebSocket and media tests.

## Server components

See [server-design.md](server-design.md) for server boundaries, connection contracts, and storage paths, and [concurrency.md](concurrency.md) for ownership and synchronization rules. Identity, messaging, attachments, media and recovery have runtime test coverage.

- `cmd/haunt`: process startup, configuration, shutdown.
- `internal/config`: immutable per-instance configuration and policy defaults.
- `internal/httpapi`: HTTP health/readiness/discovery, authenticated identity/community actions, bounded rate limits, event/media gateways and generated documentation.
- `internal/identity`: local accounts, sessions, invitations, and community permissions. Host owner recovery and owner-authorized member recovery are implemented.
- `internal/platform/postgres`: concurrent PostgreSQL pool and embedded transactional migrations, without a global serialized database worker.
- `internal/voice`: Pion peer connections, SFU rooms, track forwarding, ICE configuration, and media cleanup.
- `internal/attachments`: content validation, reservations, retention, recovery and access checks. Thumbnails are not implemented.

## Deployment and data defaults

- One process and one persistent data directory first. A host supplies HTTPS through a reverse proxy or built-in TLS setup later.
- PostgreSQL plus a filesystem attachment store. An offline checksummed backup/restore tool and verified recovery procedure are provided; see [operations.md](operations.md).
- Server settings, roles, theme assets, emoji, and profile customization live with that instance. Browser client styling can be highly customizable without changing the transport or authorization model.

## Decisions to test before a public network release

- SFU capacity and bandwidth for realistic voice channel sizes; RTP forwarding, RTCP, reconnection, and track removal.
- UDP/NAT behavior across home hosts, VPSs, and restrictive client networks; TURN fallback and host configuration.
- Permission changes during active text/voice sessions, event ordering, reconnects, and slow consumers.
- Authentication, rate limits, attachment validation, TLS, and backup/restore.
