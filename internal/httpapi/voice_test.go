package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/voice"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func joinVoice(t *testing.T, f *fixture, token, room string) (*websocket.Conn, voice.Snapshot) {
	t.Helper()
	var admission voice.Admission
	f.call(t, "POST", "/api/v1/voice/"+room+"/admissions", token, nil, 201, &admission)
	conn := wsConnect(t, f, token)
	wsSend(t, conn, map[string]any{"type": "media.join", "admission_id": admission.ID})
	event := wsRead(t, conn, "media.joined")
	var participant voice.Snapshot
	if json.Unmarshal(event["participant"], &participant) != nil {
		t.Fatal("invalid voice join")
	}
	if !participant.State.CameraOff || !participant.State.ScreenOff {
		t.Fatal("new participant advertised video sources before publishing")
	}
	return conn, participant
}
func peerOffer(t *testing.T, pc *webrtc.PeerConnection) webrtc.SessionDescription {
	t.Helper()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
		return *pc.LocalDescription()
	case <-time.After(5 * time.Second):
		t.Fatal("ICE gathering timed out")
		return offer
	}
}
func publishTracks(t *testing.T, conn *websocket.Conn) *webrtc.PeerConnection {
	t.Helper()
	// Enable the microphone, camera and screen, as the client does when
	// starting these sources; admission itself has no active video.
	wsSend(t, conn, map[string]any{"type": "media.state", "state": voice.State{}})
	wsRead(t, conn, "media.state.accepted")
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	t.Cleanup(func() { pc.Close(); workers.Wait() })
	capabilities := []webrtc.RTPCodecCapability{
		{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0"},
		{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"},
	}
	tracks := []*webrtc.TrackLocalStaticRTP{}
	purposes := []string{"audio", "camera", "screen"}
	ids := map[string]string{}
	for i, capability := range capabilities {
		track, err := webrtc.NewTrackLocalStaticRTP(capability, purposes[i]+"_source", "source")
		if err != nil {
			t.Fatal(err)
		}
		sender, err := pc.AddTrack(track)
		if err != nil {
			t.Fatal(err)
		}
		tracks = append(tracks, track)
		ids[purposes[i]] = track.ID()
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				if _, _, err := sender.ReadRTCP(); err != nil {
					return
				}
			}
		}()
	}
	offer := peerOffer(t, pc)
	wsSend(t, conn, map[string]any{"type": "media.publish", "sdp": offer, "tracks": ids})
	event := wsRead(t, conn, "media.answer")
	var answer webrtc.SessionDescription
	if json.Unmarshal(event["sdp"], &answer) != nil {
		t.Fatal("invalid publish answer")
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	workers.Add(1)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var seq uint16
		var timestamp uint32
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				seq++
				timestamp += 1800
				for i, track := range tracks {
					_ = track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: timestamp, SSRC: uint32(i + 1), Marker: true}, Payload: []byte{byte(i + 1), 11, 22, 33}})
				}
			}
		}
	}()
	return pc
}

type receivedTrack struct {
	purpose, codec string
	payload        []byte
}

func subscribeTracks(t *testing.T, conn *websocket.Conn, publisher string) (*webrtc.PeerConnection, <-chan receivedTrack) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	output := make(chan receivedTrack, 256)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); pc.Close(); workers.Wait() })
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				purpose := ""
				for _, kind := range []string{"audio", "camera", "screen"} {
					if strings.HasSuffix(track.ID(), "_"+kind) {
						purpose = kind
					}
				}
				select {
				case output <- receivedTrack{purpose: purpose, codec: track.Codec().MimeType, payload: append([]byte{}, packet.Payload...)}:
				case <-ctx.Done():
					return
				}
			}
		}()
	})
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeVideo} {
		if _, err := pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	offer := peerOffer(t, pc)
	wsSend(t, conn, map[string]any{"type": "media.subscribe", "publisher_id": publisher, "sdp": offer})
	event := wsRead(t, conn, "media.answer")
	var answer webrtc.SessionDescription
	if json.Unmarshal(event["sdp"], &answer) != nil {
		t.Fatal("invalid subscribe answer")
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	return pc, output
}
func awaitMedia(t *testing.T, packets <-chan receivedTrack) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for len(seen) < 3 {
		select {
		case packet := <-packets:
			want := map[string]string{"audio": webrtc.MimeTypeOpus, "camera": webrtc.MimeTypeVP9, "screen": webrtc.MimeTypeH264}[packet.purpose]
			if !strings.EqualFold(packet.codec, want) {
				t.Fatalf("%s codec %s, want %s", packet.purpose, packet.codec, want)
			}
			if len(packet.payload) != 4 || packet.payload[1] != 11 || packet.payload[2] != 22 || packet.payload[3] != 33 {
				t.Fatal("encoded RTP payload changed")
			}
			seen[packet.purpose] = true
		case <-deadline.C:
			t.Fatalf("media timeout, received purposes %v", seen)
		}
	}
}

