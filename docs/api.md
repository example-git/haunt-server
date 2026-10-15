# Server API v1

Generated OpenAPI 3.1 and an interactive reference are available at `/openapi.json` and `/docs/`. See [client-development.md](client-development.md), the generated [endpoint inventory](endpoints.md), and [WebSocket schemas](websocket.json). This guide explains behavior and recovery beyond the schemas.

All paths below are relative to the selected installation's base URL. Public hosts require HTTPS; localhost, loopback and private-network IP addresses also accept HTTP for local testing. JSON bodies require `Content-Type: application/json`, have a 16 KiB limit (512 KiB for message requests), and reject unknown fields and trailing JSON. Errors return `{"error":"code"}`. UUID resource IDs are local to the instance; preserve the discovery `instance_id` alongside portable references.

## Discovery and authentication

`GET /healthz` checks the process. `GET /readyz` checks PostgreSQL, schema version, persisted identity, and owner presence. `GET /api/v1/instance` returns public instance metadata and implemented capabilities. Capabilities include the implemented messaging, attachment, event, and media services. `media.relay` appears when the optional relay is enabled.

The owner is created from `HAUNT_BOOTSTRAP_USERNAME` and `HAUNT_BOOTSTRAP_PASSWORD` on first startup. There is no public owner-bootstrap endpoint. These settings never reset an existing owner. Registration always requires an active invitation.

| Method/path | JSON body or behavior |
| --- | --- |
| `POST /api/v1/auth/login` | `{"username":"owner","password":"your passphrase","device_name":"desktop"}` |
| `POST /api/v1/auth/register` | Login fields plus `"invitation":"invitation token"`; returns 201 |
| `GET /api/v1/me` | Current user, session ID, and instance permissions |
| `GET /api/v1/sessions` | Current account's active device sessions |
| `DELETE /api/v1/sessions/{id}` | Revoke one of your sessions, including the current one; returns 204 |
| `POST /api/v1/auth/logout` | Revoke current session; returns 204 |

Login/registration returns `instance_id`, `user`, `session`, `token`, and `token_type: "Bearer"`. Subsequent requests use `Authorization: Bearer <token>`. Cookies, URL tokens, and cross-instance tokens do not authenticate. Tokens are returned once; only SHA-256 token hashes are stored. Sessions expire after a fixed lifetime (30 days by default), without sliding renewal. Account sessions are independent; revoking a device leaves other devices signed in.

Usernames are case-insensitive ASCII letters/digits/underscores, 3–32 characters, stored lowercase. Registration passwords require at least 8 characters and at most 1024 UTF-8 bytes. Device labels require 1–128 characters. Passwords use Argon2id with a random 128-bit salt and 256-bit result, using RFC 9106's 64 MiB/3-pass/4-lane profile. See [RFC 9106](https://www.rfc-editor.org/rfc/rfc9106.html#section-4).

Each account has at most 20 active sessions. An additional login returns 409 `session_limit_reached`; revoke an existing session first. Hashing concurrency is bounded by `HAUNT_AUTH_PARALLEL` (default four); saturation returns 503 `authentication_busy` with `Retry-After`. No database transaction or connection is held while hashing.

## Invitations

| Method/path | Behavior |
| --- | --- |
| `POST /api/v1/invites` | `{"uses":1,"expires_in_hours":24}`; returns metadata plus a one-time-displayed token |
| `GET /api/v1/invites` | Active invitations (up to 1000); owners see all, other inviters see their own |
| `DELETE /api/v1/invites/{id}` | Revoke invitation; owner or its creator, with invite permission |

Invitation creation requires owner status or `create_invites`. Uses range from 1–100; expiry from 1–720 hours. Each creator may have 100 active invitations. Consuming a use, creating an account, and creating its session commit together. Concurrent registration cannot exceed remaining uses; a duplicate username rolls back consumption. Revocation does not remove already registered accounts.

## Roles and members

