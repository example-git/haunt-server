package voice

import (
	"bytes"
	"encoding/hex"
	"strings"

	"github.com/pion/webrtc/v4"
)

// Used only after Pion has negotiated/bound the subscription. Stream capability
// parameters need not be byte-for-byte identical on the two peer connections.
// Keep codecEqual for detecting a publisher changing its format mid-stream.
func relayCodecCompatible(receiver, source webrtc.RTPCodecCapability) bool {
	if !strings.EqualFold(receiver.MimeType, source.MimeType) || receiver.ClockRate != source.ClockRate || receiver.Channels != source.Channels {
		return false
	}
	switch strings.ToLower(source.MimeType) {
	case "audio/opus":
		// Bitrate, stereo receive preference, FEC, and maxplaybackrate describe
		// endpoints; they do not change Opus packet framing or require transcoding.
		return true
	case "video/h264":
		parse := func(line string) map[string]string {
			values := map[string]string{"profile-level-id": "42e01f", "packetization-mode": "0"}
			for _, parameter := range strings.Split(line, ";") {
				if k, v, ok := strings.Cut(parameter, "="); ok {
					values[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
				}
			}
			return values
		}
		a, b := parse(receiver.SDPFmtpLine), parse(source.SDPFmtpLine)
		ap, ae := hex.DecodeString(a["profile-level-id"])
		bp, be := hex.DecodeString(b["profile-level-id"])
		// RFC 6184 8.2.2: profile and packetization mode are symmetric; level
		// and stream properties may differ. This matches Pion's H.264 matcher.
		return ae == nil && be == nil && len(ap) == 3 && len(bp) == 3 && bytes.Equal(ap[:2], bp[:2]) && a["packetization-mode"] == b["packetization-mode"]
	default:
		return codecEqual(receiver, source)
	}
}
