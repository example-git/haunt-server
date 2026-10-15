package voice

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/example-git/haunt-server/internal/identity"
)

const EncodedFrameLimit = 512 << 10
const relayQueueLimit = 2 << 20

// EncodedTrack describes a WebCodecs-compatible elementary stream. Description
// is decoder configuration, not a file/container. Forwarding never decodes it.
type EncodedTrack struct {
	Purpose     string `json:"purpose"`
	Codec       string `json:"codec"`
	Description []byte `json:"description,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	SampleRate  int    `json:"sample_rate,omitempty"`
	Channels    int    `json:"channels,omitempty"`
	Revision    uint32 `json:"revision,omitempty"`
}
type encodedState struct {
	EncodedTrack
	sequence uint32
	started  bool
}
type RelayFrame struct {
	Binary    bool
	Data      []byte
	publisher string
	purpose   int
	revision  uint32
}

// RelayPeer belongs to the existing authenticated voice participant. Room state
// is protected by the room lock. Queue accounting has its own short lock; no
// network I/O takes either lock.
type RelayPeer struct {
	m          *Manager
	p          *participant
	ctx        context.Context
	cancel     context.CancelFunc
	frames     chan RelayFrame
	mu         sync.Mutex
	bytes      int
	closed     bool
	tracks     [5]*encodedState
	seen       map[string][5]uint32
	keyframeAt [3]time.Time
	sourceID   []byte
}

func (m *Manager) UsesWebSocket() bool { return m.transport == "websocket" }

func (m *Manager) AttachRelay(ctx context.Context, principal identity.Principal, participantID string) (*RelayPeer, error) {
	if !m.UsesWebSocket() {
		return nil, identity.ErrNotFound
	}
	m.mu.Lock()
	var p *participant
	for _, member := range m.participants {
		if member.id == participantID && member.user == principal.User.ID && member.session == principal.SessionID {
			p = member
			break
		}
	}
	m.mu.Unlock()
	if p == nil {
		return nil, identity.ErrNotFound
	}
	checkCtx, done := context.WithTimeout(ctx, 5*time.Second)
	err := m.allowed(checkCtx, principal, p.room.id)
	done()
	if err != nil {
		return nil, err
	}
	peerCtx, cancel := context.WithCancel(ctx)
	id, _ := hex.DecodeString(strings.ReplaceAll(p.id, "-", ""))
	r := &RelayPeer{m: m, p: p, ctx: peerCtx, cancel: cancel, frames: make(chan RelayFrame, 64), seen: map[string][5]uint32{}, sourceID: id}
	p.room.mu.Lock()
	if p.closed || p.relay != nil {
		p.room.mu.Unlock()
		cancel()
		return nil, identity.ErrConflict
	}
	p.relay = r
	for _, member := range p.room.members {
		if other := member.relay; other != nil && other != r {
			r.sendTracks(other)
			other.requestKeyframe(1)
			other.requestKeyframe(2)
		}
	}
	p.room.mu.Unlock()
	return r, nil
}

func purposeIndex(purpose string) int {
	switch purpose {
	case "audio":
		return 0
	case "camera":
		return 1
	case "screen":
		return 2
	case "camera_audio":
		return 3
	case "screen_audio":
		return 4
	}
	return -1
}
func purposeName(index int) string { return trackPurposes[index] }
func (r *RelayPeer) trackList() []EncodedTrack {
	result := []EncodedTrack{}
	for _, track := range r.tracks {
		if track != nil {
			result = append(result, track.EncodedTrack)
		}
	}
	return result
}
func (r *RelayPeer) sendTracks(publisher *RelayPeer) {
	data, _ := json.Marshal(map[string]any{"type": "tracks", "participant_id": publisher.p.id, "tracks": publisher.trackList()})
	r.push(RelayFrame{Data: data})
}
func (r *RelayPeer) Configure(tracks []EncodedTrack) error {
	if len(tracks) > len(trackPurposes) {
		return identity.ErrInvalid
	}
	var prepared [5]*encodedState
	for _, track := range tracks {
		index := purposeIndex(track.Purpose)
		if index < 0 || prepared[index] != nil || len(track.Codec) < 1 || len(track.Codec) > 128 || len(track.Description) > 8192 {
			return identity.ErrInvalid
		}
		for _, char := range track.Codec {
			if char <= 32 || char > 126 {
				return identity.ErrInvalid
			}
		}
		if audioPurpose(track.Purpose) {
			if track.SampleRate < 8000 || track.SampleRate > 192000 || track.Channels < 1 || track.Channels > 8 || track.Width != 0 || track.Height != 0 {
				return identity.ErrInvalid
			}
		} else if track.Width < 1 || track.Width > 8192 || track.Height < 1 || track.Height > 8192 || track.Width*track.Height > 32_000_000 || track.SampleRate != 0 || track.Channels != 0 {
			return identity.ErrInvalid
		}
		track.Description = append([]byte{}, track.Description...)
		prepared[index] = &encodedState{EncodedTrack: track}
	}
	r.p.room.mu.Lock()
	if r.p.closed || r.p.relay != r || r.ctx.Err() != nil {
		r.p.room.mu.Unlock()
		return identity.ErrNotFound
	}
	r.p.relayRevision++
	if r.p.relayRevision == 0 {
		r.p.relayRevision++
	}
	for _, track := range prepared {
		if track != nil {
			track.Revision = r.p.relayRevision
		}
	}
	r.tracks = prepared
	for _, member := range r.p.room.members {
		if member.relay != nil {
			member.relay.sendTracks(r)
		}
	}
	r.requestKeyframe(1)
	r.requestKeyframe(2)
	v := snapshot(r.p)
	r.p.room.mu.Unlock()
	r.m.broadcast(r.p.room, map[string]any{"type": "media.tracks", "participant": v})
	return nil
}

// Input: 24-byte big-endian header followed by one encoded chunk. Version,
// purpose, flags, reserved, revision/u32, timestamp/us/u64, duration/us/u32,
// sequence/u32. Output prepends the authenticated publisher's 16 UUID bytes.
func (r *RelayPeer) Forward(data []byte) error {
	if len(data) <= 24 || len(data) > EncodedFrameLimit || data[0] != 1 || int(data[1]) >= len(trackPurposes) || data[2] > 1 || data[3] != 0 || binary.BigEndian.Uint64(data[8:16]) > (1<<53)-1 || binary.BigEndian.Uint32(data[16:20]) > 10_000_000 {
		return identity.ErrInvalid
	}
	purpose := int(data[1])
	if audioPurpose(purposeName(purpose)) && len(data) > 64<<10 {
		return identity.ErrInvalid
	}
	revision := binary.BigEndian.Uint32(data[4:8])
	sequence := binary.BigEndian.Uint32(data[20:24])
	r.p.room.mu.Lock()
	defer r.p.room.mu.Unlock()
	if r.p.closed || r.p.relay != r || r.ctx.Err() != nil {
		return identity.ErrNotFound
	}
	track := r.tracks[purpose]
	if track == nil || revision != track.Revision || (track.started && sequence != track.sequence+1) {
		return identity.ErrInvalid
	}
	track.sequence, track.started = sequence, true
	if blockedEncoded(r.p.state, State{}, purpose) {
		return nil
	}
	framed := make([]byte, 16+len(data))
	copy(framed, r.sourceID)
	copy(framed[16:], data)
	frame := RelayFrame{Binary: true, Data: framed, publisher: r.p.id, purpose: purpose, revision: revision}
	for _, member := range r.p.room.members {
		receiver := member.relay
		if receiver == nil || receiver == r || member.closed || blockedEncoded(r.p.state, member.state, purpose) {
			continue
		}
		seen := receiver.seen[r.p.id]
		if !audioPurpose(purposeName(purpose)) && seen[purpose] != revision {
			if data[2] != 1 {
				r.requestKeyframe(purpose)
				continue
			}
			seen[purpose] = revision
			receiver.seen[r.p.id] = seen
		}
		receiver.push(frame)
	}
	return nil
}
func blockedEncoded(publisher, receiver State, purpose int) bool {
	return blockedSource(publisher, receiver, purposeName(purpose))
}
func (r *RelayPeer) Deliverable(frame RelayFrame) bool {
	if !frame.Binary {
		return r.ctx.Err() == nil
	}
	r.p.room.mu.Lock()
	defer r.p.room.mu.Unlock()
	publisher := r.p.room.members[frame.publisher]
	if r.p.closed || r.p.relay != r || publisher == nil || publisher.closed || publisher.relay == nil || blockedEncoded(publisher.state, r.p.state, frame.purpose) {
		return false
	}
	track := publisher.relay.tracks[frame.purpose]
	return track != nil && track.Revision == frame.revision
}
func (r *RelayPeer) RequestKeyframe(publisherID, purpose string) error {
	index := purposeIndex(purpose)
	if index != 1 && index != 2 {
		return identity.ErrInvalid
	}
	r.p.room.mu.Lock()
	defer r.p.room.mu.Unlock()
	if r.p.closed || r.p.relay != r {
		return identity.ErrNotFound
	}
	publisher := r.p.room.members[publisherID]
	if publisher == nil || publisher.relay == nil || publisher.relay.tracks[index] == nil {
		return identity.ErrNotFound
	}
	seen := r.seen[publisherID]
	seen[index] = 0
	r.seen[publisherID] = seen
	publisher.relay.requestKeyframe(index)
	return nil
}
func (r *RelayPeer) requestKeyframe(index int) {
	if r.tracks[index] == nil || time.Since(r.keyframeAt[index]) < time.Second {
		return
	}
	r.keyframeAt[index] = time.Now()
	data, _ := json.Marshal(map[string]string{"type": "keyframe", "purpose": purposeName(index)})
	r.push(RelayFrame{Data: data})
}
func resetRelayVideo(p *participant, old State) {
	if p.relay == nil {
		return
	}
	for index, changed := range [3]bool{false, old.CameraOff != p.state.CameraOff, old.ScreenOff != p.state.ScreenOff} {
		if !changed {
			continue
		}
		for _, member := range p.room.members {
			if member.relay != nil {
				seen := member.relay.seen[p.id]
				seen[index] = 0
				member.relay.seen[p.id] = seen
			}
		}
		p.relay.keyframeAt[index] = time.Time{}
		p.relay.requestKeyframe(index)
	}
}

func (r *RelayPeer) push(frame RelayFrame) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil {
		return false
	}
	if len(frame.Data) <= relayQueueLimit-r.bytes {
		select {
		case r.frames <- frame:
			r.bytes += len(frame.Data)
			r.m.relayBytes.Add(int64(len(frame.Data)))
			return true
		default:
		}
	}
	r.m.relayOverflows.Add(1)
	r.cancel() // A dependent video stream cannot silently lose arbitrary chunks.
	return false
}
func (r *RelayPeer) Next(ctx context.Context) (RelayFrame, error) {
	select {
	case <-ctx.Done():
		return RelayFrame{}, ctx.Err()
	case <-r.ctx.Done():
		return RelayFrame{}, r.ctx.Err()
	case frame := <-r.frames:
		r.mu.Lock()
		if !r.closed {
			r.bytes -= len(frame.Data)
			r.m.relayBytes.Add(-int64(len(frame.Data)))
		}
		r.mu.Unlock()
		return frame, nil
	}
}
func (r *RelayPeer) Context() context.Context { return r.ctx }
func (r *RelayPeer) Close() {
	r.p.room.mu.Lock()
	current := r.p.relay == r
	if current {
		r.p.relay = nil
	}
	v := snapshot(r.p)
	closed := r.p.closed
	r.p.room.mu.Unlock()
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		r.cancel()
		r.m.relayBytes.Add(-int64(r.bytes))
		r.bytes = 0
		for len(r.frames) > 0 {
			select {
			case <-r.frames:
			default:
			}
		}
	}
	r.mu.Unlock()
	if current && !closed {
		r.m.broadcast(r.p.room, map[string]any{"type": "media.tracks", "participant": v})
	}
}
