# Media hosting and client protocol

Transport is an installation setting. Clients read `transport` from an authorized voice admission and connect automatically. Client call controls use the installation's transport policy. Both paths use the same rooms, participants, device sessions, mute/deafen state and permission revocation.

| Host settings | Admission transport | Network path |
| --- | --- | --- |
| Normal host | `webrtc` | Client WebRTC ↔ instance SFU |
| `HAUNT_MEDIA_TURN_HOST` and credentials | `webrtc`, ICE policy `relay` | Clients and SFU use the external TURN host |
| Cloudflare Realtime TURN key and token | `webrtc`, ICE policy `relay` | Clients and SFU use Cloudflare's managed TURN service |
| `HAUNT_CLOUDFLARED_ENABLED=true`, external TURN configured | `webrtc`, ICE policy `relay` | API/signaling over cloudflared; WebRTC media through external TURN |
| `HAUNT_CLOUDFLARED_ENABLED=true`, no external TURN | `websocket` | API, signaling and encoded media over HTTPS/WSS through cloudflared |

STUN alone does not count as an external relay. The optional built-in relay does not make a host behind an HTTP tunnel publicly reachable, so it does not suppress the WebSocket fallback. Configured but unavailable external TURN produces an explicit media error; it does not silently change the protocol during a call.

## External TURN

Set `HAUNT_MEDIA_TURN_HOST` to an IP or DNS hostname without a scheme/port, plus `HAUNT_MEDIA_TURN_USERNAME` and `HAUNT_MEDIA_TURN_PASSWORD`. UDP and TCP use `HAUNT_MEDIA_TURN_PORT` (3478 by default). An optional `HAUNT_MEDIA_TURN_TLS_PORT` adds TURN over TLS; use a hostname matching its certificate. The operator supplies the TURN service and firewall rules. The shortcut provides credentials to authorized callers, so use a dedicated restricted relay account. `HAUNT_ICE_SERVERS` supports advanced ICE JSON configuration.

## Cloudflare hosting

Set `HAUNT_CLOUDFLARED_ENABLED=true`. To use managed WebRTC connectivity, also set `HAUNT_CLOUDFLARE_TURN_KEY_ID` and `HAUNT_CLOUDFLARE_TURN_API_TOKEN` from a Cloudflare Realtime TURN key. The API token remains server-side. The server obtains separate temporary credentials for each device session and for its SFU, caches them with bounded concurrency, and returns only client credentials. Refresh begins when less than an hour remains. Provider errors are returned as `503 media_relay_unavailable`, without exposing the provider response or token. This integration follows Cloudflare's [credential API](https://developers.cloudflare.com/realtime/turn/generate-credentials/).

Cloudflare Realtime TURN carries the WebRTC media separately from the HTTP tunnel. Cloudflare Stream is a broadcast product; this server does not implement Stream WHIP/WHEP broadcasting or replace its SFU with Cloudflare's SFU. No Cloudflare account resources are provisioned automatically.