| Bit | Permission | Implemented effect |
| --- | --- | --- |
| 1 | `view_channel` | Channel discovery, message history, attachments and events |
| 2 | `send_messages` | Channel message and attachment mutations |
| 4 | `connect_voice` | Voice-room admission, media signaling and relay credentials |
| 8 | `create_invites` | Invitation administration |
| 16 | `manage_channels` | Categories/channels and their permission overrides |

The owner always has all permissions. Every account implicitly receives the default `everyone` role (initially 7); custom assigned roles add their permissions. The owner and members with ManageRoles may change lower-ranked roles and assignments, subject to the role hierarchy. Only the owner may edit the default role. The default role is editable but cannot be deleted or explicitly assigned. Owner status cannot be changed through role assignment.

| Method/path | Behavior |
| --- | --- |
| `GET /api/v1/roles` | Authenticated role listing |
| `POST /api/v1/roles` | ManageRoles: `{"name":"friends","permissions":7,"position":1,"display_separately":false}` |
| `PUT /api/v1/roles/{id}` | ManageRoles: replace name and permission bits; optional rank/display settings preserve their values when omitted |
| `DELETE /api/v1/roles/{id}` | ManageRoles: delete a lower-ranked custom role and its assignments/overrides |
| `GET /api/v1/members?after={id}` | Owner: up to 100 members ordered by UUID; use the last ID for the next page |
| `PUT /api/v1/members/{id}/roles` | ManageRoles: `{"role_ids":["role UUID"]}` replaces lower-ranked members’ assignments; `[]` clears custom roles |

Permission bits must be explicit and in the range 0–32767. Assignments are limited to 32 unique existing custom roles and replace atomically. Existing sessions read updated roles on subsequent requests; there is no authorization cache.

Servers advertising `community.role_sections` accept an optional boolean
`display_separately` on role create/update. New and migrated roles default to false;
omission on update preserves the saved value. `/users` and `/users/{id}` include
optional `display_role: {id, name, position}` for the highest-ranked enabled role,
including the implicit default role; equal ranks resolve by role ID. This is
public display metadata, not an authorization grant. Clients group online users
once under this role, then ungrouped Online and Offline, sorting names within each
section. Existing `directory.changed` hints cover role and assignment changes.


## Categories and channels

Category/channel names contain 1–64 characters. Mutations require `manage_channels` or owner status. Text and voice channel kinds are immutable.

| Method/path | Behavior |
| --- | --- |
| `GET /api/v1/categories` | Visible categories; managers also see empty/hidden categories |
| `POST /api/v1/categories` | `{"name":"Friends"}` |
| `PUT /api/v1/categories/{id}` | Replace name |
| `DELETE /api/v1/categories/{id}` | Remove category; channels become uncategorized and lose category overrides |
| `GET /api/v1/channels` | Visible channels with effective permissions |
| `GET /api/v1/channels/{id}` | Visible channel; inaccessible/missing channels return 404 |
| `POST /api/v1/channels` | `{"name":"general","kind":"text","category_id":null}`; kind is `text` or `voice` |
| `PUT /api/v1/channels/{id}` | Replace name/category, preserving original kind; send all fields |
| `DELETE /api/v1/channels/{id}` | Remove access to the channel and its media/history; retained rows age out under retention |
| `GET /api/v1/{categories\|channels}/{id}/permissions` | Manager: list role overrides |
| `PUT /api/v1/{categories\|channels}/{id}/permissions/{role}` | Manager: `{"allow":1,"deny":0}` |

Overrides may contain only channel bits (1, 2, 4), with no bit in both allow and deny. Apply the role union, then category overrides, then channel overrides. At each scope, union all applicable grants/denials and let denials win. Channel overrides can restore a category-denied permission. `{"allow":0,"deny":0}` clears the effect of an override. Invitation/management permissions remain instance-wide. Owners bypass overrides; managers can administer a channel without read access, but metadata discovery still applies read checks.

## Request limits and lifecycle

