![Haunt](assets/haunt-wordmark.png)

# Haunt Server

This repository contains the Go server, database migrations, container runtime,
deployment examples, protocol documentation, and server verification workflows.
Downloads, published container images, and central installation documentation are
in [example-git/haunt](https://github.com/example-git/haunt).

The server provides independently hosted accounts, channels, direct messages,
voice/video calls, permissions, moderation, file storage, and client update
distribution. It is under active development and has not received an independent
security audit.

![Haunt community chat with a voice call](assets/09-voice-pip.png)

## Independent servers, one client

Each Haunt server owns its accounts, community settings, roles and data. A user can
connect to several servers in the same client, with lightweight background event
connections and local preferences that follow the device. Accounts are identified
as `username@server-address`.

Decentralized here means independently operated services. It does **not** mean that
servers federate identities, automatically exchange messages or share databases.
Direct messages are scoped to the service where their participants have accounts.

## What the service provides

- Invitation-based accounts, saved device sessions, password resets and account management.
- Text and voice channels, categories, roles and per-channel permissions.
- Real-time events, reconnect replay, presence, unread counts and mention notifications.
- Direct messaging with client-side MLS encryption, including encrypted image uploads;
  older plaintext messages remain visibly distinguishable.
- Relayed WebRTC voice, camera and screen sharing, plus configurable media transports.
- Moderation, account and network bans, appeals, and administration audit records.
- Configurable attachment quotas, retention, storage providers and declarative settings menus.
- Bundled RNNoise noise suppression, with replaceable server audio plugins processed locally by clients through a restricted WebAssembly interface.
- Generated HTTP and event protocol documentation for independent clients.

The server is written in Go and uses PostgreSQL. Linux amd64 and arm64 are the
release targets. Media forwarding does not require a server-side desktop or GPU.

## The web client is included

The [Haunt Client](https://github.com/example-git/haunt-client) repository is pinned
as the lowercase `client/` Git submodule. Release builds embed its compiled web assets
inside the server binary. The same HTTP listener serves the interface and API; a
separate nginx static-site configuration is unnecessary.

You can still host the web client separately or use the desktop app. HTTPS is
required for browser microphone/camera access outside localhost. A reverse proxy or
other TLS termination can provide that without managing a separate client directory.

## Build

Requires Go 1.26+, Node.js 22.12+ and npm. Both public repositories are fetched
at their pinned revisions by the recursive clone.

```sh
git clone --recurse-submodules https://github.com/example-git/haunt-server.git
cd haunt-server
./scripts/build-server.sh
```

This installs the client's locked dependencies, builds the web interface and produces
`bin/haunt-server` with the client embedded. For a chosen Linux architecture:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags webclient -trimpath -o bin/haunt-server-arm64 ./cmd/haunt
```

Run the client build first. Plain `go build ./cmd/haunt` remains available for an
API-only development binary without embedded assets.

## Run

For a complete installation in one container, use the root `Dockerfile` and
`compose.yml`. They include Nginx, a separate static web client, private
PostgreSQL and the Haunt server, with automatic HTTPS and a persistent data
volume. Follow the [container quick start](docs/container.md). Linux amd64 and
arm64 are supported; the standalone binary workflow below is also available.

Use [.env.example](.env.example) as the configuration reference. Supply a dedicated
PostgreSQL database and a persistent data directory, then start the binary. On a
fresh installation, open the private setup link printed at startup. The wizard runs
on the same listener as the client/API, guides you through the server address and
policies, and lets you choose the owner username and password. Settings are saved
privately in the data directory; the password is stored only as a database hash.
Existing owners skip setup. See [first-time setup](docs/first-run.md).

For a fully terminal-based setup, run `python3 scripts/install.py`. It explains
every container and bare-metal setting, creates the first owner from a private
password file, and asks whether to preload the bundled RNNoise plugin. This
explicit route skips browser setup; starting without an installer profile keeps
the browser wizard behavior.

- [Client updates and installer downloads](docs/client-updates.md)
- [Operations and backups](docs/operations.md)
- [Media transports and network deployment](docs/media-transports.md)
- [Runtime settings and storage](docs/runtime-settings.md)
- [Moderation](docs/moderation.md)
- [DM security](docs/dm-security.md)
- [Audio plugin interface](docs/audio-plugins.md)
- [Build and distribute an audio plugin](docs/audio-plugin-authoring.md)
- [Server extensions](docs/server-extensions.md)
- [API endpoints](docs/endpoints.md)

## Security boundaries

HTTPS protects client/server traffic. Encrypted DMs are intended to be readable by
the participating clients; server-provided identities and client key recovery remain
part of the trust model. Server channels are readable by the server operator.
WebRTC transport encryption is not end-to-end encryption against the media relay.
The server still sees connection metadata and the IP addresses needed to run the service.

Backups, encryption keys, updates and server administration need the same care as any
self-hosted service. Encrypted backups of the client and backups of the server solve
different recovery problems; keep both when the data matters.

## Development and releases

The server records a specific client commit, making an embedded release reproducible.
Update that submodule deliberately and commit the changed reference. The GitHub
server workflow builds binaries with that exact client's web interface.

API specifications are generated from the server and checked into the client for
standalone builds. See [client development](docs/client-development.md).

Haunt is an independent project, not affiliated with Discord. Names of other products
are used only to explain familiar behavior and interoperability.

## License and participation

Haunt-owned code in this repository is available under the
[BSD 3-Clause License](LICENSE). Bundled third-party material retains its own
licenses and notices. See [CONTRIBUTING.md](CONTRIBUTING.md) before submitting
changes and [SECURITY.md](SECURITY.md) for private vulnerability reports.
