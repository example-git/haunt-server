package voice

import "github.com/pion/rtp"

type audioSequence struct {
	epoch            uint64
	started, paused  bool
	sequence, offset uint16
}

func (s *audioSequence) pause() {
	if !s.paused {
		s.epoch++
		s.paused = true
	}
}

func (s *audioSequence) continuity(packet *rtp.Packet, epoch uint64) bool {
	if epoch != s.epoch {
		return false
	}
	if !s.started {
		s.started = true
		s.sequence = packet.SequenceNumber
	} else if s.paused {
		s.offset = packet.SequenceNumber - s.sequence - 1
	}
	s.paused = false
	packet.SequenceNumber -= s.offset
	if int16(packet.SequenceNumber-s.sequence) > 0 {
		s.sequence = packet.SequenceNumber
	}
	return true
}
