package voice

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/identity"
)

func encodedPeer(t *testing.T, m *Manager, room *room) *RelayPeer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &participant{id: identity.NewID(), room: room, send: func(any) bool { return true }}
	r := &RelayPeer{m: m, p: p, ctx: ctx, cancel: cancel, frames: make(chan RelayFrame, 64), seen: map[string][5]uint32{}, sourceID: make([]byte, 16)}
	p.relay = r
	room.members[p.id] = p
	t.Cleanup(r.Close)
	return r
}
func encodedChunk(purpose byte, revision, sequence uint32, key bool, payload []byte) []byte {
	data := make([]byte, 24+len(payload))
	data[0] = 1
	data[1] = purpose
	if key {
		data[2] = 1
	}
	binary.BigEndian.PutUint32(data[4:8], revision)
	binary.BigEndian.PutUint32(data[20:24], sequence)
	copy(data[24:], payload)
	return data
}
func drainRelay(r *RelayPeer) {
	for len(r.frames) > 0 {
		r.Next(context.Background())
	}
}
func TestEncodedKeyframeStateGenerationAndBounds(t *testing.T) {
	m := &Manager{}
	room := &room{members: map[string]*participant{}}
	pub := encodedPeer(t, m, room)
	sub := encodedPeer(t, m, room)
	if err := pub.Configure([]EncodedTrack{{Purpose: "camera", Codec: "vp09.00.10.08", Width: 64, Height: 64}}); err != nil {
		t.Fatal(err)
	}
	revision := pub.tracks[1].Revision
	drainRelay(pub)
	drainRelay(sub)
	if err := pub.Forward(encodedChunk(1, revision, 0, false, []byte{1})); err != nil {
		t.Fatal(err)
	}
	if len(sub.frames) != 0 {
		t.Fatal("late receiver got delta before keyframe")
	}
	if err := pub.Forward(encodedChunk(1, revision, 1, true, []byte{2})); err != nil {
		t.Fatal(err)
	}
	frame, err := sub.Next(context.Background())
	if err != nil || !frame.Binary || len(frame.Data) != 41 || frame.Data[40] != 2 || !sub.Deliverable(frame) {
		t.Fatal("keyframe forwarding failed")
	}
	room.mu.Lock()
	old := pub.p.state
	pub.p.state.CameraOff = true
	resetRelayVideo(pub.p, old)
	room.mu.Unlock()
	if sub.Deliverable(frame) {
		t.Fatal("queued frame bypassed camera-off")
	}
	room.mu.Lock()
	old = pub.p.state
	pub.p.state.CameraOff = false
	resetRelayVideo(pub.p, old)
	room.mu.Unlock()
	if err := pub.Forward(encodedChunk(1, revision, 2, false, []byte{3})); err != nil {
		t.Fatal(err)
	}
	if len(sub.frames) != 0 {
		t.Fatal("resume did not require fresh keyframe")
	}
	if err := pub.Forward(encodedChunk(1, revision, 2, true, []byte{3})); err == nil {
		t.Fatal("repeated sequence accepted")
	}
	if err := pub.Configure([]EncodedTrack{{Purpose: "screen", Codec: "avc1.42e01f", Width: 64, Height: 64, Description: []byte{1, 2}}}); err != nil {
		t.Fatal(err)
	}
	if sub.Deliverable(frame) {
		t.Fatal("stale configured stream survived replacement")
	}
	for _, invalid := range [][]EncodedTrack{{{Purpose: "camera", Codec: "x", Width: 10000, Height: 1}}, {{Purpose: "audio", Codec: "opus", SampleRate: 48000, Channels: 100}}, {{Purpose: "camera", Codec: "", Width: 64, Height: 64}}, {{Purpose: "other", Codec: "x"}}} {
		if pub.Configure(invalid) == nil {
			t.Fatal("invalid track accepted")
		}
	}
	for _, invalid := range [][]byte{make([]byte, 24), make([]byte, EncodedFrameLimit+1), encodedChunk(3, 1, 0, true, []byte{1}), encodedChunk(2, revision, 0, true, []byte{1})} {
		if pub.Forward(invalid) == nil {
			t.Fatal("invalid chunk accepted")
		}
	}
}
func TestEncodedSlowQueueDisconnectsAndReleasesAccounting(t *testing.T) {
	m := &Manager{}
	room := &room{members: map[string]*participant{}}
	peer := encodedPeer(t, m, room)
	for range 4 {
		if !peer.push(RelayFrame{Data: make([]byte, 512<<10)}) {
			t.Fatal("queue filled prematurely")
		}
	}
	if peer.push(RelayFrame{Data: []byte{1}}) {
		t.Fatal("byte limit not enforced")
	}
	select {
	case <-peer.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("slow receiver not disconnected")
	}
	peer.Close()
	peer.Close()
	if m.relayBytes.Load() != 0 || m.relayOverflows.Load() != 1 {
		t.Fatal("queue accounting leaked")
	}
	peer = encodedPeer(t, m, room)
	for range 64 {
		peer.push(RelayFrame{Data: []byte{1}})
	}
	if peer.push(RelayFrame{Data: []byte{1}}) {
		t.Fatal("frame limit not enforced")
	}
}

