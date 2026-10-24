package voice

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestMuteGapRecovery(t *testing.T) {
	for _, gate := range []struct {
		name, target string
		state        State
	}{
		{"publisher_mute", "publisher", State{Muted: true}},
		{"publisher_deafen", "publisher", State{Deafened: true}},
		{"listener_deafen", "existing", State{Deafened: true}},
	} {
		t.Run(gate.name, func(t *testing.T) {
			for _, gap := range []int{6000, 21000, 45000} {
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
					_, answer, err := m.Publish(ctx, "publisher", offer(publisher), map[string]string{"audio": track.ID()})
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
						ready := r.members["publisher"].sources["audio"] != nil
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
					subscribe := func(id string) <-chan uint16 {
						pc := peer()
						packets := make(chan uint16, 4096)
						pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
							workers.Add(1)
							go func() {
								defer workers.Done()
								for {
									packet, _, err := remote.ReadRTP()
									if err != nil {
										return
									}
									select {
									case packets <- packet.SequenceNumber:
									case <-ctx.Done():
										return
									}
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
					if len(existing) == 0 {
						t.Fatal("baseline microphone packets did not reach listener")
					}
					if err := m.SetState(gate.target, gate.state); err != nil {
						t.Fatal(err)
					}
					time.Sleep(100 * time.Millisecond)
					for len(existing) > 0 {
						<-existing
					}
					for n := 0; n < gap; n++ {
						write()
						if n%100 == 99 {
							time.Sleep(10 * time.Millisecond)
						}
					}
					time.Sleep(200 * time.Millisecond)
					if len(existing) != 0 {
						t.Fatal("microphone packets leaked during mute")
					}
					if err := m.SetState(gate.target, State{}); err != nil {
						t.Fatal(err)
					}
					for n := 0; n < 100; n++ {
						write()
						time.Sleep(5 * time.Millisecond)
					}
					beforeFresh := len(existing)
					fresh := subscribe("fresh")
					for n := 0; n < 100; n++ {
						write()
						time.Sleep(5 * time.Millisecond)
					}
					t.Logf("muted input packets=%d; existing listener after unmute=%d, after fresh join=%d; fresh listener=%d; publisher=%s", gap, beforeFresh, len(existing), len(fresh), publisher.ConnectionState())
					if len(fresh) == 0 {
						t.Fatal("fresh listener received no resumed microphone packets")
					}
					if beforeFresh == 0 {
						t.Fatal("existing listener did not recover after unmute")
					}
				})
			}

		})
	}
}
