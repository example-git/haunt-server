# Server implementation status

The server-first milestones are implemented. Client work has not begun.

1. **Process/storage:** configuration, pooled PostgreSQL, migrations/upgrades, readiness/discovery, Linux amd64 and arm64 binaries, bounded shutdown.
2. **Identity/access:** owner bootstrap/recovery, invitation-only accounts, password changes, independent revocable sessions, account disabling, profiles, roles/categories/channels and live permission checks.
3. **Conversations/events:** text channels, group DMs, immutable acceptance ordering, bounded retry deduplication, edits/tombstones, history/replay cursors, byte/age retention and reconnect recovery.
4. **Attachments:** bounded raw uploads, stream/content probing, reservations, range downloads, one-message claims, expiry/deletion and restart reconciliation.
5. **Media:** voice/camera/screen SFU, negotiated codec preservation, independent rooms/queues, mute/deafen, republishing/reconnection, ICE/public-port configuration and optional host connectivity relay, external/Cloudflare TURN, and cloudflared-compatible encoded WebSocket media with automatic transport selection.
6. **Host operations:** owner statistics/maintenance, offline backup rotation and validated restore, generated OpenAPI/Swagger client reference, systemd/nginx/cloudflared examples, operational logging configuration, packages/checksums and deployment documentation.

Validation is tracked in [verification.md](verification.md), including native race tests, real PostgreSQL, decoded media and TURN transports, compiled-process recovery, and ARM64 emulation. Native arm64 hardware, actual Oracle network rules/public-IP routing, live Cloudflare account deployment, long-duration capacity testing, and browser/device coverage beyond the dedicated Chrome WebCodecs harness require those environments. The current results do not claim those checks have happened.

Before public deployment, apply and validate the documented host configuration. Later client work should follow the API's history-gap, replay, attachment-expiry, codec and ICE-renewal contracts. Transport/relay choices are automatic implementation behavior, not client-facing settings.
