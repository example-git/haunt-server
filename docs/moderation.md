# Server moderation

Moderation belongs to each installation. Roles, accounts, bans, IP history and
appeals are never shared with another server. The web and desktop clients expose
these controls in **Server settings**, and **Manage member** in user context menus.

## Permissions and hierarchy

Administrator (bit 32768) grants every server permission, including Server extensions,
and bypasses category/channel permission overrides. It retains the member's role
rank: higher/equal roles and the owner remain protected from moderation.
Administrators can edit lower-ranked roles, including the default role; its permissions are not automatically
changed by migration. Existing sessions gain or lose Administrator immediately.

The owner retains all permissions and cannot be kicked, banned, disabled or deleted
through the API. Roles have a numeric rank (0–1000); the implicit everyone role is
rank 0. A member's highest assigned rank governs moderation. Delegates can only
manage lower-ranked accounts and roles, cannot change themselves, and cannot grant
permissions they do not possess. Role changes apply to existing sessions.

| Bit | Permission |
| --- | --- |
| 1 | View channels |
| 2 | Send messages |
| 4 | Join voice |
| 8 | Create invitations |
| 16 | Manage channels/categories |
| 32 | Manage server appearance and emojis |
| 64 | Manage lower-ranked roles and assignments |
| 128 | Kick lower-ranked members |
| 256 | Ban/restore lower-ranked accounts, lift bans, review appeals |
| 512 | Delete lower-ranked accounts |
| 1024 | Read the audit log |
| 2048 | Read member IP history; combined with 256, manage IP bans |
| 4096 | Delete others' messages in accessible server channels |
| 8192 | Reset lower-ranked members' passwords |
| 16384 | Manage configuration, storage, extensions and diagnostics |
| 32768 | Administrator: all permissions and channel override bypass |
| 65536 | Mute lower-ranked voice members |
| 131072 | Deafen lower-ranked voice members |

Message moderation does not grant access to private DMs or the ability to rewrite
someone else's message. Host recovery remains host-only; password resets require
ResetPasswords, and diagnostic operations require ManageConfiguration. Existing
roles migrate to rank 1. The voice-moderation migration grants Mute members and
Deafen members once to roles that already have Kick or Ban; later edits control
the two permissions independently. Owners and administrators receive both.
The generated API documents describe the corresponding endpoints and wire types.

## Voice restrictions

Participant context menus provide moderator mute/deafen. The channel's **Voice
restrictions** list also clears restrictions while a member is absent. The
authenticated GET and PATCH routes under `/api/v1/voice/{id}/restrictions`
check current permissions, channel access and role hierarchy, and record updates
in the audit log. Lists include member identities and return up to 100 entries;
pass the last user ID as `after` for the next page. Existing restrictions can be
cleared for deleted accounts, but new restrictions cannot be added to them.
Restrictions are stored per instance, channel and user and remain through
leaving, reconnecting, multiple sessions and server restarts.

Self-selected mute/deafen remains independent. Effective state and restriction
flags are broadcast to clients; clearing a restriction restores the member's
own choices. Deafen blocks incoming audio and outgoing microphone audio on both
transports, including queued delivery. Independent camera and screen audio is
preserved.

## Account actions

Every action requires a reason (1–512 characters) and a confirmation in the client.

- **Kick:** disconnect all device sessions and voice, revoke invitations created by
  the member, and remove assigned roles. Returning requires a new valid invitation
  and the account's existing password through registration; the identity and
  message history remain intact.
- **Ban:** disconnect all devices/voice and block the account until explicitly
  unbanned. A correct password reveals an opaque appeal ticket; incorrect passwords
  still receive the generic authentication failure. Lifting a ban does not restore
  old sessions, nor does it implicitly undo a separate kick/disable.
- **Restore access:** clear a kick/disable. Active bans must still be lifted.
- **Delete:** permanently scrub the username, password hash, profile/avatar, IP
  history, roles, sessions, invitations, read positions and DM membership. A
  tombstone identity remains to preserve referential integrity of messages and
  audit records. Retained messages show **Deleted user** and follow the normal
  message/attachment retention policy. This is account deletion, not a request to
  erase every historical message or moderation record.

