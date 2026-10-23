package voice

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestPacketExtensionRemappingDoesNotMutateSource(t *testing.T) {
	original := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 17, Timestamp: 90, SSRC: 123}, Payload: []byte{7, 8, 9}}
	if err := original.SetExtension(1, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if err := original.SetExtension(2, []byte{4, 5}); err != nil {
		t.Fatal(err)
	}
	mapped := packetMessage("camera", original, map[uint8]uint8{2: 9}).rtp
	if mapped.GetExtension(1) != nil || !bytes.Equal(mapped.GetExtension(9), []byte{4, 5}) {
		t.Fatal("extension remap failed")
	}
	mapped.Payload[0] = 99
	mapped.GetExtension(9)[0] = 88
	if original.Payload[0] != 7 || original.GetExtension(2)[0] != 4 || original.SequenceNumber != 17 {
		t.Fatal("receiver modified source/shared packet")
	}
}

func TestDeafenNormalizesStoredAndBroadcastState(t *testing.T) {
	r := &room{members: map[string]*participant{}}
	var received []Snapshot
	p := &participant{id: "sender", room: r, send: func(value any) bool {
		received = append(received, value.(map[string]any)["participant"].(Snapshot))
		return true
	}}
	r.members[p.id] = p
	m := &Manager{participants: map[string]*participant{p.id: p}}
	for _, requested := range []State{
		{Deafened: true, CameraOff: true, ScreenOff: true},
		{Muted: true},
		{},
	} {
		if err := m.SetState(p.id, requested); err != nil {
			t.Fatal(err)
		}
		expected := requested
		if expected.Deafened {
			expected.Muted = true
		}
		if p.state != expected || received[len(received)-1].State != expected {
			t.Fatalf("requested %+v: stored %+v, broadcast %+v", requested, p.state, received)
		}
		if blockedSource(p.state, State{}, "audio") != expected.Muted {
			t.Fatal("microphone forwarding differs from visible mute state")
		}
	}
}
func TestCodecParametersAndPeerCapacity(t *testing.T) {
	a := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "packetization-mode=1;profile-level-id=42001f"}
	b := a
	b.MimeType = strings.ToUpper(b.MimeType)
	b.SDPFmtpLine = "profile-level-id=42001f; packetization-mode=1"
	if !codecEqual(a, b) {
		t.Fatal("equivalent fmtp order rejected")
	}
	b.SDPFmtpLine = "packetization-mode=0;profile-level-id=42001f"
	if codecEqual(a, b) {
		t.Fatal("incompatible packetization accepted")
	}
	m, err := New(config.Config{MediaMaxPeers: 2}, &identity.Service{})
	if err != nil {
		t.Fatal(err)
	}
	peers := []*webrtc.PeerConnection{}
	for range 2 {
		pc, err := m.newPeer()
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, pc)
	}
	if extra, err := m.newPeer(); err != ErrCapacity {
		if extra != nil {
			extra.Close()
		}
		t.Fatal("peer capacity not enforced")
	}
	for _, pc := range peers {
		pc.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestOfferResourceLimits(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	for range 5 {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOffer(offer); err != nil {
		t.Fatal("five-source offer rejected", err)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	offer, err = pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if validateOffer(offer) == nil {
		t.Fatal("unbounded transceiver offer accepted")
	}
	for _, offer := range []webrtc.SessionDescription{{Type: webrtc.SDPTypeOffer, SDP: "garbage"}, {Type: webrtc.SDPTypeAnswer, SDP: "invalid"}, {Type: webrtc.SDPTypeOffer, SDP: strings.Repeat("x", 97<<10)}} {
		if validateOffer(offer) == nil {
			t.Fatal("invalid SDP accepted")
		}
	}
}

func FuzzPacketForwarding(f *testing.F) {
	f.Add([]byte{0x80, 96, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 1, 2, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			return
		}
		var packet rtp.Packet
		if packet.Unmarshal(data) != nil {
			return
		}
		before, err := packet.Marshal()
		if err != nil {
			return
		}
		clone := packetMessage("camera", &packet, map[uint8]uint8{1: 2, 2: 3, 3: 1}).rtp
		if len(clone.Payload) > 0 {
			clone.Payload[0] ^= 255
		}
		for _, id := range clone.GetExtensionIDs() {
			value := clone.GetExtension(id)
			if len(value) > 0 {
				value[0] ^= 255
			}
		}
		after, err := packet.Marshal()
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("forwarding mutated source packet")
		}
	})
}
