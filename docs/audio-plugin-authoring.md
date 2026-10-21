# Build and distribute a server audio plugin

A Haunt audio plugin is a ZIP containing a declarative manifest and CPU-only
WebAssembly modules. The server stores and distributes it; the client's trusted
worker processes microphone audio locally. Installing a plugin does not execute
its repository or require changing the server or client.

Start with the [client's working gain example and ABI tutorial](https://github.com/example-git/haunt-client/blob/main/docs/audio-plugin-authoring.md).
For a real denoiser, see [RNNoise's adaptation guide](https://github.com/example-git/haunt-audio-rnnoise/blob/main/docs/authoring.md).
The [host and package reference](audio-plugins.md) documents the security boundary
and download behavior.

## 1. Implement and test the adapter

Use a stable plugin ID, such as `my-denoiser`, and embed all inference code and
model weights in the Wasm file. Each variant has its own module. The interface is
48 kHz mono float32, with a fixed frame of 120–960 samples. Your adapter owns model
state, any internal resampling and correctly aligned dry/filtered blending.
An ONNX file alone is not a compatible package: its inference runtime and adapter
must also fit the sandbox and size limits.

Compile for Wasm32 with an exported, bounded memory, no start function and ideally
no imports. Do not add networking, JavaScript, native helpers or custom UI code.
The manifest declares numeric settings; trusted client code renders the controls.
Read the client tutorial for exact function signatures, scaling and lifecycle.

Test finite, bounded output, initialization, reset and each variant. Compare
saved original and filtered speech clips, including quiet/loud speech, silence,
keyboard/fan noise and competing speech. A synthetic smoke check verifies the
ABI; listening comparisons assess speech quality.

## 2. Package only the supported files

```text
manifest.json
standard.wasm
light.wasm       # only if declared as a variant
LICENSE.txt      # optional, include required distribution notices
```

There is no enclosing folder. A variant named `light` must use `light.wasm`.
Do not put README files, source, model downloads, checksum files or directories
inside the ZIP. Keep those beside it in the repository instead.

Both packed and total unpacked size are limited to **50 MiB**. Manifest and license
text are each limited to 64 KiB. There are at most 16 variants, 32 numeric controls
and eight diagnostic exports. A server allows at most four installed packages.
Each module must satisfy the client's 64 MiB memory and other resource bounds.
Unknown manifest fields and unsupported ZIP entries are rejected.

See the client tutorial for a runnable packaging example. Publish these files in
your repository, committing the ZIP intentionally:

```text
dist/haunt-plugin.zip
dist/SHA256SUMS
```

Compute SHA-256 from the final ZIP, after every packaging change. Keep a lock file
with compiler, upstream source and model hashes. License permission for your
adapter does not automatically cover someone else's model weights.

## 3. Install on an isolated server

An owner or role with **ManageConfiguration** opens **Server settings → Audio
plugins**. Upload the ZIP, or use **Install from GitHub** with:

- The repository's HTTPS GitHub URL.
- The complete 40-character commit containing `dist/haunt-plugin.zip`.
- The 64-character lowercase SHA-256 of that ZIP.

The repository installer reads one pinned file through GitHub's API. It never
clones the repository or executes scripts. Branches, tags and arbitrary URLs are
not accepted. Hash mismatch or validation failure preserves the installed plugin.
For private repositories, the server operator supplies a read-only token using
`HAUNT_AUDIO_PLUGIN_GITHUB_TOKEN_FILE`; never put that token in the plugin or client.

The API equivalent is `POST /api/v1/admin/audio-plugins/repository` with
`repository`, `commit` and `sha256` string fields. ZIP upload uses a raw
`application/zip` request body at `POST /api/v1/admin/audio-plugins`.
Both require a normal authenticated administrator session.

## 4. Verify the user experience

Enable **Noise suppression** in Devices, then choose the plugin in **User settings
→ Server audio plugins → Active Server Audio Plugin:**. Selection changes the
active microphone processor. The model selector chooses a variant within that
plugin. Changes apply during calls and
microphone preview; the selected model and tuning persist locally per server and plugin.

Owners and administrators with ManageConfiguration can click **Set as server
default** on this page to save their current plugin, model and tuning. New clients
use that choice; existing personal preferences are retained.

Exercise download progress, joining before download completion, retries, changing
plugins during a call, microphone monitoring and a cached reload. Confirm that
turning suppression off gives raw capture, and failures display a notice. Plugins
must not introduce echo cancellation or route playback back into outgoing capture.

## Releasing and updating

Commit a reviewed package, its exact checksum, source and license notices. Give
operators the revision and checksum together. Installing a changed ZIP with the
same plugin ID replaces that package; use stable variant/control keys so saved
settings remain meaningful. Changing the ID creates a separate plugin. Updates
are explicit, not automatic tracking of a moving Git branch.

RNNoise is the bundled default for fresh Haunt installations. The server's pinned
copy lives under `internal/audioplugin/default/`; update its ZIP, checksums, build
metadata, notices and recorded source revision together. Operator-installed
packages are stored in the server's data directory.
Existing operator plugins and defaults intentionally removed by an operator are
preserved across restarts and server upgrades.