New moderation actions and role/message moderation audit entries commit in the
same database transaction. Serializable transactions protect against concurrent
role changes; a serialization conflict returns a retryable busy response. There is
no global database worker or application-wide moderation lock.

## Temporary password recovery

The **Reset member passwords** permission (8192) lets an administrator reset
lower-ranked members in Server settings → Members. Owner and equal/higher-ranked
accounts remain protected. The server generates a random temporary password and
shows it once to the administrator; share it privately. Resetting revokes all
existing sessions and records an audit event without the password.

A temporary-password login returns `password_change_required: true`. Its bearer
session expires after 15 minutes even if remember-device was requested. It can only
change the password or log out: ordinary API calls, event gateways and media reject
it. Clients must keep this token in memory rather than their saved-session vault.
The new password must be different and at least eight characters. Changing it
revokes all temporary sessions; the client signs in with the new password.
The legacy supplied-password reset endpoint also forces replacement. Host-operated
owner recovery remains available through the CLI. Resets never lift bans or restore
a disabled/kicked account.

Deleted accounts no longer appear in the member directories. DMs with no other
remaining member disappear from the sidebar, unread state and gateway subscriptions.
Messages in surviving channels/group DMs retain anonymous attribution. Public
profile lookup returns not-found for deleted accounts; only a scrubbed database
tombstone remains to preserve message references.

## IP history and bans

Only authenticated account activity records addresses. The server takes the
address from the socket, or a single validated address from an explicitly trusted
proxy. Untrusted forwarding headers and address chains are ignored. Nginx must
replace `X-Forwarded-For`, not append arbitrary incoming headers. Cloudflared mode
uses `CF-Connecting-IP` only when the socket peer is trusted.

Addresses are canonicalized, including IPv4-mapped IPv6. History retains the latest
32 addresses per account for up to 90 days. Login records immediately; subsequent
HTTP activity is sampled at most once per account/address every five minutes.
No IP history is included in public profiles, directory events or voice presence.

Bans accept a single IPv4/IPv6 address or CIDR range; host bits are normalized.
They gate API access, registration, login, existing event gateways and media.
Matching recorded sessions are revoked. A ban cannot include the operator's
current IP, a known owner IP, or (for a delegate) known equal/higher-ranked member
IPs. These checks cover recorded addresses, not addresses the server has never
seen. A shared public IP or range can affect multiple people. Account bans alone
cannot recognize a new identity registered with another valid invitation.

At most 1024 active network bans are accepted. Private IP values in ban and audit
responses require the separate IP-view permission. IP history begins when the
server observes authenticated account activity.

## Appeals and audit history

A ban response gives its affected client a random 256-bit ticket with no general
account access. Tickets are submitted in JSON bodies, not URLs. The appeal status
endpoint returns only that ban's type, reason, active state and next eligible date.
An IP-banned client can still use these two endpoints.

Appeals accept 10–1000 characters of plain text, reject control/format characters,
URLs and HTML brackets, and do not accept files. A conditional database update
allows exactly one submission per ban in a rolling 30-day period, including
concurrent requests from different addresses. A shared IP-range ban has one shared
appeal allowance. Additional per-source limits allow 10 status checks/hour and
3 submission attempts/day. Reviewing an appeal does not automatically lift a ban.
Appeal bodies expire after 180 days; the ban retains its last-submission timestamp.

Audit records contain time, action, actor/target, reason, and the server-observed IP
when available. Passwords, bearer tokens, appeal tickets and request bodies are
never audit fields. Settings routes log successful actions after commit;
moderation, role and message actions log atomically. Denied access is sampled once
per source/action/minute, with a global budget of 60 writes/minute. Authenticated
permission failures retain the actor ID. This is a bounded application audit trail,
not a packet log; requests rejected by nginx are visible in nginx's logs instead.

Minute-based maintenance prunes audit entries older than 90 days and keeps a target
of the latest 10,000 records, in batches of 2000. Recent writes can temporarily
exceed the target between maintenance runs. IP and appeal cleanup is also periodic.
All list APIs are paginated. Clients fetch audit, ban and appeal pages on demand;
no per-client moderation polling worker is added.