func TestVoiceCameraScreenCodecForwardingAndRooms(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	outsider := f.register(t, "outsider")
	var room, other identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "call", "kind": "voice"}, 201, &room)
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "other", "kind": "voice"}, 201, &other)
	aliceWS, alicePeer := joinVoice(t, f, alice.Token, room.ID)
	bobWS, bobPeer := joinVoice(t, f, bob.Token, room.ID)
	outsiderWS, _ := joinVoice(t, f, outsider.Token, other.ID)
	alicePublish := publishTracks(t, aliceWS)
	publishTracks(t, bobWS)
	// Track events are emitted after actual encrypted media arrives, so
	// receivers can negotiate the source's real codec parameters.
	for _, conn := range []*websocket.Conn{aliceWS, bobWS} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			var state struct {
				Participants []voice.Snapshot `json:"participants"`
			}
			f.call(t, "GET", "/api/v1/voice/"+room.ID, alice.Token, nil, 200, &state)
			ready := len(state.Participants) == 2
			for _, p := range state.Participants {
				ready = ready && len(p.Tracks) == 3
			}
			if ready {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("publish tracks did not arrive")
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = conn
	}
	_, aliceReceives := subscribeTracks(t, aliceWS, bobPeer.ID)
	_, bobReceives := subscribeTracks(t, bobWS, alicePeer.ID)
	awaitMedia(t, aliceReceives)
	awaitMedia(t, bobReceives)
	// Replacing a publishing peer supports newly started camera/screen tracks
	// and reconnects, without closing other participants' publishing peers.
	publishTracks(t, aliceWS)
	alicePublish.Close()
	deadlineTracks := time.Now().Add(5 * time.Second)
	for {
		var state struct {
			Participants []voice.Snapshot `json:"participants"`
		}
		f.call(t, "GET", "/api/v1/voice/"+room.ID, alice.Token, nil, 200, &state)
		ready := false
		for _, p := range state.Participants {
			if p.ID == alicePeer.ID && len(p.Tracks) == 3 {
				ready = true
			}
		}
		if ready {
			break
		}
		if time.Now().After(deadlineTracks) {
			t.Fatal("replacement publisher tracks did not arrive")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, bobReceives = subscribeTracks(t, bobWS, alicePeer.ID)
	awaitMedia(t, bobReceives)
	// A participant in another room cannot subscribe to these media sources.
	outsiderPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { outsiderPC.Close() })
	outsiderPC.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	wsSend(t, outsiderWS, map[string]any{"type": "media.subscribe", "publisher_id": alicePeer.ID, "sdp": peerOffer(t, outsiderPC)})
	wsRead(t, outsiderWS, "error")
	wsSend(t, aliceWS, map[string]any{"type": "media.state", "state": voice.State{Muted: true, CameraOff: true, ScreenOff: true}})
	wsRead(t, aliceWS, "media.state.accepted")
	// Drain packets sent before the state change; then all three source types
	// must stop while the independent reverse direction continues.
	drain := time.NewTimer(200 * time.Millisecond)
	for draining := true; draining; {
		select {
		case <-bobReceives:
		case <-drain.C:
			draining = false
		}
	}
	select {
	case packet := <-bobReceives:
		t.Fatalf("muted/off %s track still forwarded", packet.purpose)
	case <-time.After(200 * time.Millisecond):
	}
	awaitMedia(t, aliceReceives)
	wsSend(t, aliceWS, map[string]any{"type": "media.leave"})
	wsRead(t, aliceWS, "media.left")
	bobWS.CloseNow()
	outsiderWS.CloseNow()
	aliceWS.CloseNow()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stats := features.Voice.Stats()
		if stats["participants"].(int) == 0 && stats["peer_connections"].(int64) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("media cleanup retained state: %v", stats)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestVoiceAdmissionsPermissionsAndRoomCapacity(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	// Install a smaller configured room capacity before accepting any peers.
	if err := features.Voice.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := f.cfg
	cfg.MediaRoomLimit = 2
	manager, err := voice.New(cfg, f.identities)
	if err != nil {
		t.Fatal(err)
	}
	features.Voice = manager
	features.Gateway.media = manager
	alice := f.register(t, "alice")
	var room identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "limited", "kind": "voice"}, 201, &room)
	text := textChannel(t, f)
	f.call(t, "POST", "/api/v1/voice/"+text.ID+"/admissions", alice.Token, nil, 400, nil)
	f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", "", nil, 401, nil)
	ownerWS, _ := joinVoice(t, f, f.owner.Token, room.ID)
	var token voice.Admission
	f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", alice.Token, nil, 201, &token)
	wsSend(t, ownerWS, map[string]any{"type": "media.join", "admission_id": token.ID})
	wsRead(t, ownerWS, "error")
	aliceWS := wsConnect(t, f, alice.Token)
	wsSend(t, aliceWS, map[string]any{"type": "media.join", "admission_id": token.ID})
	wsRead(t, aliceWS, "media.joined")
	second := wsConnect(t, f, alice.Token)
	wsSend(t, second, map[string]any{"type": "media.join", "admission_id": token.ID})
	wsRead(t, second, "error")
	var full voice.Admission
	f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", alice.Token, nil, 201, &full)
	wsSend(t, second, map[string]any{"type": "media.join", "admission_id": full.ID})
	wsRead(t, second, "error")
	wsSend(t, aliceWS, map[string]any{"type": "media.ice", "peer_id": identity.NewID(), "candidate": map[string]string{"candidate": "garbage"}})
	wsRead(t, aliceWS, "error")
	var roles struct {
		Roles []identity.Role `json:"roles"`
	}
	f.call(t, "GET", "/api/v1/roles", f.owner.Token, nil, 200, &roles)
	f.call(t, "PUT", "/api/v1/channels/"+room.ID+"/permissions/"+roles.Roles[0].ID, f.owner.Token, map[string]any{"allow": 0, "deny": 4}, 204, nil)
	wsRead(t, aliceWS, "media.revoked")
	f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", alice.Token, nil, 403, nil)
	if features.Voice.Stats()["participants"].(int) != 1 {
		t.Fatal("revocation affected wrong participants")
	}
	// Revoking a device removes its remaining room membership on the next tick.
	f.call(t, "DELETE", "/api/v1/sessions/"+f.owner.Session.ID, f.owner.Token, nil, 204, nil)
	deadline := time.Now().Add(3 * time.Second)
	for features.Voice.Stats()["participants"].(int) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("session revocation retained room membership")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