func TestSeparateAudioSourcesRespectMuteDeafenAndSourceState(t *testing.T) {
	m := &Manager{}
	room := &room{members: map[string]*participant{}}
	pub, sub := encodedPeer(t, m, room), encodedPeer(t, m, room)
	tracks := []EncodedTrack{}
	for _, purpose := range []string{"audio", "camera_audio", "screen_audio"} {
		tracks = append(tracks, EncodedTrack{Purpose: purpose, Codec: "opus", SampleRate: 48000, Channels: 2})
	}
	if err := pub.Configure(tracks); err != nil {
		t.Fatal(err)
	}
	drainRelay(pub)
	drainRelay(sub)
	for _, test := range []struct {
		publisher, receiver State
		allowed             [3]bool
	}{
		{State{}, State{}, [3]bool{true, true, true}},
		{State{Muted: true}, State{}, [3]bool{false, true, true}},
		{State{Deafened: true}, State{}, [3]bool{false, true, true}},
		{State{CameraOff: true}, State{}, [3]bool{true, false, true}},
		{State{ScreenOff: true}, State{}, [3]bool{true, true, false}},
		{State{}, State{Deafened: true}, [3]bool{}},
	} {
		pub.p.state, sub.p.state = test.publisher, test.receiver
		for i, purpose := range []int{0, 3, 4} {
			track := pub.tracks[purpose]
			seq := track.sequence + 1
			if err := pub.Forward(encodedChunk(byte(purpose), track.Revision, seq, false, []byte{1})); err != nil {
				t.Fatal(err)
			}
			if got := len(sub.frames) > 0; got != test.allowed[i] {
				t.Fatalf("%s forwarding = %v for %+v", purposeName(purpose), got, test)
			}
			if test.allowed[i] {
				frame, _ := sub.Next(context.Background())
				if !sub.Deliverable(frame) {
					t.Fatal("valid audio rejected")
				}
				sub.p.state.Deafened = true
				if sub.Deliverable(frame) {
					t.Fatal("queued audio bypassed deafen")
				}
				sub.p.state = test.receiver
				if purpose == 0 {
					pub.p.state.Deafened = true
					if sub.Deliverable(frame) {
						t.Fatal("queued microphone bypassed publisher deafen")
					}
					pub.p.state = test.publisher
				}
			}
		}
	}
	for _, purpose := range []int{3, 4} {
		track := pub.tracks[purpose]
		if err := pub.Forward(encodedChunk(byte(purpose), track.Revision, track.sequence+1, false, make([]byte, 64<<10))); err == nil {
			t.Fatal("source audio bypassed audio size limit")
		}
		if err := sub.RequestKeyframe(pub.p.id, purposeName(purpose)); err == nil {
			t.Fatal("audio accepted a video keyframe request")
		}
	}
}