The container example uses cloudflared's `auto` transport, which prefers QUIC for the connector-to-edge link and can fall back to HTTP/2. The public application endpoints remain HTTPS/WSS; this does not expose a custom WebTransport endpoint. See Cloudflare's [tunnel protocol setting](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/run-parameters/#protocol) and [operations](operations.md).

## WebRTC client contract

Create an admission, open/authenticate the event socket, then send `media.join`. Apply `ice_servers` and `ice_transport_policy` from the admission to every publishing/receiving peer. Publish an SDP offer and an audio/camera/screen purpose-to-track-ID mapping; subscribe with an offer supporting the source codecs. The SFU forwards encoded RTP without decoding or transcoding. Codecs are negotiated; they are not limited to VP8.

Instances advertising `media.layers.v1` accept video layer metadata on publication.
Each layer declares its RID, dimensions and FPS. Clients publish the selected
capture quality and distinct lower copies bounded to 720p/30 and 360p/15.
`media.subscribe` accepts initial per-source quality selections;
`media.subscription.quality` changes only the requesting viewer's subscription
and receives an acknowledgement. The relay switches on complete keyframes,
preserves outgoing RTP sequence/timestamp continuity and discards queued packets
from the previous layer. Keyframe requests follow the requested source layer.
Legacy sources remain single-quality; unsupported manual choices are rejected.

The client defaults each remote tile to Auto and samples incoming statistics
every two seconds. Two consecutive samples above 5% packet loss or 10% decode
drops trigger a downgrade. Twenty seconds below 1% loss and 2% decode drops
permit an upward probe, with a ten-second switch cooldown. Missing metrics and
unchanged screen frames are ignored. A fixed viewer choice suspends automatic
changes until Auto is selected again. Viewer choices never alter source capture.

When temporary credentials are used, `ice_expires_at` is the earliest relevant client/SFU expiry. Two minutes before managed credentials expire, the server sends `media.reconnect_required`. Obtain a new admission, leave/rejoin, republish and recreate receiver subscriptions before expiry. Merely fetching `/ice` does not replace existing SFU allocations or extend participant lifetime. Expired managed participants are removed. Built-in host-relay credentials can be refreshed through `/ice`; recreate/restart affected peers as needed.

## Encoded WebSocket client contract

The machine-readable [websocket.json](websocket.json) is generated from the server command and payload types and includes all header offsets, limits and message sequencing. The [API guide](api.md) describes event authentication, permissions and recovery.

1. Create a voice admission. For `transport=websocket`, `media_path` is `/api/v1/media` and no ICE servers are required.
2. Authenticate `/api/v1/events` and consume the admission with `media.join`.
3. Open `/api/v1/media` and send `{type:"authenticate",token,participant_id}` within five seconds. The joined participant must belong to that exact device session. Wait for `ready` with protocol `chatxo-encoded-v1`.
4. Send `configure` with up to five tracks, one per purpose (`audio`, `camera`, `screen`, `camera_audio`, `screen_audio`). Use a WebCodecs codec identifier, decoder configuration as base64 `description`, and the applicable dimensions or sample rate/channel count. Empty tracks stops publication. Wait for the server's `tracks` response and its assigned revision.
5. Send one encoded elementary chunk per binary message. The 24-byte input header contains version, purpose, key/delta flag, reserved byte, revision, microsecond timestamp, microsecond duration and sequence. Sequence numbers are independent per track and contiguous after the first, including uint32 wrap. Output prepends the publisher's authenticated 16-byte UUID. Receivers use the declared decoder configuration and timestamps.
6. Honor `keyframe` requests for camera/screen. New receivers and resumed video wait for a keyframe. Reattaching a media socket requires reconfiguration and a new revision. Losing the event socket requires a new admission and participant as well.

Limits include 512 KiB per input frame, 64 KiB per audio frame, 8 MiB/s and 600 input frames/s with bounded bursts. Each receiver queue holds at most 64 frames/2 MiB; overflow disconnects that receiver. Control messages and keyframe requests are rate limited. Clients must regulate bitrate and send buffering, synchronize audio/video, and handle reconnects. Reliable WebSocket delivery can increase latency under packet loss.

Browsers require WebCodecs encoders/decoders and compatible codecs; desktop clients can use native codec APIs with the same framing. Device and browser compatibility checks are described in [testing](testing.md).

Both transports honor the publisher's selected capture resolution and frame rate.
The WebSocket path has one quality per source and does not implement viewer
adaptation. Camera/screen audio remains separate from microphone audio.

Instances advertising `media.moderation.v1` expose separate participant
`self_state` and moderator `restriction` fields alongside effective `state`.
Moderator deafen blocks incoming audio and outgoing microphone audio; moderator
mute blocks outgoing microphone audio. Checks apply before forwarding and queued
delivery on both transports. Restrictions persist independently of sessions;
clearing them restores the participant's own choices.
