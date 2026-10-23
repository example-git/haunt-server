package voice

import (
	"context"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"testing"
)

func TestLayerFrameRequiresCompleteKeyframe(t *testing.T) {
	packet := func(seq uint16, ts uint32, marker bool, payload ...byte) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: marker}, Payload: payload}
	}
	var frame videoFrame
	if frame.push(packet(1, 1, false, 0x7c, 0x85, 1), "video/H264") != nil {
		t.Fatal("incomplete keyframe")
	}
	if frame.push(packet(3, 1, false, 0x7c, 0x05, 2), "video/H264") != nil {
		t.Fatal("gap accepted")
	}
	if frame.push(packet(4, 1, true, 0x65, 3), "video/H264") != nil {
		t.Fatal("damaged timestamp restarted on another NAL")
	}
	frame.push(packet(5, 2, false, 0x7c, 0x85, 1), "video/H264")
	complete := frame.push(packet(6, 2, true, 0x7c, 0x45, 2), "video/H264")
	if complete == nil || !complete.key || len(complete.packets) != 2 {
		t.Fatal("complete keyframe lost")
	}
	if frame.push(packet(7, 2, true, 0x65, 1), "video/H264") != nil {
		t.Fatal("completed timestamp replayed")
	}
}

func TestLayerQueueOverflowInvalidatesQueuedFrames(t *testing.T) {
	r := &room{members: map[string]*participant{}}
	p := &participant{id: "publisher", room: r}
	choice := &layerSelection{active: "h", wanted: "h", epoch: 1}
	sub := &subscription{packets: make(chan packet, 1), selections: map[string]*layerSelection{"camera": choice}, extensions: map[string]map[uint8]uint8{}}
	r.members["viewer"] = &participant{subscriptions: map[string]*subscription{p.id: sub}}
	sub.packets <- packet{epoch: 1}
	m := &Manager{}
	m.forwardLayer(p, &source{rid: "h", purpose: "camera"}, &videoFrame{packets: []*rtp.Packet{{}}, key: false})
	queued := <-sub.packets
	if choice.active != "" || choice.continuity(&rtp.Packet{}, queued.epoch) {
		t.Fatal("overflow retained a damaged GOP")
	}
}

func TestLayerForwardingNeverBlocksBehindConcurrentAudio(t *testing.T) {
	r := &room{members: map[string]*participant{}}
	p := &participant{id: "publisher", room: r}
	choice := &layerSelection{active: "h", wanted: "h", epoch: 1}
	sub := &subscription{packets: make(chan packet, 2048), selections: map[string]*layerSelection{"camera": choice}, extensions: map[string]map[uint8]uint8{}}
	r.members["viewer"] = &participant{subscriptions: map[string]*subscription{p.id: sub}, send: func(any) bool { return true }}
	frame := &videoFrame{key: true, packets: make([]*rtp.Packet, 2048)}
	for i := range frame.packets {
		frame.packets[i] = &rtp.Packet{Payload: []byte{0x10, 0}}
	}
	m := &Manager{}
	src := &source{rid: "h", purpose: "camera", codec: webrtc.RTPCodecCapability{MimeType: "video/VP8"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case sub.packets <- packet{purpose: "audio"}:
			default:
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			for range len(sub.packets) {
				<-sub.packets
			}
			m.forwardLayer(p, src, frame)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		go func() {
			for {
				select {
				case <-done:
					return
				case <-sub.packets:
				}
			}
		}()
		t.Fatal("video queue blocked while holding the room lock")
	}
	cancel()
	workers.Wait()
}

func TestRestrictionLocksRetireAfterConcurrentUsers(t *testing.T) {
	m := &Manager{}
	var workers sync.WaitGroup
	inside := 0
	for range 40 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			unlock := m.lockRestrictions("channel")
			inside++
			if inside != 1 {
				t.Error("restriction operations overlapped")
			}
			inside--
			unlock()
		}()
	}
	workers.Wait()
	if len(m.restrictionLocks) != 0 {
		t.Fatal("idle channel locks were retained")
	}
}

func TestLayerContinuityDiscardsSupersededQueue(t *testing.T) {
	selection := layerSelection{epoch: 1, fps: 60}
	first := &rtp.Packet{Header: rtp.Header{SequenceNumber: 65535, Timestamp: 1000}}
	if !selection.continuity(first, 1) {
		t.Fatal("first packet lost")
	}
	selection.epoch = 2
	selection.fps = 15
	old := &rtp.Packet{Header: rtp.Header{SequenceNumber: 0, Timestamp: 2500}}
	if selection.continuity(old, 1) {
		t.Fatal("superseded queue delivered")
	}
	next := &rtp.Packet{Header: rtp.Header{SequenceNumber: 110, Timestamp: 900000}}
	if !selection.continuity(next, 2) || next.SequenceNumber != 0 || next.Timestamp != 7000 {
		t.Fatal("switch broke RTP continuity", next.Header)
	}
	more := &rtp.Packet{Header: rtp.Header{SequenceNumber: 111, Timestamp: 900000}}
	selection.continuity(more, 2)
	if more.Timestamp != next.Timestamp || more.SequenceNumber != 1 {
		t.Fatal("frame packet continuity")
	}
}

func TestH264LayerRejectsLostFirstNALOfKeyframe(t *testing.T) {
	var frame videoFrame
	packet := func(seq uint16, timestamp uint32) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: timestamp, Marker: true}, Payload: []byte{0x65, 0x80}}
	}
	if frame.push(packet(1, 1), "video/H264") == nil {
		t.Fatal("initial frame lost")
	}
	if frame.push(packet(3, 2), "video/H264") != nil {
		t.Fatal("keyframe missing its first NAL was accepted")
	}
	if frame.push(packet(4, 3), "video/H264") == nil {
		t.Fatal("next intact keyframe did not recover")
	}
}
