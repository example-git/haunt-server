package httpapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/voice"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

func TestDecodableMediaThroughSFUAndTURN(t *testing.T) {
	decodableMedia(t, false)
}
func TestCloudflaredTURNDecodableMedia(t *testing.T) {
	decodableMedia(t, true)
}
func decodableMedia(t *testing.T, serverRelay bool) {
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		udp.Close()
		t.Fatal(err)
	}
	generator := &turn.RelayAddressGeneratorStatic{RelayAddress: net.ParseIP("127.0.0.1"), Address: "127.0.0.1"}
	relay, err := turn.NewServer(turn.ServerConfig{Realm: "test", AuthHandler: func(a *turn.RequestAttributes) (string, []byte, bool) {
		return a.Username, turn.GenerateAuthKey("test", "test", "test-password"), a.Username == "test"
	}, PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: udp, RelayAddressGenerator: generator}}, ListenerConfigs: []turn.ListenerConfig{{Listener: tcp, RelayAddressGenerator: generator}}})
	if err != nil {
		udp.Close()
		tcp.Close()
		t.Fatal(err)
	}
	defer relay.Close()
	f, _ := featureFixture(t, 256_000_000, func(c *config.Config) {
		if serverRelay {
			c.Cloudflared = true
			c.MediaRelayOnly = true
			c.ICE = []config.ICEServer{{URLs: []string{"turn:" + tcp.Addr().String() + "?transport=tcp"}, Username: "test", Credential: "test-password"}}
		}
	})
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	var room identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "real samples", "kind": "voice"}, 201, &room)
	if serverRelay {
		var admission voice.Admission
		f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", alice.Token, nil, 201, &admission)
		if admission.Transport != "webrtc" || admission.ICEPolicy != "relay" || len(admission.ICE) != 1 {
			t.Fatal("TURN transport was not automatic")
		}
	}
	publisherWS, publisher := joinVoice(t, f, alice.Token, room.ID)
	subscriberWS, _ := joinVoice(t, f, bob.Token, room.ID)
	for _, tc := range []struct {
		name, encoder, format, fmtp, purpose, transport string
		clock                                           uint32
		channels                                        uint16
		depacketizer                                    func() rtp.Depacketizer
	}{
		{"VP8", "libvpx", "ivf", "", "camera", "", 90000, 0, func() rtp.Depacketizer { return &codecs.VP8Packet{} }},
		{"VP9", "libvpx-vp9", "ivf", "profile-id=0", "camera", "udp", 90000, 0, func() rtp.Depacketizer { return &codecs.VP9Packet{} }},
		{"H264", "libx264", "h264", "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", "screen", "tcp", 90000, 0, func() rtp.Depacketizer { return &codecs.H264Packet{} }},
		{"AV1", "libaom-av1", "ivf", "", "screen", "", 90000, 0, func() rtp.Depacketizer { return &codecs.AV1Depacketizer{} }},
		{"opus", "libopus", "ogg", "minptime=10;useinbandfec=1", "audio", "udp", 48000, 2, func() rtp.Depacketizer { return &codecs.OpusPacket{} }},
	} {
		t.Run(tc.name+"/"+tc.transport, func(t *testing.T) {
			scratch := t.TempDir()
			original := filepath.Join(scratch, "source."+tc.format)
			args := []string{"-v", "error", "-threads", "1", "-filter_threads", "1", "-f", "lavfi"}
			if tc.purpose == "audio" {
				args = append(args, "-i", "sine=frequency=440:sample_rate=48000", "-t", "0.1", "-ac", "2", "-c:a", tc.encoder, "-frame_duration", "20", "-page_duration", "20000")
			} else {
				args = append(args, "-i", "testsrc2=size=64x64:rate=10", "-frames:v", "1", "-c:v", tc.encoder, "-threads", "1", "-pix_fmt", "yuv420p")
				if tc.name == "H264" {
					args = append(args, "-profile:v", "baseline", "-level", "3.1")
				}
			}
			args = append(args, "-f", tc.format, "-y", original)
			command := func(args ...string) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if output, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
					t.Fatalf("media codec check failed: %v: %s", err, output)
				}
			}
			command(args...)
			encoded, err := os.ReadFile(original)
			if err != nil {
				t.Fatal(err)
			}
			sample := encoded
			if tc.format == "ivf" {
				reader, _, err := ivfreader.NewWith(bytes.NewReader(encoded))
				if err != nil {
					t.Fatal(err)
				}
				sample, _, err = reader.ParseNextFrame()
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.format == "ogg" {
				reader, _, err := oggreader.NewWith(bytes.NewReader(encoded))
				if err != nil {
					t.Fatal(err)
				}
				for {
					sample, _, err = reader.ParseNextPage()
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.HasPrefix(sample, []byte("OpusTags")) {
						break
					}
				}
			}
			cfg := webrtc.Configuration{}
			if serverRelay {
				cfg = webrtc.Configuration{ICETransportPolicy: webrtc.ICETransportPolicyRelay, ICEServers: []webrtc.ICEServer{{URLs: f.cfg.ICE[0].URLs, Username: "test", Credential: "test-password"}}}
			}
			if tc.transport != "" {
				address := udp.LocalAddr().String()
				if tc.transport == "tcp" {
					address = tcp.Addr().String()
				}
				cfg = webrtc.Configuration{ICETransportPolicy: webrtc.ICETransportPolicyRelay, ICEServers: []webrtc.ICEServer{{URLs: []string{"turn:" + address + "?transport=" + tc.transport}, Username: "test", Credential: "test-password"}}}
			}
			pub, err := webrtc.NewPeerConnection(cfg)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := webrtc.NewPeerConnection(cfg)
			if err != nil {
				pub.Close()
				t.Fatal(err)
			}
			streamCtx, cancel := context.WithCancel(context.Background())
			var workers sync.WaitGroup
			defer func() { cancel(); pub.Close(); sub.Close(); workers.Wait() }()
			mime := "video/" + tc.name
			if tc.purpose == "audio" {
				mime = "audio/opus"
			}
			track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: mime, ClockRate: tc.clock, Channels: tc.channels, SDPFmtpLine: tc.fmtp}, "encoded_"+tc.name, "actual-source")
			if err != nil {
				t.Fatal(err)
			}
			sender, err := pub.AddTrack(track)
			if err != nil {
				t.Fatal(err)
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					if _, _, err := sender.ReadRTCP(); err != nil {
						return
					}
				}
			}()
			wsSend(t, publisherWS, map[string]any{"type": "media.publish", "sdp": peerOffer(t, pub), "tracks": map[string]string{tc.purpose: track.ID()}})
			answer := wsRead(t, publisherWS, "media.answer")
			var description webrtc.SessionDescription
			if err := json.Unmarshal(answer["sdp"], &description); err != nil {
				t.Fatal(err)
			}
			if serverRelay && (!strings.Contains(description.SDP, " typ relay") || strings.Contains(description.SDP, " typ host")) {
				t.Fatal("SFU bypassed external TURN")
			}
			if err := pub.SetRemoteDescription(description); err != nil {
				t.Fatal(err)
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				ticker := time.NewTicker(30 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-streamCtx.Done():
						return
					case <-ticker.C:
						track.WriteSample(media.Sample{Data: sample, Duration: time.Second / 10})
					}
				}
			}()
			deadline := time.Now().Add(8 * time.Second)
			for {
				var state struct {
					Participants []voice.Snapshot `json:"participants"`
				}
				f.call(t, "GET", "/api/v1/voice/"+room.ID, alice.Token, nil, 200, &state)
				ready := false
				for _, p := range state.Participants {
					if p.ID == publisher.ID && len(p.Tracks) == 1 && strings.EqualFold(p.Tracks[0].Codec.MimeType, mime) {
						ready = true
					}
				}
				if ready {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("encoded publisher track unavailable")
				}
				time.Sleep(20 * time.Millisecond)
			}
			received := make(chan []byte, 1)
			sub.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
				workers.Add(1)
				go func() {
					defer workers.Done()
					builder := samplebuilder.New(64, tc.depacketizer(), tc.clock)
					for {
						packet, _, err := remote.ReadRTP()
						if err != nil {
							return
						}
						builder.Push(packet)
						for value := builder.Pop(); value != nil; value = builder.Pop() {
							select {
							case received <- value.Data:
							default:
							}
						}
					}
				}()
			})
			kind := webrtc.RTPCodecTypeVideo
			if tc.purpose == "audio" {
				kind = webrtc.RTPCodecTypeAudio
			}
			if _, err := sub.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
				t.Fatal(err)
			}
			wsSend(t, subscriberWS, map[string]any{"type": "media.subscribe", "publisher_id": publisher.ID, "sdp": peerOffer(t, sub)})
			answer = wsRead(t, subscriberWS, "media.answer")
			if err := json.Unmarshal(answer["sdp"], &description); err != nil {
				t.Fatal(err)
			}
			if err := sub.SetRemoteDescription(description); err != nil {
				t.Fatal(err)
			}
			var forwarded []byte
			timeout := time.NewTimer(10 * time.Second)
			defer timeout.Stop()
			for forwarded == nil {
				select {
				case candidate := <-received:
					// Joining a running stream can start halfway through an access
					// unit. A receiver waits for SPS/PPS plus IDR before decoding.
					if tc.name == "H264" && !completeH264Keyframe(candidate) {
						continue
					}
					forwarded = candidate
				case <-timeout.C:
					t.Fatal("no complete decodable keyframe received")
				}
			}
			output := filepath.Join(scratch, "forwarded."+tc.format)
			switch tc.format {
			case "ivf":
				file := append([]byte{}, encoded[:32]...)
				header := make([]byte, 12)
				binary.LittleEndian.PutUint32(header, uint32(len(forwarded)))
				file = append(file, header...)
				file = append(file, forwarded...)
				if err := os.WriteFile(output, file, 0600); err != nil {
					t.Fatal(err)
				}
			case "h264":
				if err := os.WriteFile(output, forwarded, 0600); err != nil {
					t.Fatal(err)
				}
			case "ogg":
				if !bytes.Equal(sample, forwarded) {
					t.Fatal("encoded Opus frame changed")
				}
				if err := os.WriteFile(output, encoded, 0600); err != nil {
					t.Fatal(err)
				}
			}
			command("-v", "error", "-xerror", "-threads", "1", "-i", output, "-f", "null", "-")
			if tc.transport != "" {
				for _, pc := range []*webrtc.PeerConnection{pub, sub} {
					stats := pc.GetStats()
					found := false
					for _, item := range stats {
						if pair, ok := item.(webrtc.ICECandidatePairStats); ok && pair.Nominated {
							if local, ok := stats[pair.LocalCandidateID].(webrtc.ICECandidateStats); ok && local.CandidateType == webrtc.ICECandidateTypeRelay {
								found = true
							}
						}
					}
					if !found {
						t.Fatal("TURN test did not select a relay candidate")
					}
				}
			}
			t.Logf("%s sample decoded after SFU forwarding (%s)", tc.name, fmt.Sprint(tc.transport))
		})
	}
}

func completeH264Keyframe(data []byte) bool {
	reader, err := h264reader.NewReader(bytes.NewReader(data))
	if err != nil {
		return false
	}
	sps, pps, idr := false, false, false
	for {
		nal, err := reader.NextNAL()
		if err != nil {
			break
		}
		switch nal.UnitType {
		case h264reader.NalUnitTypeSPS:
			sps = true
		case h264reader.NalUnitTypePPS:
			pps = true
		case h264reader.NalUnitTypeCodedSliceIdr:
			idr = true
		}
	}
	return sps && pps && idr
}