Login allows 30 attempts per immediate peer IP and 10 per normalized username per five minutes; registration allows 10 per peer IP per five minutes. Authenticated mutations allow 60 per account per minute. These are process-local fixed windows, with bounded, sharded storage; limit/saturated limiter state returns 429. By default the server uses the immediate peer IP. When `HAUNT_TRUSTED_PROXIES` contains that peer, a single valid `X-Forwarded-For` address replaces it; chains and malformed headers are ignored. The proxy must overwrite the header. Limits reset on restart and do not coordinate multiple processes.

Expired sessions/invitations are rejected immediately. A cancellable maintenance loop removes expired/spent credential rows in batches of 1000 each minute, and at startup. Graceful shutdown drains HTTP work, stops maintenance, closes/waits for WebSocket and media workers and relay sockets, then closes the PostgreSQL pool. Schema/owner creation uses startup synchronization; invitation consumption and session limits use narrow invitation/account row boundaries. Ordinary requests do not share a global database mutex.

Account disabling is implemented; physical account deletion and ownership transfer are not offered by this API.

## Passwords and account administration

| Method/path | Behavior |
| --- | --- |
| `PUT /api/v1/me/password` | `{ "current_password":"...", "new_password":"..." }`; 204, revokes **all** of your sessions |
| `PUT /api/v1/admin/members/{id}` | Owner: `{ "disabled":true }` or false; disabling revokes sessions and created invitations |
| `PUT /api/v1/admin/members/{id}/password` | Owner: `{ "new_password":"..." }`; resets a non-owner password and revokes sessions |
| `GET /api/v1/users?after={id}` | Authenticated profile directory, UUID order, 100 per page |
| `GET /api/v1/users/{id}` | Authenticated local profile: ID, username, owner flag, creation time, display name, bio, accent color and avatar version |

Owner recovery is host-only: `haunt -owner-password-file /protected/file`. The owner cannot be disabled/reset through member administration. Directory profiles omit full role assignments, disabled state, password hashes and session data; they may include the selected member-list display role. Existing disabled accounts remain identifiable as historical message authors.

## Conversations and messages

| Method/path | Behavior |
| --- | --- |
| `GET /api/v1/conversations?after={id}` | Visible text/voice channel chats and your DMs, UUID order, at most 100 per page |
| `POST /api/v1/dms` | `{ "members":["other user UUID"] }`; includes the caller, 2–16 unique members; same set returns the same conversation |
| `GET /api/v1/conversations/{id}/messages?after=0&limit=100` | Ascending sequence page, default/max 100 |
| `POST /api/v1/conversations/{id}/messages` | `{ "request_id":"new UUID", "body":"text", "attachment_ids":[] }`; 201 |
| `PUT /api/v1/conversations/{id}/messages/{message}` | Author: `{ "body":"replacement text" }`; 200 |
| `DELETE /api/v1/conversations/{id}/messages/{message}` | Author: stores a tombstone; 200 |

A text or voice channel's conversation ID is its channel ID. Voice-channel chat requires view/send permissions independently of voice-connect permission, persists after calls end, and uses the same history, event, attachment and retention rules as text channels. Reading or sending chat does not join a call. The `messages.voice_channels` capability advertises this behavior. DMs always require membership, including for the owner. This API restriction does not prevent the host/database operator from reading stored DMs.

Messages contain `id`, `conversation_id`, `author_id`, `seq`, `body`, `attachment_ids`, `created_at`, optional `edited_at`, `deleted`, and `request_id`. The server assigns IDs, authors, acceptance times, and sequences. Edits preserve original age/order. Deleted bodies and attachment links are cleared, old replay copies are removed, and associated attachments expire. Deleted messages cannot be edited again.

Bodies require nonblank text or at least one attachment, valid UTF-8 without NUL, and at most `min(65536, (text_max_bytes-2048)/4)` UTF-8 bytes. At most eight distinct attachment IDs are accepted. Serialized message size must also fit in three quarters of the text budget, accounting for JSON escaping. Invalid requests do not consume a sequence. Attachments must be ready, unexpired, owned by the author, in the same conversation, and not previously claimed.

