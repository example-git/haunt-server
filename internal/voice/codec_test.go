package voice

import (
	"github.com/pion/webrtc/v4"
	"testing"
)

func TestRelayCodecCompatibility(t *testing.T) {
	h264 := func(fmtp string) webrtc.RTPCodecCapability {
		return webrtc.RTPCodecCapability{MimeType: "video/H264", ClockRate: 90000, SDPFmtpLine: fmtp}
	}
	base := h264("packetization-mode=1;profile-level-id=42e01f;level-asymmetry-allowed=1")
	for _, test := range []struct {
		fmtp string
		want bool
	}{
		{"packetization-mode=1;profile-level-id=42e02a;level-asymmetry-allowed=1", true},
		{"packetization-mode=0;profile-level-id=42e01f", false},
		{"packetization-mode=1;profile-level-id=64001f", false},
		{"packetization-mode=1;profile-level-id=garbage", false},
		{"packetization-mode=1;profile-level-id=42e0", false},
	} {
		if got := relayCodecCompatible(base, h264(test.fmtp)); got != test.want {
			t.Errorf("%s: got %v", test.fmtp, got)
		}
	}
	a := webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}
	b := a
	b.SDPFmtpLine += ";stereo=1;maxaveragebitrate=510000;sprop-stereo=1"
	if !relayCodecCompatible(a, b) || codecEqual(a, b) {
		t.Fatal("endpoint preferences confused with an in-stream format change")
	}
	b.Channels = 1
	if relayCodecCompatible(a, b) {
		t.Fatal("channel mismatch accepted")
	}
	b = a
	b.ClockRate = 16000
	if relayCodecCompatible(a, b) {
		t.Fatal("clock mismatch accepted")
	}
	if relayCodecCompatible(a, base) {
		t.Fatal("codec mismatch accepted")
	}
	vp9 := webrtc.RTPCodecCapability{MimeType: "video/VP9", ClockRate: 90000, SDPFmtpLine: "profile-id=0"}
	other := vp9
	other.SDPFmtpLine = "profile-id=2"
	if relayCodecCompatible(vp9, other) {
		t.Fatal("unrelated codec compatibility relaxed")
	}
}
