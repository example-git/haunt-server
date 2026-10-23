package voice

import (
	"errors"
	"strings"

	"github.com/example-git/haunt-server/internal/identity"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

var ErrQuality = errors.New("media_quality_unavailable")

type VideoLayer struct {
	ID     string  `json:"id"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	FPS    float64 `json:"fps"`
}

type layerSelection struct {
	active, wanted    string
	epoch             uint64
	started           bool
	sequence          uint16
	timestamp, offset uint32
	writtenEpoch      uint64
	fps               float64
}

func validateLayers(tracks map[string]string, layers map[string][]VideoLayer) error {
	if len(layers) > 2 {
		return identity.ErrInvalid
	}
	for purpose, values := range layers {
		if (purpose != "camera" && purpose != "screen") || tracks[purpose] == "" || len(values) < 1 || len(values) > 3 {
			return identity.ErrInvalid
		}
		seen := map[string]bool{}
		for _, layer := range values {
			if (layer.ID != "h" && layer.ID != "m" && layer.ID != "l") || seen[layer.ID] || layer.Width < 1 || layer.Width > 8192 || layer.Height < 1 || layer.Height > 8192 || layer.FPS <= 0 || layer.FPS > 1000 {
				return identity.ErrInvalid
			}
			seen[layer.ID] = true
		}
	}
	return nil
}

func (p *participant) availableLayers(purpose string) []VideoLayer {
	group := p.sources[purpose]
	if group == nil || len(group.layers) == 0 {
		return nil
	}
	result := []VideoLayer{}
	for _, layer := range p.videoLayers[purpose] {
		if group.layers[layer.ID] != nil {
			result = append(result, layer)
		}
	}
	return result
}

func (m *Manager) SubscriptionQuality(id, publisher, purpose, layer string) error {
	owner, err := m.get(id)
	if err != nil {
		return err
	}
	owner.room.mu.Lock()
	sub := owner.subscriptions[publisher]
	if sub == nil || owner.closed || sub.publisher.closed {
		owner.room.mu.Unlock()
		return identity.ErrNotFound
	}
	group := sub.publisher.sources[purpose]
	selection := sub.selections[purpose]
	if group == nil || selection == nil || layer == "" || group.layers[layer] == nil {
		owner.room.mu.Unlock()
		return ErrQuality
	}
	selection.wanted = layer
	src := group.layers[layer]
	owner.room.mu.Unlock()
	m.keyframe(sub.publisher, src)
	return nil
}

func (m *Manager) subscriptionKeyframe(sub *subscription, fallback *source) {
	sub.owner.room.mu.Lock()
	src := fallback
	if choice := sub.selections[fallback.purpose]; choice != nil {
		if group := sub.publisher.sources[fallback.purpose]; group != nil && group.layers[choice.wanted] != nil {
			src = group.layers[choice.wanted]
		}
	}
	sub.owner.room.mu.Unlock()
	m.keyframe(sub.publisher, src)
}

type videoFrame struct {
	packets   []*rtp.Packet
	timestamp uint32
	last      uint16
	bytes     int
	key       bool
	started   bool
	seen      bool
}

func videoBoundary(mime string, payload []byte) (bool, bool) {
	if len(payload) == 0 {
		return false, false
	}
	switch strings.ToLower(mime) {
	case "video/vp8":
		var p codecs.VP8Packet
		body, err := p.Unmarshal(payload)
		if err != nil || len(body) == 0 {
			return false, false
		}
		start := p.S == 1 && p.PID == 0
		return start, start && body[0]&1 == 0
	case "video/h264":
		kind := payload[0] & 31
		if kind == 28 && len(payload) >= 2 {
			return payload[1]&128 != 0, payload[1]&128 != 0 && payload[1]&31 == 5
		}
		if kind == 24 {
			key := false
			for offset := 1; offset+2 <= len(payload); {
				size := int(payload[offset])<<8 | int(payload[offset+1])
				offset += 2
				if size == 0 || offset+size > len(payload) {
					return false, false
				}
				key = key || payload[offset]&31 == 5
				offset += size
			}
			return true, key
		}
		return kind >= 1 && kind <= 23, kind == 5
	case "video/vp9":
		var p codecs.VP9Packet
		if _, err := p.Unmarshal(payload); err != nil {
			return false, false
		}
		return p.B, p.B && !p.P
	}
	return false, false
}

func (frame *videoFrame) push(packet *rtp.Packet, mime string) *videoFrame {
	start, key := videoBoundary(mime, packet.Payload)
	contiguous := !frame.seen || packet.SequenceNumber == frame.last+1
	if !frame.seen || frame.timestamp != packet.Timestamp {
		*frame = videoFrame{timestamp: packet.Timestamp, started: start && (contiguous || !strings.EqualFold(mime, "video/h264")), seen: true}
	} else if !contiguous {
		frame.started = false
		frame.packets = nil
	}
	frame.last = packet.SequenceNumber
	if !frame.started {
		return nil
	}
	frame.key = frame.key || key
	frame.bytes += len(packet.Payload)
	if frame.bytes > 4<<20 || len(frame.packets) >= 2048 {
		*frame = videoFrame{timestamp: packet.Timestamp, last: packet.SequenceNumber, seen: true}
		return nil
	}
	frame.packets = append(frame.packets, packet)
	if !packet.Marker {
		return nil
	}
	complete := *frame
	*frame = videoFrame{timestamp: packet.Timestamp, last: packet.SequenceNumber, seen: true}
	return &complete
}

func (m *Manager) forwardLayer(p *participant, src *source, frame *videoFrame) {
	p.room.mu.Lock()
	defer p.room.mu.Unlock()
	if p.closed || p.publish != src.peer || blockedSource(p.state, State{}, src.purpose) {
		return
	}
	for _, member := range p.room.members {
		sub := member.subscriptions[p.id]
		if sub == nil || member.closed {
			continue
		}
		selection := sub.selections[src.purpose]
		if selection == nil {
			continue
		}
		if selection.wanted == src.rid && selection.active != src.rid && frame.key {
			selection.active = src.rid
			selection.epoch++
			selection.fps = src.layer.FPS
			member.send(map[string]any{"type": "media.subscription.quality.changed", "publisher_id": p.id, "purpose": src.purpose, "layer": src.rid})
		}
		if selection.active != src.rid {
			continue
		}
		if cap(sub.packets)-len(sub.packets) < len(frame.packets) {
			m.drops.Add(uint64(len(frame.packets)))
			selection.active = ""
			selection.epoch++
			continue
		}
		for _, incoming := range frame.packets {
			out := packetMessage(src.purpose, incoming, sub.extensions[src.purpose])
			out.epoch = selection.epoch
			if strings.EqualFold(src.codec.MimeType, "video/vp8") {
				var descriptor codecs.VP8Packet
				if body, err := descriptor.Unmarshal(out.rtp.Payload); err == nil {
					out.rtp.Payload = append([]byte{out.rtp.Payload[0] & 0x3f}, body...)
				}
			}
			select {
			case sub.packets <- out:
			default:
				m.drops.Add(uint64(len(frame.packets)))
				selection.active = ""
				selection.epoch++
			}
			if selection.active == "" {
				break
			}
		}
	}
}

func (s *layerSelection) continuity(packet *rtp.Packet, epoch uint64) bool {
	if epoch != s.epoch {
		return false
	}
	if !s.started {
		s.started = true
		s.sequence = packet.SequenceNumber
		s.writtenEpoch = epoch
	} else {
		s.sequence++
		if s.writtenEpoch != epoch {
			step := uint32(3000)
			if s.fps > 0 {
				step = uint32(90000 / s.fps)
			}
			s.offset = s.timestamp + step - packet.Timestamp
			s.writtenEpoch = epoch
		}
	}
	packet.SequenceNumber = s.sequence
	packet.Timestamp += s.offset
	s.timestamp = packet.Timestamp
	return true
}