Retries use a caller-generated `request_id`, unique per author across conversations. Identical retries return the currently retained message; changed content/conversation/attachment lists return 409. **Deduplication lasts only while the message row is retained.** After pruning, old request IDs are no longer remembered. Clients must not automatically resubmit a stale offline outbox beyond retained history.

History returns `messages`, `last_seq`, `pruned_through`, `event_cursor`, and `gap`. `gap` means the requested sequence precedes the retention boundary or exceeds the current sequence. Expired messages are hidden immediately, before maintenance. Page with the last returned message sequence. A multi-page fetch is not one long snapshot: reconcile edits/events using the earliest page's event cursor. A `history.pruned` event reports server retention and does **not** request deletion of client-held older history. An explicit `message.deleted` tombstone has separate meaning.

Text accounting is serialized message JSON plus 64 bytes per row, and serialized event JSON plus 64 bytes per replay row. Both channel and DM data share one strict cap. PostgreSQL page/index/WAL/metadata overhead is separate.

## Attachments

`POST /api/v1/conversations/{id}/attachments?filename=photo.png&size=1234` accepts the raw file body (not multipart). `size` must match received bytes; `Content-Length`, when supplied, must match too. The filename is display metadata, never a filesystem path. Success returns 201 with `id`, `conversation_id`, `user_id`, `filename`, detected `content_type`, `bytes`, and `expires_at`. Include the returned ID in one message creation request.

`GET /api/v1/attachments/{id}` requires conversation read permission and supports HTTP ranges/HEAD. `DELETE /api/v1/attachments/{id}` requires ownership and conversation write permission, returning 204. Unavailable/expired files return 404. Defaults are 2 MiB/image, 10 MiB/video, seven days from accepted upload, and 1 GiB of reserved capacity. In-progress uploads reserve their full declared size. A full store returns 507 `attachment_storage_full`; other messages/history remain available.

Allowed containers are PNG, JPEG, GIF, WebP, MP4 and WebM, identified from content and inspected by `ffprobe`. Exactly one video/image stream, up to two audio streams for videos, image sides ≤8192, video sides ≤4096, at most 32 million pixels, average video rate ≤120 fps and duration ≤300 seconds. This is bounded container/stream validation, not a guarantee that every frame will decode in every client. Probe execution is limited to five seconds with local-file protocols and bounded output. Two uploads/probes run concurrently; overflow returns 503. Uploads are additionally limited to ten per account/minute and recheck session/access after receiving bytes.

An expired/deleted file becomes unavailable before physical deletion. Capacity is released only after removal succeeds. A cleanup failure remains accounted and retries on maintenance/startup. Unclaimed uploads also expire. Pruning message history does not extend or reset attachment expiry.

## Event gateway

Connect to `GET /api/v1/events` over WSS. Within five seconds send a text frame:

```json
{"type":"authenticate","token":"bearer token","after_event":0}
```

`after_event` is optional. The server responds with `{"type":"ready","instance_id":"...","event_cursor":123}`. Authentication is in the first frame, never a URL token. To replace subscriptions:

```json
{"type":"subscribe","conversations":["conversation UUID"],"after_event":123}
```

This legacy explicit-subscription command permits at most 64 distinct conversations; success is `subscribed`. New clients should use the account feed described below. Use a history cursor for initial/reconnect synchronization, then deduplicate events/messages by their IDs. Changes during the disconnected interval are recovered from the committed replay log. Initial synchronization can connect/authenticate, fetch HTTP history, and subscribe from that history's cursor. When refreshing several conversations use the earliest cursor across their fetched pages.

