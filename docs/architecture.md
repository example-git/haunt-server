# Haunt architecture

One server process hosts a community with its own accounts, permissions,
PostgreSQL database, and persistent data directory. Browser and desktop clients
can maintain independent authenticated connections to several servers.

## Identity and access

An installation has a stable identifier. Accounts, sessions, messages, and media
access are scoped to that installation. Server discovery advertises capabilities
and limits; configured origins control browser access. Credentials and session
tokens are confined to the server that issued them.

## Messaging and storage

HTTP handles authentication, snapshots, messages, and uploads. An authenticated
WebSocket carries events and reconnect checkpoints. Accepted messages commit
before acknowledgement and event delivery; clients retain local history and
merge replayed events by stable identifiers.

Channel history and encrypted DM history have separate storage and retention
policies. DMs use client-side MLS encryption; legacy plaintext remains identified
as such. See [DM security and recovery](dm-security.md) and
[runtime settings](runtime-settings.md).

PostgreSQL pooling supports concurrent requests. Transactions and locks protect
specific storage, account, and room state. Uploads and media forwarding occur
outside accounting transactions. See [concurrency and ownership](concurrency.md).

## Media

The server forwards audio, camera, and screen media between authorized room
members. WebRTC and encoded WebSocket transport share room membership and
permission checks. Transport selection follows installation configuration.
See [media transports](media-transports.md).

## Components and operation

The [server design](server-design.md) describes component responsibilities,
transactions, and storage lifecycles. [API documentation](api.md) covers the
client contract. Deployment, backup, and restore procedures are in
[operations](operations.md); test environments and commands are in
[testing](testing.md).
