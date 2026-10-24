package voice

import (
	"testing"

	"github.com/pion/rtp"
)

func TestAudioContinuityAcrossMute(t *testing.T) {
	var sequence audioSequence
	forward := func(input, expected uint16, timestamp uint32) {
		t.Helper()
		packet := &rtp.Packet{Header: rtp.Header{SequenceNumber: input, Timestamp: timestamp}}
		if !sequence.continuity(packet, sequence.epoch) || packet.SequenceNumber != expected || packet.Timestamp != timestamp {
			t.Fatalf("input=%d: got seq=%d timestamp=%d, want seq=%d timestamp=%d", input, packet.SequenceNumber, packet.Timestamp, expected, timestamp)
		}
	}
	forward(65534, 65534, 1000)
	sequence.pause()
	epoch := sequence.epoch
	sequence.pause()
	if sequence.epoch != epoch {
		t.Fatal("every blocked packet advanced the pause epoch")
	}
	if sequence.continuity(&rtp.Packet{}, epoch-1) {
		t.Fatal("queued packet from before mute was delivered")
	}
	forward(45000, 65535, 43201000)
	forward(45002, 1, 43202920)
	forward(45001, 0, 43201960)
	forward(45003, 2, 43203880)
	forward(45003, 2, 43203880)
	sequence.pause()
	forward(100, 3, 90000000)
	forward(101, 4, 90000960)
}

func TestAudioContinuityStartsAfterInitialMute(t *testing.T) {
	var sequence audioSequence
	sequence.pause()
	packet := &rtp.Packet{Header: rtp.Header{SequenceNumber: 40000, Timestamp: 960}}
	if !sequence.continuity(packet, sequence.epoch) || packet.SequenceNumber != 40000 {
		t.Fatal("initial mute changed the first transmitted sequence number")
	}
}
