# Server audio plugins

Noise suppression implementations are optional **server-provided Wasm packages**.
The browser and desktop clients contain the plugin host and a gain-only test fixture.
Server operators supply their own compatible packages.

An owner or a role with ManageConfiguration installs/removes packages in
**Server settings → Audio plugins**, without restarting. Installation requires an
authenticated raw ZIP upload to `POST /api/v1/admin/audio-plugins`; clients get the
catalog from `GET /api/v1/audio-plugins`. Downloads remain authenticated. Installation
and removal are audited. Server backups include installed packages.

A package is at most **50 MiB compressed and uncompressed**, including its manifest
and all models. Up to four packages per server are allowed. It contains only
`manifest.json` (at most 64 KiB) and explicitly listed `.wasm` modules. Traversal,
symlinks, duplicate entries, extra files and unknown manifest properties are rejected.
ZIP data is streamed and validated before atomic installation. The server never
executes plugin code.

Clients prefetch packages on connection, check SHA-256 and resource declarations,
and store bytes in IndexedDB (on disk in Electron's persistent profile). A 256 MiB
LRU budget bounds the total cache and shares identical model files across servers.
Cached files are verified on use; changed, missing or corrupt files are downloaded
again. The catalog is refreshed on reconnect/settings entry. Processing does not
make network requests. Plugin settings are saved per installation ID and per plugin
on the client, remain after logout, and are included in client configuration backups;
model bytes are redownloadable and aren't included in session backup exports.

Devices retains the global Noise suppression checkbox. User settings → Server
audio plugins contains the server’s plugin selector, declared controls, reset
defaults and microphone preview. CSD/VAD diagnostic scores appear only there,
not on Devices. Calls use their own server's settings even while another
server is selected. With no plugin, capture remains unprocessed.

## Download and join behavior

The Server audio plugins page shows per-model byte progress, cache checks,
verification, readiness and failures. Progress subscriptions are scoped to one
instance and do not broadcast every chunk through the global client state.
The selected model is prefetched first; other models cache in the background.

If the selected model is not ready when joining voice, a modal offers **Join**
(disabled until verified), **Join now**, Cancel and retry on failure. Join now
publishes unfiltered microphone audio immediately, then activates the selected
plugin on the same output track. It does not change the saved suppression setting.
A visible call notice reports loading, activation or failure and offers retry.
Cancel never joins automatically; leaving prevents a late activation/rejoin.
Disabling suppression while a download is pending remains an explicit opt-out.

## Security boundary

Downloaded JavaScript, HTML, native executables, dynamic imports and arbitrary UI
components are not supported. Only trusted client code constructs the worker,
audio graph and settings controls. A Wasm module receives mono PCM, scalar settings
and its own linear memory. It gets no tokens, DOM, filesystem, network, GPU, JS object
or host-memory imports. A small allowlist of inert WASI compatibility stubs supports
CPU-only libraries; all other imports are rejected before instantiation.

Before compilation, the client checks one bounded, nonshared Wasm32 memory of at
most 64 MiB, at most one bounded function table of 8192 entries, numeric function
types, and no start function or Wasm GC/reference types. Compilation and execution
happen in a dedicated worker, never on the UI/audio thread. Initialization is bounded
to five seconds; a missing worker heartbeat for 1.5 seconds terminates it. Each graph
has at most two outstanding PCM frames and two queued output frames. Output is checked
for finite samples and clamped. Failure returns to raw microphone capture; echo
cancellation remains disabled. No hidden replacement denoiser is enabled.

The package-size cap is not a CPU-cost guarantee. A slow or malicious module can
consume a worker's CPU until its watchdog fires; the host bounds recovery but cannot
promise to eliminate browser/OS engine vulnerabilities. This uses WebAssembly's
[host-import sandbox](https://webassembly.org/docs/security/) and the browser's
[worker termination API](https://developer.mozilla.org/en-US/docs/Web/API/Worker/terminate).

## Version 1 ABI and settings

See `client/tests/fixtures/audio-plugin/manifest.json` and `passthrough.c` for a
redistributable gain-only example. It proves the interface and is not a denoiser.
`client/packages/browser/src/audio-plugins/contract.ts` defines the complete client
contract; server types live in `internal/audioplugin/store.go`.

The manifest declares a stable ID, label, description, variant files, default variant,
ABI export names and numeric range/number controls. Controls define key, label, bounds,
step, default, scaling, optional setter export, and optional variant visibility.
There are no scripts, computed expressions, HTML or external URLs in the schema.

All graphs input/output 48,000 Hz mono float32. A module exports its own `memory`,
a creation function returning 1, input/output buffer pointers, and a processing
function returning 0 on success. Optional initialization/reset functions are allowed.
The fixed frame length is 120–960 samples. Processing receives declared scalar controls
in order; setter controls are called only when their value changes. DSP implementations
own any internal resampling and state. Browser Web Audio converts the actual device
rate to 48 kHz before the boundary, and handles output-device conversion afterward.

The trusted audio worklet exchanges frames directly with the worker through a
MessagePort, avoiding per-frame main-thread relays. No SharedArrayBuffer or cross-origin
isolation requirement is introduced. A plugin needing a different model interface
must provide a compatible Wasm adapter; arbitrary ONNX files alone are not runnable
plugins. New control kinds or non-CPU engines require a future host ABI version.

Diagnostics may declare up to eight numeric exports with plain-text labels and
variant visibility; they are read only for the settings monitor, at a bounded
rate. The legacy metadata field is accepted for older manifests but no longer imports
global model settings; all options come from the selected plugin schema.
