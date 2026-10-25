package httpapi

// This describes protocol sequencing alongside schemas reflected from the
// actual command structs and shared domain payloads. OpenAPI covers upgrades;
// ordinary HTTP SDK generators cannot implement WebSocket state machines.
func webSocketDocument(schemas map[string]any, components any) map[string]any {
	return map[string]any{
		"format": "chatxo-websocket-v1", "version": 1,
		"components": components, "schemas": schemas,
		"authentication": "Within five seconds of opening either socket, send its authenticate text frame. Use the device-session bearer token in the frame, never in the URL. Event socket logout, revocation or disconnect removes its media participant.",
		"events": map[string]any{
			"path": "/api/v1/events", "command_schema": schemas["eventCommand"], "max_input_bytes": 131072,
			"client_messages": map[string]any{
				"presence.watch":             map[string]any{"required": []string{"type"}, "optional": []string{"enabled"}, "reply": "presence.watching", "description": "When presence.online is advertised, enable (default) or disable online-user snapshots. Online means at least one authenticated event socket on this installation, including background devices; it does not imply voice participation. Initial subscribe/resume sends a full presence.snapshot; subsequent snapshots are sent only when users first connect or finally disconnect, coalesced by the existing gateway worker. No polling or durable replay. Clear client status to unknown when disconnected."},
				"authenticate":               map[string]any{"required": []string{"type", "token"}, "optional": []string{"after_event"}, "reply": "ready"},
				"events.watch":               map[string]any{"required": []string{"type"}, "optional": []string{"after_event"}, "reply": "events.watching", "description": "Watch every currently accessible conversation, including new channels and DMs, without a subscription-count limit. Replaces explicit subscriptions. Replay durable message/history events from after_event; omitted cursor starts at the connection head. Committed PostgreSQL notifications wake bounded account workers; idle fallback runs every five seconds."},
				"voice.watch":                map[string]any{"required": []string{"type"}, "optional": []string{"enabled"}, "reply": "voice.watching", "description": "Enable (default) or disable changed voice.presence snapshots. Only watch from a foreground UI that displays rosters. Presence is ephemeral; resume sends a fresh snapshot. Each member includes joined_at, an RFC3339 UTC timestamp fixed at admission to the room. Display elapsed time locally; do not poll for timers."},
				"subscribe":                  map[string]any{"required": []string{"type", "conversations"}, "optional": []string{"after_event"}, "reply": "subscribed", "description": "Replace up to 64 authorized conversation subscriptions. Use HTTP history event_cursor for synchronization."},
				"media.join":                 map[string]any{"required": []string{"type", "admission_id"}, "reply": "media.joined", "description": "Consume a device-scoped single-use admission within one minute. At most one room participant per event socket."},
				"media.leave":                map[string]any{"required": []string{"type"}, "reply": "media.left"},
				"media.level":                map[string]any{"required": []string{"type", "participant_id", "level"}, "reply": "media.level.accepted", "description": "Requires media.levels.v1. Report the current participant's post-filter, post-gain microphone level as an integer from 0 to 100 on a fixed -60 to 0 dBFS scale. Send changes at most ten times per second and unchanged readings every 500 ms. Reports have a separate limit of 20 per second. Only unmuted senders are relayed, to other participants in the same room including deafened listeners. Mute and deafen stop outgoing reports. Discard reports after two seconds without an update and on mute, leave or reconnect. No durable replay."},
				"media.state":                map[string]any{"required": []string{"type", "state"}, "reply": "media.state.accepted", "description": "Replace self-selected muted, deafened, camera_off and screen_off booleans. Persistent channel restrictions remain independent; participant self_state reports the submitted choices, restriction reports moderator flags, and state reports effective state. muted affects microphone only; deafened blocks all incoming audio and forces muted=true. camera_off and screen_off block both video and associated source audio. These gates are enforced by the server for WebRTC and encoded WebSocket media, including queued packets."},
				"media.publish":              map[string]any{"optional": []string{"layers"}, "required": []string{"type", "sdp", "tracks"}, "reply": "media.answer", "description": "WebRTC only. SDP offer and purpose-to-track-ID map. Purposes: audio (microphone), camera, screen, camera_audio, screen_audio. Replaces prior publishing peer and invalidates its receiver subscriptions. With media.layers.v1, layers maps camera/screen to at most three representations with id (h, m, l), width, height and fps; SDP RID identifies each copy. Track metadata lists available representations. Legacy publications remain single-quality."},
				"media.subscribe":            map[string]any{"optional": []string{"qualities"}, "required": []string{"type", "publisher_id", "sdp"}, "reply": "media.answer", "description": "WebRTC only. Receiver offer must support publisher codecs. Only publishers in the same room are accessible. Optional qualities maps each video purpose to an advertised layer ID; omitted entries initially select the highest available quality. Each subscriber owns its selection; publishing settings are unaffected."},
				"media.subscription.quality": map[string]any{"required": []string{"type", "publisher_id", "purpose", "layer"}, "reply": "media.subscription.quality.accepted", "description": "Select an advertised video layer for this subscriber only. Changes take effect on a complete keyframe; media.subscription.quality.changed reports the applied layer. Unknown layers and legacy single-quality sources return media_quality_unavailable."},
				"media.ice":                  map[string]any{"required": []string{"type", "peer_id", "candidate"}, "reply": "media.ice.accepted", "description": "Use the current peer_id from media.answer; stale peer generations are rejected."},
			},
			"server_messages": map[string]any{
				"presence.watching": []string{"type"}, "presence.snapshot": []string{"type", "online_user_ids (complete unique array, at most 512 user IDs; absent users are offline)"},
				"events.watching": []string{"type", "event_cursor"}, "events.checkpoint": []string{"type", "event_cursor"}, "directory.changed": []string{"type"}, "read.sync": []string{"type"}, "read.state": []string{"type", "states"},
				"voice.watching": []string{"type"}, "voice.presence": []string{"type", "rooms"},
				"ready": []string{"type", "instance_id", "event_cursor"}, "subscribed": []string{"type", "conversations"}, "error": []string{"type", "error"},
				"sync.required": []string{"type", "event_cursor"}, "subscription.revoked": []string{"type", "conversation_id"},
				"media.joined": []string{"type", "participant"}, "media.left": []string{"type"}, "media.state.accepted": []string{"type"},
				"media.level": []string{"type", "participant_id", "level"}, "media.level.accepted": []string{"type"},
				"media.answer": []string{"type", "peer_id", "sdp", "publisher_id (receiver answers only)"}, "media.ice.accepted": []string{"type"},
				"media.participant.joined": []string{"type", "room_id", "participant"}, "media.participant.left": []string{"type", "participant_id"},
				"media.participant.state": []string{"type", "participant"}, "media.tracks": []string{"type", "participant"}, "media.revoked": []string{"type"},
				"media.subscription.quality.accepted": []string{"type", "publisher_id", "purpose", "layer"},
				"media.subscription.quality.changed":  []string{"type", "publisher_id", "purpose", "layer"},
				"media.reconnect_required":            []string{"type", "reason", "expires_at"},
			},
			"durable_events": map[string]any{"schema": schemas["durableEvent"], "types": []string{"message.created", "message.updated", "message.deleted", "history.pruned", "dm.keys_changed", "dm.devices_changed"}, "data": "Message object for created/updated/deleted; {pruned_through: sequence} for history.pruned. For dm.keys_changed the data is {revision}; for dm.devices_changed it is {user}. Refresh the DM encryption endpoints without incrementing unread counts. Monotonic global event IDs can skip inaccessible events. A gap requires HTTP history refresh."},
			"account_feed":   "Use events.watch when events.account_feed is advertised. Persist events.checkpoint only after applying preceding events. Initial/reconnect read positions come from paginated GET /api/v1/read-state. read.state contains changed readState objects; compare event_cursor and read_seq to reject stale snapshots. directory.changed invalidates directory/permission/emoji metadata, coalesce refreshes; read.sync requests a fresh read-state snapshot. These state hints are ephemeral and reconstructible, while message events are replayable. Advance read positions only through the highest message actually displayed in a focused conversation. Replays restore counts silently, not old notification sounds.",
			"reconnect":      "On sync.required in account-feed mode refresh the directory and read-state snapshot, load the visible conversation history, then events.watch from the supplied event_cursor. Legacy explicit subscriptions refresh their HTTP histories and resubscribe from the earliest returned event_cursor. On media.reconnect_required obtain a new admission, leave/rejoin, republish and recreate subscriptions before expires_at; credentials are separate from session validity. Serialise signaling commands and process asynchronous events while awaiting replies.",
		},
		"media": map[string]any{
			"path": "/api/v1/media", "enabled_when": "Admission transport is websocket", "protocol": "chatxo-encoded-v1",
			"authenticate_schema": schemas["mediaAuthenticate"], "command_schema": schemas["mediaCommand"], "track_schema": schemas["encodedTrack"],
			"client_messages": map[string]any{

				"authenticate": map[string]any{"required": []string{"type", "token", "participant_id"}, "reply": "ready", "description": "participant_id must belong to this device session's joined event socket. One media socket per participant."},
				"configure":    map[string]any{"required": []string{"type", "tracks"}, "reply": "tracks", "description": "Replace up to five streams: microphone audio, camera video, screen video, camera audio and screen audio. Empty tracks stops publication. Server assigns a new revision; use it for all chunks. description is base64 decoder configuration. Codec is an ASCII WebCodecs identifier, not a fixed whitelist."},
				"keyframe":     map[string]any{"required": []string{"type", "publisher_id", "purpose"}, "description": "Ask a camera/screen publisher for a keyframe. Server coalesces requests to at most one per second per source track."},
			},
			"server_messages": map[string]any{"ready": []string{"type", "protocol"}, "tracks": []string{"type", "participant_id", "tracks"}, "keyframe": []string{"type", "purpose"}},
			"binary": map[string]any{
				"byte_order": "big-endian", "input_header_bytes": 24, "output_header_bytes": 40,
				"output_prefix": "16 raw UUID bytes identifying the authenticated publisher; followed by the unchanged input header and encoded chunk",
				"fields": []map[string]any{
					{"offset": 0, "bytes": 1, "name": "version", "value": 1}, {"offset": 1, "bytes": 1, "name": "purpose", "values": map[string]int{"audio": 0, "camera": 1, "screen": 2, "camera_audio": 3, "screen_audio": 4}},
					{"offset": 2, "bytes": 1, "name": "flags", "values": map[string]int{"delta": 0, "key": 1}}, {"offset": 3, "bytes": 1, "name": "reserved", "value": 0},
					{"offset": 4, "bytes": 4, "name": "revision", "description": "Server-issued track revision"}, {"offset": 8, "bytes": 8, "name": "timestamp_us", "maximum": 9007199254740991},
					{"offset": 16, "bytes": 4, "name": "duration_us", "maximum": 10000000}, {"offset": 20, "bytes": 4, "name": "sequence", "description": "Per-track contiguous sequence; uint32 wrap allowed. First value is arbitrary."},
					{"offset": 24, "name": "payload", "description": "One nonempty encoded elementary chunk; no container, RTP or SDP."},
				},
				"max_input_bytes": 524288, "max_audio_input_bytes": 65536,
			},
			"limits":   map[string]any{"outbound_frames": 64, "outbound_bytes": 2097152, "inbound_bytes_per_second": 8388608, "inbound_frames_per_second": 600, "configure_per_second": 1},
			"recovery": "Late receivers wait for a video keyframe. Obey keyframe requests. On media socket loss, reattach to the still-active participant, configure again and use the new revision. When the event socket is lost, obtain a new admission and participant first. Queue overflow closes a slow receiver. Clients need bitrate/backpressure control, audio scheduling and timestamp synchronization; the server never decodes media.",
		},
	}
}