Durable events are `{ "id":124, "type":"message.created", "conversation_id":"...", "data":{...message...} }`, `message.updated`, `message.deleted`, or `history.pruned` with `data.pruned_through`. IDs are monotonically increasing and may skip events for other conversations. Replay retains at most an hour, 10,000 rows and one quarter of the text budget. Reads are bounded to 100 events/1 MiB per batch. It is not permanent storage.

If replay is no longer complete, `sync.required` includes the current `event_cursor`. Fetch fresh history and resubscribe with an appropriate cursor; retaining the earliest history cursor avoids missing concurrent changes. Permission loss sends `subscription.revoked`; session expiry/revocation disconnects the socket. Checks/replay normally run every second. Previously authorized in-flight work may finish during concurrent permission changes.

Limits: 512 sockets per process, four per device session, 120 commands/minute per connection, eight pending commands, outbound queues ≤128 frames **and** 1 MiB. Slow clients are disconnected for resynchronization. Clients must recover after any abnormal close rather than assuming every enqueued update arrived. The server pings every 20 seconds and bounds socket writes/ping responses to five seconds.

## Voice, camera and screen sharing

1. `POST /api/v1/voice/{channel}/admissions` requires view/connect permission and returns a one-use `admission_id`, `room_id`, one-minute `expires_at`, and `ice_servers`. When the host relay is enabled, `ice_expires_at` accompanies automatically supplied credentials.
2. On the authenticated event socket, send `{ "type":"media.join", "admission_id":"..." }`. `media.joined` returns the local `participant`; `GET /api/v1/voice/{channel}` lists current participants. Participant IDs are per join, not user IDs. `joined_at` is the UTC RFC3339 time that participant joined, also included in `voice.presence`. State updates and track changes preserve it; leaving and rejoining creates a fresh timestamp. Compute elapsed duration on the client without polling.
3. Create a publishing peer with microphone (`audio`), camera/screen video, and optional `camera_audio`/`screen_audio` tracks and send `{ "type":"media.publish", "tracks":{"audio":"client track ID","camera":"...","screen":"..."}, "sdp":{"type":"offer","sdp":"..."} }`. Omit unused tracks. `media.answer` returns a unique `peer_id` and gathered SDP answer. One published track per purpose, at most five media sections; simulcast/RID and data channels are not part of the SFU protocol.
4. `media.tracks` announces a participant's actual incoming tracks, purpose, `track_id`, and negotiated codec parameters. For each other publisher, create a receiving peer with matching receive transceivers in audio/camera/screen/camera_audio/screen_audio order, then send `{ "type":"media.subscribe", "publisher_id":"participant UUID", "sdp":{...offer...} }`. `media.answer` includes `publisher_id`, unique `peer_id`, and SDP.
5. Optional trickle candidates use `{ "type":"media.ice", "peer_id":"returned peer UUID", "candidate":{...ICECandidateInit...} }`. Fully gathered client offers also work. Peer IDs change on replacement, preventing old candidate updates from affecting a new peer.

The SFU preserves compatible MIME/clock/channel/fmtp parameters from negotiation. Pion's registered codecs include Opus, VP8, VP9, H.264, AV1 and others; there is no VP8-only rule. Incompatible receiver offers fail explicitly. No codec conversion/mixing/rendering is performed. RTP payloads stay encoded; per-hop packet identifiers/extensions are mapped, and RTCP keyframe requests are forwarded. Each receiver has its own bounded 128-packet queue; slow receivers drop packets without stalling other participants.

`media.publish` with a new offer replaces that participant's publishing peer. Track changes trigger `media.tracks`; receivers must recreate their corresponding subscriptions. Old peer IDs are invalid. A new join creates a new participant ID. Client code should serialize signaling commands and match responses by peer/publisher IDs while continuing to process asynchronous events.

