# Server extensions

Server extensions are locally installed executable packages. Each package
provides a declarative manifest and implements the versioned `stdio-v1` protocol.
The server discovers packages, publishes their settings pages and voice catalogs,
and runs speech requests. Browser and desktop clients render the server-provided
menus and play the returned audio.

## Installation and administration

Install a package directory under
`HAUNT_DATA_DIR/server-extensions/packages/<extension-id>/`. It contains
`manifest.json` and the executable named by its `entrypoint`. Packages run as the
server's operating-system user. Installation is performed on the server's
filesystem, not by uploading executable code through the client.

Open **Server settings → Server extensions → Reload installed extensions** after
adding, replacing or removing a package. Startup also discovers packages. Reload
validates all manifests before replacing the active catalog. An invalid package
returns an error and leaves the previous catalog in place. Reload invalidates
cached speech and in-flight results from the previous package generation.

Each installed package contributes a named page to the server's declarative menu.
The page renders its configuration fields from the server response. The owner,
**Administrator**, and **Manage configuration** delegates can configure and test
extensions. Permission changes apply to existing sessions immediately.

1. Open the extension's page and enter its settings.
2. Save configuration; the extension can remain disabled during setup.
3. Choose test text and a voice, then **Test and play speech**.
4. Enable the extension and save to serve signed-in clients.

Password fields are write-only. Blank values preserve saved credentials;
**Clear credentials** clears the saved password fields and disables the extension.
Revision conflicts reject stale edits. Secrets are encrypted in
`HAUNT_DATA_DIR/server-extensions/configurations.encrypted`, using the private
`key` file alongside it. Back up both files together. Existing single-provider
configuration is migrated on startup. Removing a package leaves its saved
configuration available for reinstalling it.

## Client controls

**Account settings → Text to speech** lists the selected server's extensions.
Provider names, voice groups, labels and defaults come from installed package
metadata. The same voice catalog drives personal selection, user overrides and
administrator tests. Custom voice IDs are passed to the selected extension.

Personal choices are scoped to the server/account. Missing, disabled or
unconfigured extensions remain unavailable without changing saved preferences.
Device fallback is a separate preference; when off, an extension error does not
invoke local speech or switch servers. Administrator tests never use fallback.

## Package contract

A schema-1 manifest contains:

- `id`, `name`, `description`, `category: "tts"`, `runtime: "stdio-v1"`.
- `entrypoint`: `extension` or `extension.exe`, a regular executable in the package.
- `fields`: configuration fields with `key`, `label` and `type` (`text`, `password`
  or `select`); optional `help`, `default`, `required`, `max_length`, `pattern`
  and select `options` with `value`/`label` pairs. Password defaults are empty.
- `voices`: objects with `id`, `name` and optional `group`.
- `default_voice`: the voice used when a speech request leaves its voice empty.
- Optional `voice_pattern`: a regular expression for custom voice IDs, using syntax
  supported by both Go and JavaScript. Clients retain the last valid selection
  while an invalid custom ID is being edited.

The server launches the executable in its package directory for each uncached
request. Stdin contains one JSON object and is then closed:

```json
{
  "protocol": 1,
  "operation": "speech",
  "configuration": {"credential": "<saved value>"},
  "text": "Welcome",
  "voice": "example_voice"
}
```

Stdout contains one JSON result, followed by process exit:

```json
{"protocol":1,"audio":"<base64 MP3>"}
```

Errors use `{"protocol":1,"error":"provider_failed"}`. Recognized error codes
are `invalid_request`, `credentials_rejected`, `busy` and `provider_failed`.
Unknown errors, malformed results and failed processes become a generic provider
failure; process stderr and arbitrary upstream error text are not exposed.
Credentials never enter command arguments. The child receives PATH and configured
proxy/certificate variables rather than the server's database and account secrets.

The host caps prompts at 500 UTF-8 bytes, audio at 8 MiB and process duration at
20 seconds. It permits two concurrent generations, 30 speech requests per account
per minute and six administrator tests per minute. The shared memory cache holds
up to 64 entries or 16 MiB for ten minutes. Provider failures apply a short
per-extension backoff; client cancellation does not block other listeners.
Configuration changes invalidate cached results. Administrator tests bypass the
cache, enablement check and backoff, but still require complete configuration.

## API

- `GET /api/v1/server-extensions`: authenticated catalog with names and voices.
- `POST /api/v1/admin/server-extensions/reload`: discover installed packages.
- `GET /api/v1/admin/server-extensions/{id}`: fields, non-secret values and credential status.
- `PUT /api/v1/admin/server-extensions/{id}`: save `revision`, `enabled`, `values` and optional `clear_secrets`.
- `POST /api/v1/admin/server-extensions/{id}/test`: test saved configuration.
- `POST /api/v1/server-extensions/{id}/speech`: generate MP3 audio.
- `GET /api/v1/menus/server`: permission-filtered pages contributed by installed extensions.

Administration requires **Manage configuration** or **Administrator**. Speech
requires an authenticated session; requests with a room ID also require active
membership and channel access. Successful speech responses are `audio/mpeg`.

## Example extension

[TikTok TTS](https://github.com/example-git/tiktok-tts-plugin) is a standalone
standard-library Go implementation of this interface, with its own build script,
manifest, voice catalog and transport/protocol tests. Build and install its package
independently of the server. Its README describes provider-specific setup.
