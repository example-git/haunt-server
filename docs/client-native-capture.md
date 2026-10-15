# macOS native capture

Apple Silicon Electron builds use the owner's prebuilt SwiftCaptureWorker for
webcams/capture cards and screen/window sharing. The copied executable is in
`client/static/native/mac/SwiftCaptureWorker`; its original SHA-256 is
`42d09709d46fc3e2b23477b10eab2cc1741cb11631def819ec94c38b313ca1c7`.
It targets macOS 13 or newer. macOS packages target arm64 only; native capture is
the standard path on Apple Silicon. Browser clients and Linux/Windows Electron
retain Chromium capture. No transport choice is added to the UI.

The integration follows the watch client's Electron host contract: enumerate
native cameras, audio devices, displays and windows through SwiftCapture,
request macOS camera/microphone permission in Electron, spawn a worker per source,
and parse SCAP v1 configuration/sample records. Haunt uses private inherited descriptors:
FD 3 for H.264 Annex-B access units, FD 4 for PCM source audio, and stdin for the
worker's control channel. There is no localhost capture listener. Only workers
spawned by this app are stopped; no process search or orphan-worker cleanup runs.
Restarting a source waits for the app's preceding worker to exit before opening
its replacement. The camera permission button requests TCC access without opening
a competing Chromium camera stream. Busy but connected devices remain in the
native catalog; whether simultaneous capture is supported still depends on the
device/driver. The native screen picker uses labeled source cards, since the
worker's catalog does not provide thumbnails.

## Media path

- **WebRTC:** H.264 stays in Electron and passes through the bundled
  `HauntMediaWorker` Pion publisher to the service's existing media relay. Video
  is packetized into RTP without decoding or encoding it again. Signaling still
  uses the authenticated client gateway, and the publisher honors the admission's
  ICE servers and relay policy. Incoming calls use the existing browser receiver.
- **WebSocket relay:** the existing encoded transport publishes the worker's
  H.264 access units directly. It does not run a WebCodecs video encoder on them.
- **Local preview and WebSocket receive:** WebCodecs requests hardware decoding
  first and automatically falls back when that configuration is unavailable or
  fails. Decoded `VideoFrame`s go directly into a generated media track on
  Chromium/Electron, avoiding the old canvas copy and 30 fps timing gate. Browsers
  without that track API use a canvas with no artificial frame-rate limit. All
  paths feed the existing tile/PiP/popout streams. Actual presentation rate is
  bounded by the source, display and device performance.
- **Backpressure:** preview video and PCM have separate IPC credits (16 video
  packets, 24 audio packets, at most 8 MiB combined per source). Decode queues
  accept brief scheduling delays before dropping a GOP; presentation keeps only
  one pending decoded frame while writing. A slow preview cannot accumulate
  unbounded memory or block the native WebRTC publisher.
- **Audio:** the worker's separate-FD mode emits PCM. Haunt preserves stereo for
  camera and screen audio, validates its actual format, resamples to 48 kHz with
  a bounded band-limited converter when necessary, and encodes Opus at
  the existing call bitrate. The worker also supports H.264/AAC muxed output, but
  those MPEG-TS/SRT/RTMP modes are not used in calls. Microphone noise suppression,
  gain, mute, output selection, and per-source playback volume retain their
  existing paths. Camera audio and screen audio are separate from the microphone.

Camera capture requests 1080p at 60 fps; SwiftCapture clamps to the device's frame
rate capability. Screen/window capture fits within 1920×1080 at 60 fps. The
worker emits periodic keyframes (one second for screen, approximately two for
camera); its control protocol has no force-keyframe command. New viewers or a
dropped GOP may therefore wait until the next keyframe.

Device settings list native camera and capture-audio IDs on supported Macs. Saved
Chromium device preferences migrate when the device label uniquely matches the
native catalog; otherwise select the device again. Choosing no camera audio does
not capture the microphone as camera audio. Screen audio requires checking the
audio option in the screen/window picker.

## Packaging and rebuilding

Both prebuilt helpers and their manifest are under `client/static/native/mac/`
so they sync with the repository's other input assets. Normal Electron staging
checks their hashes and the publisher source hash; it does not require Go or
Swift on the packaging host. The macOS builder copies them outside ASAR into
`Contents/Resources/native/mac/` and includes both in its native signing list.
Inherited entitlements include camera and audio input access.

After changing `client/native/media-worker/main.go` or its Go dependencies, run
`npm --prefix client run build:native-media` to refresh the prebuilt arm64 helper,
manifest, and native notices. This cross-compiles the small Pion publisher and
does not rebuild SwiftCapture or generate a macOS app bundle. Standard
`npm run electron:mac` packaging on the Mac includes the updated resources.

## Verification

`go test -race ./client/native/media-worker` exchanges real RTP over WebRTC with
five independent tracks and checks timestamps, codec negotiation, malformed
frames, and shutdown. `client/tests/native-media.test.ts` covers fragmented SCAP,
PCM conversion, native source arguments, and bounded audio buffering.

`HAUNT_CLIENT_CHECK=native-media npm --prefix client run test:integration` uses
the isolated test database and the production Electron handlers with simulated
Electron device/permission APIs. A test-only FD producer emits FFmpeg H.264 and
stereo PCM. The real Pion publisher, browser client, and service are exercised
over WebRTC and WebSocket. It checks remote video/audio, simultaneous sources,
reconfiguration, and absence of a second video encode. This Linux test does not
verify ScreenCaptureKit/AVFoundation device access, macOS TCC permissions,
VideoToolbox performance, or macOS code signing at runtime; those require a Mac.
It also measures actual video-element presentation for simultaneous 720p60 camera
and screen streams locally and after both relay transports, and verifies native
enumeration/permission never opens a Chromium camera.
The fixture uses binary packets and disables the automation driver's DevTools
Network recording during media flow, avoiding copies of every media packet into
the test-control connection. Normal app HTTP/WebSocket traffic remains active.
The production app does not depend on this test-driver setting.

In Electron's developer console, `await window.chatxoDesktop.mediaDiagnostics()`
reports whether app hardware acceleration is enabled and Chromium's GPU feature
status, including video decoding. WebCodecs' [`prefer-hardware`](https://www.w3.org/TR/webcodecs/#enumdef-hardwareacceleration) is a preference,
not proof that a particular stream used VideoToolbox. Confirm a stream's actual
decoder in Chromium media/WebRTC diagnostics on the Mac when investigating
device-specific performance.

The service includes a relay compatibility fix: negotiated H.264 profile and
packetization mode must agree, but the level and endpoint preferences can differ;
Opus receive preferences likewise need not be identical strings. The stricter
in-stream format-change check remains intact. The Oracle test service received
this fix on October 10 at 11:43:37 PM EDT. Other services must be upgraded too.
