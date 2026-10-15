# Generated HTTP endpoints

Generated from actual route registration and shared Go wire types using swaggest/openapi-go. Regenerate with `go generate ./internal/httpapi`.

Interactive reference: `/docs/`. Specifications: `/openapi.json` and `/websocket.json`. See [API behavior](api.md) and [media protocol](media-transports.md).

| Method | Path | Operation | Authentication | Success |
| --- | --- | --- | --- | --- |
| DELETE | `/api/v1/admin/audio-plugins/{id}` | Remove an audio plugin (ManageConfiguration) | Bearer | 204 |
| DELETE | `/api/v1/admin/storage/{id}` | Remove an inactive provider with no retained files (ManageConfiguration) | Bearer | 204 |
| DELETE | `/api/v1/attachments/{id}` | Delete an owned attachment | Bearer | 204 |
| DELETE | `/api/v1/categories/{id}` | Delete a Category | Bearer | 204 |
| DELETE | `/api/v1/channels/{id}` | Delete a Channel | Bearer | 204 |
| DELETE | `/api/v1/conversations/{id}/messages/{message}` | Tombstone an authored message | Bearer | 200 |
| DELETE | `/api/v1/emojis/{id}` | Delete a custom emoji (ManageServer) | Bearer | 204 |
| DELETE | `/api/v1/instance/banner` | Remove banner; icons/banners require ManageServer | Bearer | 204 |
| DELETE | `/api/v1/instance/icon` | Remove icon; icons/banners require ManageServer | Bearer | 204 |
| DELETE | `/api/v1/invites/{id}` | Revoke an invitation | Bearer | 204 |
| DELETE | `/api/v1/me/avatar` | Remove avatar; icons/banners require ManageServer | Bearer | 204 |
| DELETE | `/api/v1/roles/{id}` | Delete a nondefault role (ManageRoles; lower rank only) | Bearer | 204 |
| DELETE | `/api/v1/sessions/{id}` | Revoke this account's device session | Bearer | 204 |
| GET | `/api/v1/admin/appeals` | List ban appeals (BanMembers) | Bearer | 200 |
| GET | `/api/v1/admin/audit` | List recent moderation and sampled access events (ViewAuditLog) | Bearer | 200 |
| GET | `/api/v1/admin/bans` | List active bans (BanMembers) | Bearer | 200 |
| GET | `/api/v1/admin/members/{id}/ips` | Read private member IP history (ViewMemberIPs) | Bearer | 200 |
| GET | `/api/v1/admin/stats` | Inspect storage, pool, queue and media metrics (requires the corresponding server permission) | Bearer | 200 |
| GET | `/api/v1/admin/storage` | List attachment storage providers; credentials are never returned (ManageConfiguration) | Bearer | 200 |
| GET | `/api/v1/attachments/{id}` | Download an authorized attachment; supports Range and HEAD | Bearer | 200 |
| GET | `/api/v1/attachments/{id}/metadata` | Get filename, size and detected content type for an authorized attachment | Bearer | 200 |
| GET | `/api/v1/audio-plugins` | List local audio plugins and integrity hashes | Bearer | 200 |
| GET | `/api/v1/audio-plugins/{digest}/files/{name}` | Download an authenticated Wasm asset | Bearer | 200 |
| GET | `/api/v1/categories` | List visible categories | Bearer | 200 |
| GET | `/api/v1/categories/{id}/permissions` | Get permission overrides | Bearer | 200 |
| GET | `/api/v1/channels` | List visible channels | Bearer | 200 |
| GET | `/api/v1/channels/{id}` | Get a channel and effective permissions | Bearer | 200 |
| GET | `/api/v1/channels/{id}/menu` | Permission-filtered declarative channel settings and context menu | Bearer | 200 |
| GET | `/api/v1/channels/{id}/permissions` | Get permission overrides | Bearer | 200 |
| GET | `/api/v1/client-updates/haunt-updates.json` | Get the signed desktop update manifest (local distribution enabled) | Bearer | 200 |
| GET | `/api/v1/client-updates/installers` | List locally available desktop installers when enabled | Bearer | 200 |
| GET | `/api/v1/client-updates/{version}/{name}` | Download an authenticated desktop release artifact | Bearer | 200 |
| GET | `/api/v1/conversations` | List accessible text/voice channel chats and DMs | Bearer | 200 |
| GET | `/api/v1/conversations/{id}/encryption` | Get participant device credentials and unused key packages | Bearer | 200 |
| GET | `/api/v1/conversations/{id}/encryption/events` | Read ordered MLS group commits and welcome messages | Bearer | 200 |
| GET | `/api/v1/conversations/{id}/messages` | Read message history and recovery cursors | Bearer | 200 |
| GET | `/api/v1/conversations/{id}/messages/{message}/embeds` | Fetch up to three cached public link previews for a visible retained message | Bearer | 200 |
| GET | `/api/v1/conversations/{id}/messages/{message}/embeds/image` | Download a bounded preview image after rechecking message access | Bearer | 200 |
| GET | `/api/v1/emojis` | List this server's custom emoji catalog | Bearer | 200 |
| GET | `/api/v1/emojis/{id}/image` | Download an authenticated emoji PNG or GIF | Bearer | 200 |
| GET | `/api/v1/events` | Upgrade to authenticated events and signaling WebSocket | First WebSocket frame | 101 |
| GET | `/api/v1/instance` | Discover installation capabilities and limits | Public | 200 |
| GET | `/api/v1/instance/banner` | Download banner PNG or GIF | Public | 200 |
| GET | `/api/v1/instance/configuration` | Read live server configuration (ManageConfiguration) | Bearer | 200 |
| GET | `/api/v1/instance/icon` | Download icon PNG or GIF | Public | 200 |
| GET | `/api/v1/invites` | List visible invitations | Bearer | 200 |
| GET | `/api/v1/me` | Get the current principal | Bearer | 200 |
| GET | `/api/v1/me/dm-devices/{device}/packages` | List your device’s unused public MLS key packages | Bearer | 200 |
| GET | `/api/v1/me/dm-storage` | Get your DM storage and daily transfer usage | Bearer | 200 |
| GET | `/api/v1/media` | Upgrade to encoded media WebSocket when configured | First WebSocket frame | 101 |
| GET | `/api/v1/members` | List members and roles (any member-management permission) | Bearer | 200 |
| GET | `/api/v1/menus/server` | Permission-filtered declarative server settings and context menu | Bearer | 200 |
| GET | `/api/v1/read-state` | List persistent unread and mention counts for accessible conversations | Bearer | 200 |
| GET | `/api/v1/roles` | List community roles | Bearer | 200 |
| GET | `/api/v1/sessions` | List this account's device sessions | Bearer | 200 |
| GET | `/api/v1/users` | List public user profiles | Bearer | 200 |
| GET | `/api/v1/users/{id}` | Get one public user profile | Bearer | 200 |
| GET | `/api/v1/users/{id}/avatar` | Download avatar PNG or GIF | Bearer | 200 |
| GET | `/api/v1/voice/{id}` | List current room participants and tracks | Bearer | 200 |
| GET | `/api/v1/voice/{id}/ice` | Refresh authorized ICE credentials | Bearer | 200 |
| GET | `/healthz` | Check process liveness | Public | 200 |
| GET | `/readyz` | Check database and identity schema readiness | Public | 200 |
| POST | `/api/v1/admin/appeals/{id}/review` | Mark an appeal reviewed without lifting its ban | Bearer | 204 |
| POST | `/api/v1/admin/audio-plugins` | Install a ZIP of Wasm modules and a declarative manifest, at most 50 MiB packed and unpacked (ManageConfiguration) | Bearer | 201 |
| POST | `/api/v1/admin/audio-plugins/repository` | Install dist/haunt-plugin.zip from an HTTPS GitHub repository at a full commit ID, requiring its SHA-256 (ManageConfiguration) | Bearer | 201 |
| POST | `/api/v1/admin/bans` | Ban an IP address or CIDR range (BanMembers and ViewMemberIPs) | Bearer | 201 |
| POST | `/api/v1/admin/bans/{id}/revoke` | Lift a ban with an audit reason | Bearer | 204 |
| POST | `/api/v1/admin/maintenance` | Run bounded maintenance (requires the corresponding server permission) | Bearer | 204 |
| POST | `/api/v1/admin/members/{id}/moderate` | Kick, ban, restore or delete a lower-ranked member | Bearer | 204 |
| POST | `/api/v1/admin/members/{id}/password-reset` | Issue a temporary password and revoke sessions (Reset passwords permission; lower-ranked members only) | Bearer | 200 |
| POST | `/api/v1/admin/storage` | Verify write/read/delete access and register private attachment storage (ManageConfiguration) | Bearer | 201 |
| POST | `/api/v1/admin/storage/{id}/check` | Verify provider write/read/delete access (ManageConfiguration) | Bearer | 204 |
| POST | `/api/v1/auth/ban/appeal` | Submit one plain-text appeal per ban per rolling 30 days | Public | 204 |
| POST | `/api/v1/auth/ban/status` | Read a ban using its opaque appeal ticket | Public | 200 |
| POST | `/api/v1/auth/login` | Create a device session | Public | 200 |
| POST | `/api/v1/auth/logout` | Revoke the current device session | Bearer | 204 |
| POST | `/api/v1/auth/register` | Register using an invitation | Public | 201 |
| POST | `/api/v1/categories` | Create a category | Bearer | 201 |
| POST | `/api/v1/channels` | Create a text or voice channel | Bearer | 201 |
| POST | `/api/v1/conversations/{id}/attachments` | Upload raw image or video bytes | Bearer | 201 |
| POST | `/api/v1/conversations/{id}/encryption/events` | Compare-and-swap an MLS group revision and consume single-use key packages | Bearer | 200 |
| POST | `/api/v1/conversations/{id}/messages` | Commit a message with retry deduplication | Bearer | 201 |
| POST | `/api/v1/dms` | Get or create a DM for a participant set | Bearer | 201 |
| POST | `/api/v1/invites` | Create an invitation | Bearer | 201 |
| POST | `/api/v1/me/dm-devices` | Register an immutable MLS device credential | Bearer | 200 |
| POST | `/api/v1/me/dm-storage/purge` | Preview or purge your older DM uploads; confirmed requests remove up to 128 files | Bearer | 200 |
| POST | `/api/v1/roles` | Create a role (ManageRoles; lower rank only) | Bearer | 201 |
| POST | `/api/v1/sessions/restore` | Create an independent device session from a valid backup session without revoking it or extending its expiry | Bearer | 201 |
| POST | `/api/v1/voice/{id}/admissions` | Get a single-use room admission and automatic transport | Bearer | 201 |
| PUT | `/api/v1/admin/audio-plugins/default` | Save default plugin, model and controls for new clients (ManageConfiguration) | Bearer | 200 |
| PUT | `/api/v1/admin/members/{id}` | Disable or enable a member (owner) | Bearer | 204 |
| PUT | `/api/v1/admin/members/{id}/password` | Set a temporary member password, require replacement, and revoke sessions | Bearer | 204 |
| PUT | `/api/v1/admin/storage/active` | Select the provider for new uploads; existing files retain their provider (ManageConfiguration) | Bearer | 204 |
| PUT | `/api/v1/admin/storage/{id}/credentials` | Verify and replace S3 credentials (ManageConfiguration) | Bearer | 204 |
| PUT | `/api/v1/categories/{id}` | Update a category | Bearer | 200 |
| PUT | `/api/v1/categories/{id}/permissions/{role}` | Set a role's permission override | Bearer | 204 |
| PUT | `/api/v1/channels/{id}` | Update a channel | Bearer | 200 |
| PUT | `/api/v1/channels/{id}/permissions/{role}` | Set a role's permission override | Bearer | 204 |
| PUT | `/api/v1/conversations/{id}/messages/{message}` | Edit an authored message | Bearer | 200 |
| PUT | `/api/v1/conversations/{id}/read` | Advance this account's read position monotonically to the highest displayed message sequence | Bearer | 200 |
| PUT | `/api/v1/emojis/{name}` | Create or replace a custom emoji (ManageServer) | Bearer | 200 |
| PUT | `/api/v1/instance/banner` | Upload PNG/JPEG/GIF banner; icons/banners require ManageServer | Bearer | 200 |
| PUT | `/api/v1/instance/configuration` | Persist and apply live server policies; revision prevents lost edits (ManageConfiguration) | Bearer | 200 |
| PUT | `/api/v1/instance/icon` | Upload PNG/JPEG/GIF icon; icons/banners require ManageServer | Bearer | 200 |
| PUT | `/api/v1/instance/settings` | Update server name, description and accent color (ManageServer) | Bearer | 200 |
| PUT | `/api/v1/me/avatar` | Upload PNG/JPEG/GIF avatar; icons/banners require ManageServer | Bearer | 200 |
| PUT | `/api/v1/me/dm-devices/{device}/packages` | Publish single-use MLS key packages | Bearer | 204 |
| PUT | `/api/v1/me/password` | Change password and revoke existing sessions | Bearer | 204 |
| PUT | `/api/v1/me/profile` | Update your display name, bio and avatar color | Bearer | 200 |
| PUT | `/api/v1/me/session` | Remember or stop remembering the current device session | Bearer | 200 |
| PUT | `/api/v1/members/{id}/roles` | Replace a member's roles (ManageRoles; lower rank only) | Bearer | 204 |
| PUT | `/api/v1/roles/{id}` | Update a role (ManageRoles; lower rank only) | Bearer | 200 |
| HEAD | `/api/v1/attachments/{id}` | Attachment headers | Bearer | 200 |

GET routes also accept HEAD through Go's ServeMux. OPTIONS is handled by the origin/CORS boundary. Developer documentation is served at `/docs/`, `/openapi.json`, `/websocket.json`, and `/endpoints.md`.
