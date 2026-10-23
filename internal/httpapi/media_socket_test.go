package httpapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/voice"
)

type socketPeer struct {
	conn    *websocket.Conn
	data    chan []byte
	control chan map[string]json.RawMessage
	done    chan struct{}
}

func mediaSocket(t *testing.T, base, token, participant string) *socketPeer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/v1/media", nil)
	if err != nil {
		t.Fatal(err)
	}
	wsSend(t, conn, map[string]string{"type": "authenticate", "token": token, "participant_id": participant})
	wsRead(t, conn, "ready")
	p := &socketPeer{conn: conn, data: make(chan []byte, 128), control: make(chan map[string]json.RawMessage, 128), done: make(chan struct{})}
	readCtx, stop := context.WithCancel(context.Background())
	go func() {
		defer close(p.done)
		for {
			kind, data, err := conn.Read(readCtx)
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				select {
				case p.data <- data:
				case <-readCtx.Done():
					return
				}
			} else {
				var value map[string]json.RawMessage
				if json.Unmarshal(data, &value) != nil {
					return
				}
				select {
				case p.control <- value:
				case <-readCtx.Done():
					return
				}
			}
		}
	}()
	t.Cleanup(func() { stop(); conn.CloseNow(); <-p.done })
	return p
}
func mediaConfigure(t *testing.T, p *socketPeer, id string, tracks []voice.EncodedTrack) uint32 {
	t.Helper()
	wsSend(t, p.conn, map[string]any{"type": "configure", "tracks": tracks})
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case value := <-p.control:
			var owner string
			json.Unmarshal(value["participant_id"], &owner)
			if owner == id {
				var result []voice.EncodedTrack
				if json.Unmarshal(value["tracks"], &result) == nil && len(result) > 0 {
					return result[0].Revision
				}
			}
		case <-deadline.C:
			t.Fatal("track configuration not acknowledged")
			return 0
		}
	}
}
func mediaChunk(t *testing.T, p *socketPeer, track byte, revision, sequence uint32, key bool, payload []byte) {
	t.Helper()
	data := make([]byte, 24+len(payload))
	data[0] = 1
	data[1] = track
	if key {
		data[2] = 1
	}
	binary.BigEndian.PutUint32(data[4:8], revision)
	binary.BigEndian.PutUint32(data[16:20], 20000)
	binary.BigEndian.PutUint32(data[20:24], sequence)
	copy(data[24:], payload)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.conn.Write(ctx, websocket.MessageBinary, data); err != nil {
		t.Fatal(err)
	}
}
func mediaReceive(t *testing.T, p *socketPeer) []byte {
	t.Helper()
	select {
	case data := <-p.data:
		return data
	case <-time.After(5 * time.Second):
		t.Fatal("media frame not received")
		return nil
	}
}
func noMedia(t *testing.T, p *socketPeer) {
	t.Helper()
	select {
	case <-p.data:
		t.Fatal("unauthorized/suppressed media delivered")
	case <-time.After(120 * time.Millisecond):
	}
}

