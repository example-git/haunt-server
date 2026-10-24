package voice

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestMuteGapRecovery(t *testing.T) {
	for _, gate := range []struct {
		name, target, purpose string
		state                 State
		gaps                  []int
		blocked               bool
	}{
		{"publisher_mute", "publisher", "audio", State{Muted: true}, []int{6000, 21000, 45000}, true},
		{"publisher_deafen", "publisher", "audio", State{Deafened: true}, []int{6000, 21000, 45000}, true},
		{"listener_deafen", "existing", "audio", State{Deafened: true}, []int{6000, 21000, 45000}, true},
		{"listener_deafen_camera_audio", "existing", "camera_audio", State{Deafened: true}, []int{45000}, true},
		{"listener_deafen_screen_audio", "existing", "screen_audio", State{Deafened: true}, []int{45000}, true},
		{"publisher_mute_camera_audio", "publisher", "camera_audio", State{Muted: true}, []int{6000}, false},
		{"publisher_mute_screen_audio", "publisher", "screen_audio", State{Muted: true}, []int{6000}, false},
		{"publisher_deafen_camera_audio", "publisher", "camera_audio", State{Deafened: true}, []int{6000}, false},
		{"publisher_deafen_screen_audio", "publisher", "screen_audio", State{Deafened: true}, []int{6000}, false},
	} {
		t.Run(gate.name, func(t *testing.T) {
			for _, gap := range gate.gaps {
				t.Run(fmt.Sprintf("%d_packets", gap), func(t *testing.T) {
					m, err := New(config.Config{}, &identity.Service{})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if err := m.Close(ctx); err != nil {
							t.Error(err)
						}
					})
					r := &room{id: "mute-gap", members: map[string]*participant{}}
					m.rooms[r.id] = r
					for _, id := range []string{"publisher", "existing", "fresh"} {
						p := &participant{id: id, connection: id, room: r, send: func(any) bool { return true }, sources: map[string]*source{}, subscriptions: map[string]*subscription{}}
						r.members[id], m.participants[id] = p, p
					}
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					var workers sync.WaitGroup
					peers := []*webrtc.PeerConnection{}
					t.Cleanup(func() {
						cancel()
						for _, pc := range peers {
							_ = pc.Close()
						}
						workers.Wait()
					})
					peer := func() *webrtc.PeerConnection {
						pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
						if err != nil {
							t.Fatal(err)
						}
						peers = append(peers, pc)
						return pc
					}
					offer := func(pc *webrtc.PeerConnection) webrtc.SessionDescription {
						sdp, err := pc.CreateOffer(nil)
						if err != nil {
							t.Fatal(err)
						}
						ready := webrtc.GatheringCompletePromise(pc)
						if err := pc.SetLocalDescription(sdp); err != nil {
							t.Fatal(err)
						}
						select {
						case <-ready:
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
						return *pc.LocalDescription()
					}
					publisher := peer()
					track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "microphone", "microphone")
					if err != nil {
						t.Fatal(err)
					}
					sender, err := publisher.AddTrack(track)
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
					_, answer, err := m.Publish(ctx, "publisher", offer(publisher), map[string]string{gate.purpose: track.ID()})
					if err != nil {
						t.Fatal(err)
					}
					if err := publisher.SetRemoteDescription(answer); err != nil {
						t.Fatal(err)
					}
					sequence := uint16(25000)
					timestamp := uint32(0)
					write := func() {
						sequence++
						timestamp += 960
						if err := track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: timestamp}, Payload: []byte{0xf8, 0xff, 0xfe}}); err != nil {
							t.Fatal(err)
						}
					}
					for {
						write()
						r.mu.Lock()
						ready := r.members["publisher"].sources[gate.purpose] != nil
						r.mu.Unlock()
						if ready {
							break
						}
						select {
						case <-time.After(10 * time.Millisecond):
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
					}
					subscribe := func(id string) *atomic.Uint64 {
						pc := peer()
						packets := &atomic.Uint64{}
						pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
							workers.Add(1)
							go func() {
								defer workers.Done()
								for {
									_, _, err := remote.ReadRTP()
									if err != nil {
										return
									}
									packets.Add(1)
								}
							}()
						})
						if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
							t.Fatal(err)
						}
						_, answer, err := m.Subscribe(ctx, id, "publisher", offer(pc))
						if err != nil {
							t.Fatal(err)
						}
						if err := pc.SetRemoteDescription(answer); err != nil {
							t.Fatal(err)
						}
						return packets
					}
					existing := subscribe("existing")
					for n := 0; n < 100; n++ {
						write()
						time.Sleep(5 * time.Millisecond)
					}
					if existing.Load() == 0 {
						t.Fatal("baseline audio packets did not reach listener")
					}
					if err := m.SetState(gate.target, gate.state); err != nil {
						t.Fatal(err)
					}
					time.Sleep(100 * time.Millisecond)
					beforePause := existing.Load()
					for n := 0; n < gap; n++ {
						write()
						if n%100 == 99 {
							time.Sleep(10 * time.Millisecond)
						}
					}
					time.Sleep(200 * time.Millisecond)
					duringPause := existing.Load() - beforePause
					if gate.blocked && duringPause != 0 {
						t.Fatal("audio packets leaked during an enforced pause")
					}
					if !gate.blocked && duringPause == 0 {
						t.Fatal("publisher mute/deafen stopped independent share audio")
					}
					beforeResume := existing.Load()
					if err := m.SetState(gate.target, State{}); err != nil {
						t.Fatal(err)
					}
					for n := 0; n < 100; n++ {
						write()
						time.Sleep(5 * time.Millisecond)
					}
					beforeFresh := existing.Load() - beforeResume
					fresh := subscribe("fresh")
					for n := 0; n < 100; n++ {
						write()
						time.Sleep(5 * time.Millisecond)
					}
					t.Logf("input packets during pause=%d; received during pause=%d; existing listener after unmute=%d, after fresh join=%d; fresh listener=%d; publisher=%s", gap, duringPause, beforeFresh, existing.Load()-beforeResume, fresh.Load(), publisher.ConnectionState())
					if fresh.Load() == 0 {
						t.Fatal("fresh listener received no resumed audio packets")
					}
					if beforeFresh == 0 {
						t.Fatal("existing listener did not recover after unmute")
					}
				})
			}
		})
	}
}
