# Server extensions

Server extensions are trusted server-hosted integrations, separate from the
Wasm microphone processors distributed through **Audio plugins**. The initial
extension is a lightweight direct TikTok speech relay. Neither Cloudflare nor
the Haunt desktop shell is needed: browser and desktop clients use the same API.
The first provider is bundled and disabled until the owner configures it; this
release does not execute arbitrary uploaded extension code.

## Owner setup and live test

Open **Server settings → Server extensions**. Only the owner can view, change,
clear or test provider configuration. Delegated ManageConfiguration permission
does not expose this page or its privileged endpoints.

1. Enter a TikTok `sessionid` from an account you control. It must be a 32-character
   hexadecimal token. This is the provider's session credential, not the old
   Cloudflare Worker's optional relay API key.
2. Choose the primary US East 5 API host or its alternate.
3. Save configuration. Leave the extension disabled while setting it up.
4. Choose a voice and test text, then **Test and play speech**. This makes a real
   provider request, bypasses the speech cache and never uses device fallback.
5. Enable the extension and save when ready to offer it to signed-in clients.

A blank credential field preserves the saved token; its value is never returned
by configuration or catalog APIs. **Clear credentials** disables the extension
and removes its token. Conflicting configuration revisions are rejected rather
than overwriting another edit. Replace tokens when TikTok expires/rejects them.

Credentials are encrypted in `DATA_DIR/server-extensions/configuration.encrypted`
using the installation key in the same private directory. Back up both together
and restrict access to the data directory: encryption does not protect against
someone who can read both files. Configuration changes are audited without
credentials or synthesized text. Credentials are not sent to clients or included
in client preference backups/sync.

## Client selection

The personal **Text to speech** section is in **Client settings**, immediately
below voice plugins. It has no owner credentials or provider setup controls.
The provider dropdown offers **Device TTS** and this server's configured,
enabled TTS extensions. Personal provider choice, voices, event filters and
user overrides apply only to the selected server/account.

Device fallback is an independent preference. With an extension selected and
fallback off, errors stay silent and report a failure; they do not invoke device
speech or choose another server. Owner tests are also always extension-only.
Disabling/unconfiguring a provider does not change a user's saved preferences.

## Relay boundary and limits

Clients authenticate to Haunt normally. Requests may contain text, voice ID and
the current voice-room ID, never provider credentials or arbitrary destinations.
When a room ID is supplied, the server verifies channel access and membership.
Preview requests outside calls are available to authenticated users.

TikTok synthesis uses the reference mobile app request headers, session cookie,
URL-encoded speech fields and `/media/api/text/speech/invoke/`. Only these exact
HTTPS origins are permitted:

- `https://api16-normal-useast5.us.tiktokv.com`
- `https://api16-normal-useast5.tiktokv.us`

Redirects are refused, upstream errors are sanitized, requests time out, prompts
are limited to 500 UTF-8 bytes and chunks to 300 bytes. The host allows two
simultaneous generations, 30 requests per account per minute, and six owner tests
per minute. Upstream failures trigger a short backoff; cancelled client requests
do not lock out other listeners. Generated clips use a bounded memory-only cache
(64 entries, 16 MiB, ten-minute expiry), invalidated on configuration changes.
Changing settings during generation discards output from the old configuration.

This is the reference's undocumented TikTok endpoint. Successful local tests do
not guarantee upstream availability in every region or for every voice.

## API

- `GET /api/v1/server-extensions`: authenticated metadata without credentials.
- `GET /api/v1/admin/server-extensions/{id}`: owner configuration/status.
- `PUT /api/v1/admin/server-extensions/{id}`: owner save, rotate or clear token.
- `POST /api/v1/admin/server-extensions/{id}/test`: owner live speech test.
- `POST /api/v1/server-extensions/{id}/speech`: authenticated MP3 generation.

The initial extension ID is `tiktok-tts`. Speech errors are JSON, successful
responses are `audio/mpeg`. See the generated OpenAPI contract for request fields.

## Standalone provider project

The [standalone provider repository](https://github.com/example-git/tiktok-tts-plugin)
contains a standard-library Go module, metadata, documentation and transport tests.
The hub pins that source under `extensions/tiktok-tts-plugin/`.

This release compiles its bundled adapter in `internal/serverextension` into the
server binary. Updating the server deploys that adapter and its API. The standalone
source module is not a separately loadable runtime package or an uploadable plugin
ZIP. Existing provider credentials and settings stay in the installation's private
`server-extensions` directory across server updates.