func TestCloudflaredWebSocketMediaIsolationLifecycleAndDecode(t *testing.T) {
	f, features := featureFixture(t, 256_000_000, func(c *config.Config) { c.Cloudflared = true })
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	var room, other identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "call", "kind": "voice"}, 201, &room)
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "other", "kind": "voice"}, 201, &other)
	var admission voice.Admission
	f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", alice.Token, nil, 201, &admission)
	if admission.Transport != "websocket" || admission.MediaPath != "/api/v1/media" || len(admission.ICE) != 0 {
		t.Fatal("fallback not automatic")
	}
	pubEvents, pubID := joinVoice(t, f, alice.Token, room.ID)
	subEvents, subID := joinVoice(t, f, bob.Token, room.ID)
	_, otherID := joinVoice(t, f, f.owner.Token, other.ID)
	target, _ := url.Parse(f.server.URL)
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(proxy.Close)
	pub := mediaSocket(t, proxy.URL, alice.Token, pubID.ID)
	sub := mediaSocket(t, proxy.URL, bob.Token, subID.ID)
	outsider := mediaSocket(t, proxy.URL, f.owner.Token, otherID.ID)
	revision := mediaConfigure(t, pub, pubID.ID, []voice.EncodedTrack{{Purpose: "audio", Codec: "opus", SampleRate: 48000, Channels: 2}, {Purpose: "screen", Codec: "avc1.42e01f", Width: 64, Height: 64}})
	mediaChunk(t, pub, 0, revision, 0, true, []byte("audio"))
	received := mediaReceive(t, sub)
	id, _ := hex.DecodeString(strings.ReplaceAll(pubID.ID, "-", ""))
	if !bytes.Equal(received[:16], id) || !bytes.Equal(received[40:], []byte("audio")) {
		t.Fatal("publisher attribution/payload changed")
	}
	noMedia(t, outsider)
	wsSend(t, pubEvents, map[string]any{"type": "media.state", "state": voice.State{Muted: true}})
	wsRead(t, pubEvents, "media.state.accepted")
	mediaChunk(t, pub, 0, revision, 1, true, []byte("muted"))
	noMedia(t, sub)
	wsSend(t, pubEvents, map[string]any{"type": "media.state", "state": voice.State{}})
	wsRead(t, pubEvents, "media.state.accepted")
	wsSend(t, subEvents, map[string]any{"type": "media.state", "state": voice.State{Deafened: true}})
	wsRead(t, subEvents, "media.state.accepted")
	mediaChunk(t, pub, 0, revision, 2, true, []byte("deafened"))
	noMedia(t, sub)
	wsSend(t, subEvents, map[string]any{"type": "media.state", "state": voice.State{}})
	wsRead(t, subEvents, "media.state.accepted")
	mediaChunk(t, pub, 0, revision, 3, true, []byte("resumed"))
	if !bytes.Equal(mediaReceive(t, sub)[40:], []byte("resumed")) {
		t.Fatal("audio did not resume")
	}
	// A client cannot claim deafen while keeping its microphone publication live.
	wsSend(t, pubEvents, map[string]any{"type": "media.state", "state": voice.State{Deafened: true, Muted: false}})
	wsRead(t, pubEvents, "media.state.accepted")
	mediaChunk(t, pub, 0, revision, 4, true, []byte("deafened publisher"))
	noMedia(t, sub)
	wsSend(t, pubEvents, map[string]any{"type": "media.state", "state": voice.State{}})
	wsRead(t, pubEvents, "media.state.accepted")
	mediaChunk(t, pub, 0, revision, 5, true, []byte("publisher resumed"))
	if !bytes.Equal(mediaReceive(t, sub)[40:], []byte("publisher resumed")) {
		t.Fatal("publisher did not resume after undeafen")
	}
	scratch := t.TempDir()
	source := filepath.Join(scratch, "source.h264")
	output := filepath.Join(scratch, "forwarded.h264")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if result, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10", "-frames:v", "1", "-c:v", "libx264", "-threads", "1", "-profile:v", "baseline", "-f", "h264", source).CombinedOutput(); err != nil {
		t.Fatalf("generate H264: %v %s", err, result)
	}
	encoded, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	mediaChunk(t, pub, 2, revision, 0, false, []byte("delta"))
	noMedia(t, sub)
	mediaChunk(t, pub, 2, revision, 1, true, encoded)
	forwarded := mediaReceive(t, sub)[40:]
	if !bytes.Equal(forwarded, encoded) {
		t.Fatal("video encoded bytes changed")
	}
	if err := os.WriteFile(output, forwarded, 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-xerror", "-threads", "1", "-i", output, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("decode relayed video: %v %s", err, result)
	}
	// Session revocation closes the event participant and its media socket.
	f.call(t, "POST", "/api/v1/auth/logout", alice.Token, nil, 204, nil)
	select {
	case <-pub.done:
	case <-time.After(6 * time.Second):
		t.Fatal("revoked media session stayed connected")
	}
	if features.Voice.Stats()["peer_connections"].(int64) != 0 {
		t.Fatal("WebSocket fallback allocated a WebRTC peer")
	}
}

func TestWebSocketMediaAuthenticationReconnectAndRevocation(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000, func(c *config.Config) { c.Cloudflared = true })
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	var otherDevice identity.Login
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": testPassword, "device_name": "second device"}, 200, &otherDevice)
	var room identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "call", "kind": "voice"}, 201, &room)
	events, participant := joinVoice(t, f, alice.Token, room.ID)
	pub := mediaSocket(t, f.server.URL, alice.Token, participant.ID)
	tracks := []voice.EncodedTrack{{Purpose: "audio", Codec: "opus", SampleRate: 48000, Channels: 1}}
	oldRevision := mediaConfigure(t, pub, participant.ID, tracks)
	wsRead(t, events, "media.tracks")
	for _, tc := range []struct{ name, token, participant string }{
		{"other user", bob.Token, participant.ID},
		{"other device", otherDevice.Token, participant.ID},
		{"invalid token", "bad", participant.ID},
		{"unknown participant", alice.Token, identity.NewID()},
		{"duplicate socket", alice.Token, participant.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.server.URL, "http")+"/api/v1/media", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			wsSend(t, conn, map[string]string{"type": "authenticate", "token": tc.token, "participant_id": tc.participant})
			if _, _, err := conn.Read(ctx); err == nil {
				t.Fatal("unauthorized media attachment accepted")
			}
		})
	}
	pub.conn.CloseNow()
	wsRead(t, events, "media.tracks")
	pub = mediaSocket(t, f.server.URL, alice.Token, participant.ID)
	revision := mediaConfigure(t, pub, participant.ID, tracks)
	if revision <= oldRevision {
		t.Fatal("media reconnect reused a stale stream revision")
	}
	wsRead(t, events, "media.tracks")
	// Replaying the previous connection's stream revision terminates only media.
	mediaChunk(t, pub, 0, oldRevision, 0, true, []byte{1})
	select {
	case <-pub.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stale stream remained connected")
	}
	wsRead(t, events, "media.tracks")
	pub = mediaSocket(t, f.server.URL, alice.Token, participant.ID)
	var roles struct {
		Roles []identity.Role `json:"roles"`
	}
	f.call(t, "GET", "/api/v1/roles", f.owner.Token, nil, 200, &roles)
	f.call(t, "PUT", "/api/v1/channels/"+room.ID+"/permissions/"+roles.Roles[0].ID, f.owner.Token, map[string]any{"allow": 0, "deny": 4}, 204, nil)
	wsRead(t, events, "media.revoked")
	select {
	case <-pub.done:
	case <-time.After(5 * time.Second):
		t.Fatal("permission-revoked media stayed connected")
	}
}
