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

## Hosted web client origin

Actions builds include the standalone web client's HTTPS origin from
`client/web-deployment.json`. It is added to the server's allowed client origins
alongside its own public URL and explicit `HAUNT_CLIENT_ORIGINS`. Release binaries,
container images and application-update bundles use the same address. Local
builds can configure additional origins through `HAUNT_CLIENT_ORIGINS`.

## Rich message previews

`messages.embeds` advertises authenticated metadata and image operations:

- `/api/v1/conversations/{id}/messages/{message}/embeds` returns `{embeds: [...]}` with each card's stable `index`, destination `url`, `kind`, plain-text title/description/site, optional author/handle/avatar/published time/theme color, media descriptors and validated player identifiers, and opaque image cache keys. A failed link has `status: "unavailable"`; successful links in the same message remain available. Only retained, visible, undeleted messages qualify.
- `/api/v1/conversations/{id}/messages/{message}/embeds/image?index=0` returns checked image bytes. Select a post image or video poster with `asset=0` through `asset=3`, or the author image with `avatar=true`. Use an authenticated fetch and a revocable object URL; do not put bearer tokens in image URLs.
- `/api/v1/attachments/{id}/metadata` returns authorized attachment metadata before choosing automatic image display or explicit video download.

X/Twitter post links use the FxTwitter JSON API with its HTML metadata fallback. X, Twitter and supported fixup aliases share a post cache key while each card keeps its original destination link. Cards contain up to four assets. The browser renders plain text and native image/video controls.

Desktop downloads report progress, cancel/retry and file reveal under History &
downloads. Server-proxied video saves assemble validated byte ranges into a
temporary file and publish the file only when complete. Media cache budgets and
clearing affect cached media, leaving message archives intact. Backup & restore
runs text archive compression/import in a worker; downloaded-message search also
runs in a worker. Cancelling import can retain chunks already committed.

Desktop links use `haunt://open?server=<origin>&channel=<uuid>&message=<uuid>`.
Channel and message parameters are optional, but a message needs a channel.
Links reuse the matching server's existing account or open its sign-in screen;
they never carry credentials. Server, channel and message menus can copy links.
Launch links queue until account restoration and renderer readiness.

TikTok and Bluesky posts resolve on each client without API accounts. TikTok uses the TikWM public resolver to obtain an MP4 source, including short share links. Bluesky uses the public AT Protocol API for text, images and HLS video, including quoted posts with media. Native video controls start playback only after a click; HLS.js supplies playlist playback where Media Source Extensions are available. These requests contact the known resolver and provider CDNs directly, exposing the viewer IP to those services but creating no connection to another member. Provider URLs are validated. Metadata and HLS fetches omit credentials and reject redirects. Desktop image requests also omit credentials, reject redirects and enforce a 4 MiB body limit. Bluesky browser images render directly from the validated official image CDN with no referrer because that CDN does not provide image-fetch CORS headers; the browser manages their HTTP cache. HLS segment requests are restricted to the Bluesky video CDN. Metadata is cached per server/account for one minute, and failed or expired video sources refresh on retry. Mixed-link requests set `client_posts=true` so the server skips posts handled locally and retains the other links' indexes.

For X video and direct audio/video playback, authenticate `GET /api/v1/conversations/{id}/messages/{message}/embeds/video-ticket?index=0&asset=0`, then use the returned opaque `ticket` with `/embeds/video?ticket=...`. This media-only ticket expires after 30 minutes and is bound to that message, its body and the original session. Every range rechecks session and message access before delivery; edits, deletion, expiry and revocation invalidate playback. The session bearer token never appears in the media URL. Video responses support single byte ranges, capped at 8 MiB per response, with 16 concurrent streams and a separate 60-per-minute ticket limit per user. MP4, WebM, MP3, Ogg audio, WAV and FLAC are accepted when the browser supports their codecs. Direct files must have a supported media signature; a filename extension alone does not qualify. Images are decoded and checked; PDF and plain-text links get file cards. Arbitrary user-supplied playlists and HTML players are not executed.

