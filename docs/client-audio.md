# Client audio

Devices settings are local to the browser profile or Electron installation, not
the server login. They survive sign-out and reload, are usable while signed out,
and are included in the existing client backup. Device IDs may require renewed
permission or reselection when moving a backup to another machine.

The output selection follows active call audio (microphone, camera audio and
screen audio), attached video playback, and notification/call sounds. Switching
outputs does not reconnect a call. A missing/revoked output falls back to the
system default without erasing the saved choice; device changes retry that
choice. Browsers without output-selection support use their system output.

Listen to microphone is an explicit, local-only playback test of the same
processed track used by the meter, after noise suppression and gain. It does not
publish into a room and is never persisted as enabled. Stopping the test, leaving
Devices settings, retrying capture, changing the input, or disconnecting the
microphone stops playback. Closing settings also releases its capture/contexts.

Both Devices and Server audio plugins previews acquire shared microphone
playback ownership before starting audio. While any preview owns playback, the
call is muted and deafened, including calls joined during a preview. Stopping the
last preview releases that condition; the user's current mute/deafen choices
remain in effect. An input level meter without playback does not affect a call.

Call state is a read-only snapshot derived from user intent, active capture
tracks and preview ownership. Deafen always disables the raw and processed
microphone tracks and all received audio. These local gates change before network
acknowledgement; state commands are serialized so older replies cannot restore
previous controls. Camera and screen flags follow their actual sources, including
capture loss and failed publication. Microphone mute is independent of camera
and screen source audio. The server also forces mute whenever deafen is set and
enforces these source gates for both transports, including queued packets.

Browser/Electron echo cancellation is explicitly disabled for calls, microphone
preview, permission probes, and source-audio capture. Suppression/model/gain
changes keep it disabled. There is no echo-processing option or additional filter.
Outgoing capture graphs have no speaker output, and received audio is never mixed
into the outgoing microphone. The explicit microphone playback test stops on
output selection or device changes, including output-device fallback; resuming
requires another click. External virtual-device routing and acoustic feedback
are controlled by the OS/audio hardware, outside this application's graph.

Electron accepts capture requests only from its trusted application renderer and
relies on OS consent, without an additional per-capture application dialog.
Browser device settings observe microphone/camera grants and refresh device lists
without opening another capture when permission is already granted.

## Quality and transport

The private-room default permits Opus up to 510 kbps in both WebRTC and the
encoded WebSocket fallback, with 48 kHz processing/negotiation. This is a quality
ceiling, not a guaranteed network bitrate; WebRTC can adapt to congestion.
Microphone capture is mono; camera and screen audio request stereo where the
source supplies it. The WebSocket PCM adapter preserves the two source channels.
The server advertises full-band stereo support and forwards encoded media without
transcoding.

### Sample rates and device conversion

The network format and the physical output clock are separate. Opus uses a
48 kHz RTP timestamp clock at every bandwidth ([RFC 7587 §4.1](https://www.rfc-editor.org/rfc/rfc7587.html#section-4.1)).
Haunt publishes 48 kHz Opus in both transports. A listener selecting a 44.1 kHz
device does not change another user's encoder, renegotiate the call, or cause
48 kHz samples to be labelled as 44.1 kHz.

Browser microphone/camera/screen tracks enter a 48 kHz Web Audio graph through
`MediaStreamAudioSourceNode`, which resamples the actual input rate. Capture
constraints are preferences, not proof of the delivered rate. The audio plugin host
therefore receives 48 kHz input. WebCodecs frames
use the actual graph rate, sample count and accumulated sample timestamps;
capture worklets frame 20 ms using that rate rather than an assumed 960 samples.
Decoded audio buffers retain the decoder's reported rate. The playback element
and browser audio backend handle the final conversion to the selected output's
clock. Web Audio also requires output resampling when its graph and device
rates differ ([Web Audio](https://www.w3.org/TR/webaudio/#dom-audiocontext-audiocontext)).
Hardware clock negotiation is owned by the browser/OS; there is no reliable
cross-browser API listing every sample rate supported by an output device.

SwiftCapture's raw PCM path validates the reported rate, channel layout and
sample representation. Its stereo worklet uses a streaming windowed-sinc
low-pass resampler instead of linear interpolation, preserving fractional
position and filter history across IPC packets. Equal rates bypass filtering.
For 44.1↔48 kHz, filter lookahead is about 1.1 ms; startup/recovery waits for
20 ms plus that lookahead (rounded up to arriving capture packets). Pending PCM
is capped at 60 ms, with stale samples
discarded after stalls. These are local buffering limits, not total call
latency. A format change retires the old converter before accepting new samples,
and concurrent configuration events cannot restore an obsolete converter.
Higher-rate capture devices (including 96/192 kHz) are filtered before conversion
to 48 kHz so ultrasonic input does not alias into audible frequencies.

The microphone uses one stable 48 kHz graph. Optional server audio plugins
receive and return 48 kHz mono samples; each plugin owns its internal processing
and resampling. Camera and screen audio bypass microphone noise suppression.

See [server audio plugins](audio-plugins.md) for the processing boundary and schema.