`media.state` accepts a complete `state` with `muted`, `deafened`, `camera_off`, and `screen_off` booleans; the SFU enforces these directions. Microphone mute affects only `audio`; deafen blocks all incoming audio sources. Turning camera or screen off blocks both its video and its associated audio. `media.source_audio` advertises the additional audio purposes. `media.leave` leaves the room. Asynchronous events include `media.participant.joined`, `media.participant.left`, `media.participant.state`, `media.tracks`, and `media.revoked`. Room/session revocation and socket loss release owned peers. Defaults: 16 participants/room, 64 rooms, 512 total peer connections, and four unconsumed admissions per session. These are safety bounds, not measured capacity promises.

When host relay support is enabled, `GET /api/v1/voice/{channel}/ice` renews authorized ICE details (`ice_servers`, `expires_at`). Clients should renew and restart ICE before credential expiry, automatically. Relay packets remain opaque; credentials are short-lived and process-local, so restarting the server invalidates previously issued relay credentials. Existing allocations can live until their last accepted lifetime expires. Call participation and SFU authorization remain separate from relay credential validity. No client mode selector is required.

## Operations

Owner-only `GET /api/v1/admin/stats` returns retained text, reserved attachments, row counts, database/relation/index sizes, estimated dead tuples, cluster WAL bytes since its statistics reset, pool acquisition counts/time, quota-lock acquisition counts/time, queued event bytes/overflows, SFU peers/drops, and relay allocations/rejections. WAL statistics are cumulative cluster activity, not physical WAL disk usage or this installation's exclusive share.

Owner-only `POST /api/v1/admin/maintenance` runs bounded credential/attachment maintenance and text pruning immediately, returning 204. The normal loop runs every minute. Host backup/restore and configuration procedures are in [operations.md](operations.md).

## Automatic media transport

Admissions include `transport` (`webrtc` or `websocket`), `ice_transport_policy` for WebRTC, and `media_path` for the WebSocket fallback. The host chooses these settings. Cloudflare/external TURN can supply relay-only ICE credentials; cloudflared without external TURN selects encoded WebSocket chunks. See [media-transports.md](media-transports.md) for credential expiry/rejoin and the full fallback contract. `media.reconnect_required` warns before managed credentials expire; fetching `/ice` alone does not replace an active SFU peer.

## Profiles, server appearance and live voice rosters

Schema version 5 adds profile display names/bios/accent colors, versioned avatars, server name/description/accent/icon/banner, and channel topics. `PUT /api/v1/me/profile` changes only the current account's profile; usernames and account IDs remain stable. Only the owner can update `/api/v1/instance/settings`, the server icon or banner. A saved server name takes precedence over `HAUNT_NAME`; before the first save, discovery uses the configured name.

Image PUTs accept raw PNG/JPEG/GIF bytes. Avatar/icon input is limited to 2 MiB and 1024 × 1024 pixels; banners to 4 MiB and 2048 × 1024. Static images are decoded and re-encoded as PNG, preserving transparency and stripping metadata/trailing content. Schema version 7 also accepts animated GIFs up to 20 MiB with the same dimension limits, up to 1000 frames and 256 Mi decoded frame pixels. Each frame is validated separately with bounded memory; GIF bytes, frame timing, transparency and looping are preserved. Decoding concurrency is bounded and art is stored separately from expiring message attachments. Avatar downloads require authentication; server icon/banner downloads are public, as is instance discovery. Metadata responses contain version identifiers, never image bytes or access tokens. DELETE removes the corresponding image. The browser editor also accepts WebP and converts/crops locally before uploading.

Channel creation/update accepts an optional `topic` (512 characters). Omitting it on update preserves the existing topic; an empty string clears it. Existing role and channel permission checks apply.

The `media.presence` capability advertises `voice.watch` on the authenticated events socket. After `voice.watching`, `voice.presence` supplies a full replacement `rooms` map on change, sampled once per second. Each member includes participant ID, user ID, stable UTC `joined_at` and mute/deafen/camera/screen state. `media.joined_at` advertises timestamps on presence and call snapshots. Only channels the principal can both view and join are included. Empty rooms and rooms whose access was revoked disappear from the next map. This is ephemeral voice presence, not general account-online status or a durable replay event.

## Account feeds and read state

