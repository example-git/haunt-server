# Client development reference

The server uses [swaggest/openapi-go](https://github.com/swaggest/openapi-go) to generate OpenAPI 3.1 from registered routes and shared Go request/response types. [swaggest/swgui](https://github.com/swaggest/swgui) embeds Swagger UI assets in the binary, so the reference works without a CDN.

- `/docs/`: interactive HTTP endpoint reference, including bearer authentication and Try it out.
- `/openapi.json`: machine-readable HTTP operations, schemas, parameters, statuses and authentication for tools supporting OpenAPI 3.1.
- `/websocket.json`: generated command/payload schemas, required fields by command, signaling sequencing, and encoded media binary framing.
- `/endpoints.md`: generated endpoint inventory.

These are public developer documents containing schema metadata, not live account/configuration secrets. Try it out sends real requests; bearer tokens must belong to the intended installation. The UI does not persist authorization or send the specification to an external validator. Swagger's HTTP tooling does not open WebSocket connections.

Generate files without starting the server, opening PostgreSQL, loading a deployment environment or requiring Cloudflare credentials:

```sh
go generate ./internal/httpapi
# Verify generated files are current:
go run ./cmd/apidocs -check -client-out client/static/api
```

Outputs are `docs/openapi.json`, `docs/websocket.json` and `docs/endpoints.md`. Matching JSON snapshots are also exported to `client/static/api/` for standalone client builds. They ship in both Linux packages. Native verification and packaging reject stale files. Generation enumerates the same registration functions as the running HTTP server and rejects undocumented or stale route contracts. HTTP integration tests validate returned JSON against generated response schemas. Request schemas are derived from the exact structs used for decoding; constraint tags describe validation enforced by the services.

When adding a route, add its contract in `internal/httpapi/openapi_contracts.go`, use a shared request/response type, regenerate and run the relevant tests. Update WebSocket sequencing metadata when adding a command; OpenAPI cannot infer protocol state machines or binary framing from Go types alone.

Start with `GET /api/v1/instance` for installation identity, capabilities, transport and configured limits. Keep credentials, event cursors, messages and connections scoped to each instance. Implement invitation/login, histories and recovery, uploads, then media using [api.md](api.md) and [media-transports.md](media-transports.md). The browser/Electron client and reusable messaging SDK now live under [`client/`](../client/README.md); that directory documents actual client behavior, Android integration boundaries, builds and verification.

`messages.replies` adds optional `reply_to_id` fields to message creation and returned messages. The target must be a retained, undeleted message in the same conversation when the reply is first committed. Keep the identifier when its target is later edited, deleted or pruned so clients can show a tombstone or unavailable-reference state; retries use the original request ID and reply target.

## Rich message previews

`messages.embeds` advertises three generated authenticated GET operations:

- `/api/v1/conversations/{id}/messages/{message}/embeds` returns `{embeds: [...]}` with each card's stable `index`, destination `url`, `kind`, plain-text title/description/site, optional author/published time/theme color, image availability and optional validated YouTube ID. Only retained, visible, undeleted messages qualify.
- `/api/v1/conversations/{id}/messages/{message}/embeds/image?index=0` returns checked image bytes. Use an authenticated fetch and a revocable object URL; do not put bearer tokens in image URLs.
- `/api/v1/attachments/{id}/metadata` returns authorized attachment metadata before choosing automatic image display or explicit video download.

Clients should fetch only near visible messages, cancel when leaving, and treat failures as a normal link fallback. No new gateway subscription is needed and preview requests never mark a message read. Message edits/deletion or permission changes must discard displayed cards. Return values contain plain text, never executable markup. A YouTube ID permits constructing a fixed privacy-enhanced player URL, but does not authorize arbitrary provider HTML. Metadata follows [OpenGraph](https://ogp.me/); the optional player uses [YouTube's embed parameters](https://developers.google.com/youtube/player_parameters) and [privacy-enhanced host](https://support.google.com/youtube/answer/171780).

The server fetches HTTP(S) on standard ports only, rejects private/reserved addresses and mixed public/private DNS answers, pins each connection to a validated address, and revalidates redirects. It ignores environment proxies and forwards no caller credentials, cookies or referer. Eight shared concurrent fetches, six-second deadlines, 1 MiB HTML, 4 MiB images, 16 megapixels, 512 cache entries and 32 MiB total cached data bound remote work. Metadata/images cache for 15 minutes and failures for 30 seconds. Each user has separate 240-per-minute card/image limits. Saturation returns 503 with Retry-After; the browser retries card requests twice. Preview data is ephemeral and does not enter message retention or event replay storage.

## Voice timestamps and source audio

`media.joined_at` supplies UTC `joined_at` on each participant and roster entry. Use a shared client clock for elapsed timers. A new participant gets a fresh time; state/track updates preserve the original value. No timer messages or periodic HTTP calls are needed.

`media.source_audio` adds `camera_audio` and `screen_audio` purposes. The transceiver/wire order is `audio`, `camera`, `screen`, `camera_audio`, `screen_audio`; encoded-purpose bytes are respectively 0–4 and the existing three values are unchanged. The two new purposes follow audio codec/size limits and never request video keyframes. Deafen applies to all audio, microphone mute to `audio` only, camera-off to camera video/audio, and screen-off to screen video/audio. Updated clients request extra capture audio only from a server advertising this capability. Upgrade participating clients together; legacy clients may not understand the additional purposes.

Playback levels are local settings keyed by listener account, server and remote user, with separate 0–100% microphone/camera/screen values. Do not store them on ephemeral participant IDs or send them as publisher state. Both browser transports expose separate audio streams and apply the selected level at the playback element.

## Restoring a client session from backup

After checking `GET /api/v1/instance` against the saved installation ID, submit
`POST /api/v1/sessions/restore` with the backup's bearer token and a `device_name`.
Capability: `sessions.restore`. The 201 response is a normal login envelope with a
fresh token/session ID for the same account. Store only that new token on the
receiving device. The source remains signed in; either device can log out without
revoking the other. Expiry is inherited without extending it, including an absent
expiry for remembered sessions. The per-account 20-session cap includes restored
devices; requests are limited to 10 per five minutes per account. Temporary
password sessions, revoked/expired sessions and banned/disabled accounts cannot
restore. Revalidate under a per-account database lock before inserting, so resets
and concurrent restores cannot bypass the policy.