Clients should fetch only near visible messages, cancel when leaving, and show failed links with a retry action. Do not persist failed or empty results as successful metadata. No new gateway subscription is needed and preview requests never mark a message read. Message edits/deletion or permission changes must discard displayed cards. Return values contain plain text, never executable markup. Generic page previews read only the bounded HTML head: OpenGraph, Twitter card fields, description and title, stopping at the head end or body. Relative images resolve against the final page URL and load through the authenticated image proxy. Scripts, iframe markup and user-supplied player URLs are never rendered.

YouTube, Vimeo, Streamable and Twitch links use fixed official player URLs built from validated hosts and identifiers. Players load only after a click; that connection exposes the viewer IP to the selected provider. Generic previews and direct files contact remote hosts from the server, never from the viewing browser. See [OpenGraph](https://ogp.me/), [YouTube parameters](https://developers.google.com/youtube/player_parameters), [Vimeo player](https://github.com/vimeo/player.js), and [Twitch embeds](https://dev.twitch.tv/docs/embed/video-and-clips/) for provider contracts.

Image and avatar cache keys are scoped by the client to its server/account and reused across messages for the same asset within a 15-minute period. Server-fetched metadata refreshes after five minutes online and can be reused offline. Failed previews are not persisted.

The server fetches HTTP(S) on standard ports only, rejects private/reserved addresses and mixed public/private DNS answers, pins each connection to a validated address, and revalidates redirects. It ignores environment proxies and forwards no caller credentials, cookies or referer. Separate metadata and media lanes each allow eight active and 32 queued fetches, with one-second queue waits and six-second fetch deadlines. Remote work is bounded to 1 MiB HTML, 4 MiB images, 16 megapixels, 512 cache entries and 32 MiB total cached data. Metadata/images cache for 15 minutes and failures for 30 seconds. Each user has separate 240-per-minute card/image limits. Saturation returns 503 with Retry-After; the browser retries card requests up to three times. Successful page metadata persists in a separate PostgreSQL cache for 15 minutes, bounded to 2,048 entries of at most 64 KiB each; decoded images remain in the bounded memory cache. Preview data does not enter message retention or event replay storage. History reads never wait for a preview provider.

## Downloaded history and local search

The client opens the newest 100 messages first, reconciles edits/deletions, and
archives the active conversation in the background. **History & downloads** sets
an automatic-download switch, byte budget and request budget. Defaults are
64 KiB/s and 30 requests/minute, shared across connected servers in one window.
One page can arrive immediately; subsequent requests account for its decoded JSON
size. Retries also consume request budget. Hidden windows, offline state and the
browser data-saver preference suspend background work. Foreground history loads
remain available. Per-conversation pause state and archive checkpoints persist.

**Search downloaded messages** queries the local account/conversation index and
matches all normalized words, returning 50 results at a time. New messages,
edits and deletions update the index atomically; old caches migrate in cancellable
250-message batches. Search does not request remote history. Previously fetched
contiguous ranges and completed archive ranges serve navigation from local
storage. Messages outside the downloaded archive are not search results. Device
storage quotas and browser eviction still apply.

The browser also keeps a separate private text archive. Downloaded readable text
survives live-chat deletion; restoring that archive never recreates live messages
or live-search results. **Backup & restore** exports/imports compressed JSONL
text files and lets users delete saved entries. Optional Haunt Sync encrypts and
merges this archive across devices. Explicit archive deletions win over older
synced/imported entries; exported files remain independent copies. Attachment
bytes, session tokens and encryption keys are excluded from these text exports.

## Voice timestamps and source audio

`media.joined_at` supplies UTC `joined_at` on each participant and roster entry. Use a shared client clock for elapsed timers. A new participant gets a fresh time; state/track updates preserve the original value. No timer messages or periodic HTTP calls are needed.

`media.source_audio` adds `camera_audio` and `screen_audio` purposes. The transceiver/wire order is `audio`, `camera`, `screen`, `camera_audio`, `screen_audio`; encoded-purpose bytes are respectively 0–4 and the existing three values are unchanged. The two new purposes follow audio codec/size limits and never request video keyframes. Deafen silences received playback and blocks microphone publication; camera and screen audio publication remain independent. Microphone mute applies to `audio` only, camera-off to camera video/audio, and screen-off to screen video/audio. Updated clients request extra capture audio only from a server advertising this capability. Upgrade participating clients together; legacy clients may not understand the additional purposes.

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