When `events.account_feed` is advertised, send `events.watch` on the authenticated `/api/v1/events` socket. It covers all readable channels and member DMs, including future additions, with no explicit subscription limit. Persist `events.checkpoint.event_cursor` after applying preceding durable events. `directory.changed` invalidates channel/member/permission/emoji metadata; `read.state` contains changed read counters, and `read.sync` requests a fresh snapshot. State hints are ephemeral; message events remain replayable. Full details are generated in `websocket.json`.

`GET /api/v1/read-state?after=<conversation UUID>` returns up to 100 authorized states and an optional `next` cursor. Each state includes `kind`, `conversation_id`, `read_seq`, `last_seq`, `unread_count`, `mention_count` and `event_cursor`. Counts cover retained, nondeleted messages from other authors. `PUT /api/v1/conversations/{id}/read` with `{ "seq": 123 }` advances the account’s read position monotonically, clamped to the conversation’s current last sequence. All devices see the change. A message’s optional `mention_user_ids` contains resolved immutable username mentions. There are no role/everyone broadcast mentions.

## Server emojis

The `community.emojis` capability exposes an authenticated `GET /api/v1/emojis` catalog (IDs, lowercase names, versions and animation flags; no image bytes). Owners use `PUT /api/v1/emojis/{name}` with raw PNG/JPEG/GIF bytes to create or replace an emoji, and `DELETE /api/v1/emojis/{id}` to remove it. Downloads at `GET /api/v1/emojis/{id}/image` require server authentication. Names contain 2–32 ASCII letters, digits or underscores. Limits are 100 emojis per instance, 1 MiB/image and 256 × 256; GIFs use the same bounded frame validation as profile art. Replacement preserves the ID and changes the version. Quota checks serialize only the brief per-instance emoji write transaction; image decoding occurs before it.

Clients insert `:name:` in ordinary message bodies and resolve against that instance's catalog in both channels and DMs. Code spans and URLs remain literal; unknown/deleted names remain text. Replacing an image updates previous uses of that name. Catalog changes issue the existing coalesced `directory.changed` gateway hint; initial connect/reconnect retrieves the current catalog. No per-emoji listener or periodic polling is needed. The browser/desktop editor converts static PNG/JPEG/WebP to a bounded PNG, while GIF uploads retain animation.


Invite links use the client-readable format `<server-origin>/#invite=<invitation>`;
registration still sends the invitation only in its JSON body. The core SDK exposes
`inviteLink` and `parseInviteLink`, and `Client.login` accepts a full invite link.
See [moderation](moderation.md#temporary-password-recovery) for restricted temporary
password sessions and the reset permission. The generated login contract includes
`password_change_required`; clients must complete replacement before saving a session.

## Online user presence

When discovery advertises `presence.online`, send `{"type":"presence.watch"}`
on the existing authenticated events socket. `presence.watching` acknowledges the
subscription, followed by `{"type":"presence.snapshot","online_user_ids":["user UUID"]}`.
This is a complete replacement set, capped by the gateway's 512-connection limit;
users absent from a received snapshot are offline. Send `enabled:false` to stop
watching; enabling again always supplies a current snapshot.

Online means at least one authenticated event connection to this installation,
including background clients and additional devices. HTTP login alone and voice
membership are not presence signals. The first connection and final disconnect
change presence; extra tabs/devices do not generate redundant updates. Graceful
disconnect is immediate; abrupt network loss is detected by the existing socket
heartbeat, and revoked sessions by existing gateway validation. Presence is
in-memory and scoped to one installation. It creates no database rows, presence
queries, extra polling timer or replay history. Snapshots share the gateway's
bounded output queues and 100 ms coalescing. Unchanged state emits no frames.

Clients clear cached presence to **unknown** when their connection is lost and
wait for a fresh snapshot after reconnect. Older servers without this capability
also have unknown status. The SDK stores presence separately from message state
and notifies only subscribers for users whose online status changed.
