// Package voice forwards encoded RTP for audio, camera, and screen tracks.
// It does not render, decode, transcode, or mix media. Source codec parameters
// come from negotiation and must match each receiver's offered capabilities.
package voice

import (
	"context"
	"errors"
	"github.com/pion/sdp/v3"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/turnclient"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var ErrCapacity = errors.New("media_capacity_reached")

type Send func(any) bool
type Admission struct {
	ID            string             `json:"admission_id"`
	RoomID        string             `json:"room_id"`
	ExpiresAt     time.Time          `json:"expires_at"`
	ICE           []config.ICEServer `json:"ice_servers"`
	ICEExpiresAt  *time.Time         `json:"ice_expires_at,omitempty"`
	Transport     string             `json:"transport"`
	ICEPolicy     string             `json:"ice_transport_policy,omitempty"`
	MediaPath     string             `json:"media_path,omitempty"`
	session, user string
}
type State struct {
	Muted     bool `json:"muted"`
	Deafened  bool `json:"deafened"`
	CameraOff bool `json:"camera_off"`
	ScreenOff bool `json:"screen_off"`
}
type Track struct {
	Purpose string                    `json:"purpose"`
	ID      string                    `json:"track_id"`
	Codec   webrtc.RTPCodecCapability `json:"codec"`
	Layers  []VideoLayer              `json:"layers,omitempty"`
}
type Snapshot struct {
	ID            string                    `json:"participant_id"`
	UserID        string                    `json:"user_id"`
	JoinedAt      time.Time                 `json:"joined_at"`
	State         State                     `json:"state"`
	SelfState     State                     `json:"self_state,omitempty"`
	Restriction   identity.VoiceRestriction `json:"restriction,omitempty"`
	Tracks        []Track                   `json:"tracks"`
	EncodedTracks []EncodedTrack            `json:"encoded_tracks,omitempty"`
}
type room struct {
	mu      sync.Mutex
	id      string
	members map[string]*participant
}
type source struct {
	rid        string
	layer      VideoLayer
	layers     map[string]*source
	purpose    string
	codec      webrtc.RTPCodecCapability
	ssrc       uint32
	peer       *webrtc.PeerConnection
	extensions []webrtc.RTPHeaderExtensionParameter
}
type packet struct {
	epoch   uint64
	purpose string
	rtp     *rtp.Packet
}
type participant struct {
	id, user, session string
	connection        string
	joinedAt          time.Time
	room              *room
	send              Send
	state             State
	self              State
	restriction       identity.VoiceRestriction
	publish           *webrtc.PeerConnection
	publishID         string
	trackIDs          map[string]string
	videoLayers       map[string][]VideoLayer
	sources           map[string]*source
	subscriptions     map[string]*subscription
	closed            bool
	relay             *RelayPeer
	relayRevision     uint32
	iceExpires        time.Time
	iceNotified       bool
}
type subscription struct {
	id               string
	owner, publisher *participant
	pc               *webrtc.PeerConnection
	tracks           map[string]*webrtc.TrackLocalStaticRTP
	selections       map[string]*layerSelection
	extensions       map[string]map[uint8]uint8
	packets          chan packet
	ctx              context.Context
	cancel           context.CancelFunc
}
type Manager struct {
	ids                 *identity.Service
	api                 *webrtc.API
	ice                 []webrtc.ICEServer
	publicICE           []config.ICEServer
	roomLimit, maxPeers int
	mu                  sync.Mutex
	closed              bool
	rooms               map[string]*room
	participants        map[string]*participant
	admissions          map[string]Admission
	wg                  sync.WaitGroup
	peers               atomic.Int64
	drops               atomic.Uint64
	transport           string
	relayOnly           bool
	relayBytes          atomic.Int64
	relayOverflows      atomic.Uint64
	iceSource           turnclient.Source
	restrictionLocks    map[string]*restrictionGuard
}

func New(c config.Config, ids *identity.Service, sources ...turnclient.Source) (*Manager, error) {
	media := &webrtc.MediaEngine{}
	// Declare full-band stereo receive capability before registering the other
	// defaults. This relay forwards Opus unchanged; it imposes no speech-rate cap.
	if err := media.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;maxaveragebitrate=510000;maxplaybackrate=48000",
		}, PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := media.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	for _, uri := range []string{sdp.SDESMidURI, sdp.SDESRTPStreamIDURI, sdp.SDESRepairRTPStreamIDURI} {
		if err := media.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: uri}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(media, registry); err != nil {
		return nil, err
	}
	settings := webrtc.SettingEngine{}
	if c.MediaPublicIP != "" {
		settings.SetNAT1To1IPs([]string{c.MediaPublicIP}, webrtc.ICECandidateTypeHost)
	}
	if c.MediaPortMin != 0 {
		if err := settings.SetEphemeralUDPPortRange(c.MediaPortMin, c.MediaPortMax); err != nil {
			return nil, err
		}
	}
	if c.MediaRoomLimit == 0 {
		c.MediaRoomLimit = 16
	}
	if c.MediaMaxPeers == 0 {
		c.MediaMaxPeers = 512
	}
	m := &Manager{ids: ids, api: webrtc.NewAPI(webrtc.WithMediaEngine(media), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(settings)), publicICE: c.ICE, roomLimit: c.MediaRoomLimit, maxPeers: c.MediaMaxPeers, rooms: map[string]*room{}, participants: map[string]*participant{}, admissions: map[string]Admission{}}
	for _, server := range c.ICE {
		m.ice = append(m.ice, webrtc.ICEServer{URLs: server.URLs, Username: server.Username, Credential: server.Credential})
	}
	m.transport = c.MediaTransport()
	m.relayOnly = c.MediaRelayOnly
	if len(sources) > 0 {
		m.iceSource = sources[0]
	}
	return m, nil
}

func (m *Manager) ConnectionICE(ctx context.Context, p identity.Principal) (turnclient.Credentials, error) {
	result := turnclient.Credentials{Servers: append([]config.ICEServer{}, m.publicICE...)}
	if m.iceSource != nil {
		client, err := m.iceSource.Get(ctx, p.SessionID)
		if err != nil {
			return turnclient.Credentials{}, err
		}
		server, err := m.iceSource.Get(ctx, "server")
		if err != nil {
			return turnclient.Credentials{}, err
		}
		result.Servers = append(result.Servers, client.Servers...)
		result.ExpiresAt = client.ExpiresAt
		if server.ExpiresAt.Before(result.ExpiresAt) {
			result.ExpiresAt = server.ExpiresAt
		}
	}
	return result, nil
}
func (m *Manager) ICEPolicy() string {
	if m.relayOnly {
		return "relay"
	}
	return "all"
}
func (m *Manager) allowed(ctx context.Context, p identity.Principal, id string) error {
	ch, err := m.ids.Channel(ctx, p, id)
	if err != nil {
		return err
	}
	if ch.Kind != "voice" {
		return identity.ErrInvalid
	}
	if ch.Permissions&(identity.ViewChannel|identity.ConnectVoice) != (identity.ViewChannel | identity.ConnectVoice) {
		return identity.ErrForbidden
	}
	return nil
}
func (m *Manager) Admit(ctx context.Context, p identity.Principal, roomID string) (Admission, error) {
	if err := m.allowed(ctx, p, roomID); err != nil {
		return Admission{}, err
	}
	credentials, err := m.ConnectionICE(ctx, p)
	if err != nil {
		return Admission{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Admission{}, ErrCapacity
	}
	active := 0
	for id, a := range m.admissions {
		if time.Now().After(a.ExpiresAt) {
			delete(m.admissions, id)
		} else if a.session == p.SessionID {
			active++
		}
	}
	if active >= 4 || len(m.admissions) >= 1024 {
		return Admission{}, ErrCapacity
	}
	a := Admission{ID: identity.NewID(), RoomID: roomID, ExpiresAt: time.Now().Add(time.Minute), ICE: credentials.Servers, session: p.SessionID, user: p.User.ID}
	if !credentials.ExpiresAt.IsZero() {
		a.ICEExpiresAt = &credentials.ExpiresAt
	}
	a.Transport = m.transport
	if m.transport == "websocket" {
		a.ICE = nil
		a.MediaPath = "/api/v1/media"
	} else {
		a.ICEPolicy = "all"
		if m.relayOnly {
			a.ICEPolicy = "relay"
		}
	}
	m.admissions[a.ID] = a
	return a, nil
}
func (m *Manager) Join(ctx context.Context, p identity.Principal, connectionID, admissionID string, send Send) (Snapshot, error) {
	m.mu.Lock()
	a, exists := m.admissions[admissionID]
	m.mu.Unlock()
	if !exists || a.session != p.SessionID || a.user != p.User.ID || time.Now().After(a.ExpiresAt) {
		return Snapshot{}, identity.ErrForbidden
	}
	if err := m.allowed(ctx, p, a.RoomID); err != nil {
		return Snapshot{}, err
	}
	unlock := m.lockRestrictions(a.RoomID)
	defer unlock()
	restriction, err := m.ids.VoiceRestriction(ctx, a.RoomID, p.User.ID)
	if err != nil {
		return Snapshot{}, err
	}
	m.mu.Lock()
	if m.closed || m.participants[connectionID] != nil {
		m.mu.Unlock()
		return Snapshot{}, identity.ErrConflict
	}
	a, exists = m.admissions[admissionID]
	if !exists {
		m.mu.Unlock()
		return Snapshot{}, identity.ErrForbidden
	}
	delete(m.admissions, admissionID)
	r := m.rooms[a.RoomID]
	if r == nil {
		if len(m.rooms) >= 64 {
			m.mu.Unlock()
			return Snapshot{}, ErrCapacity
		}
		r = &room{id: a.RoomID, members: map[string]*participant{}}
		m.rooms[a.RoomID] = r
	}
	r.mu.Lock()
	if len(r.members) >= m.ids.RoomLimit(m.roomLimit) {
		r.mu.Unlock()
		m.mu.Unlock()
		return Snapshot{}, ErrCapacity
	}
	member := &participant{id: identity.NewID(), joinedAt: time.Now().UTC(), connection: connectionID, user: p.User.ID, session: p.SessionID, room: r, send: send, trackIDs: map[string]string{}, sources: map[string]*source{}, subscriptions: map[string]*subscription{}}
	// No video sources exist at admission. Advertising them as active produces
	// false camera/screen stop events when the client sends its initial state.
	member.self = State{CameraOff: true, ScreenOff: true}
	member.restriction = restriction
	member.state = effectiveState(member.self, restriction)
	if a.ICEExpiresAt != nil {
		member.iceExpires = *a.ICEExpiresAt
	}
	r.members[member.id] = member
	m.participants[connectionID] = member
	v := snapshot(member)
	r.mu.Unlock()
	m.mu.Unlock()
	m.broadcast(r, map[string]any{"type": "media.participant.joined", "room_id": r.id, "participant": v})
	return v, nil
}
func snapshot(p *participant) Snapshot {
	v := Snapshot{ID: p.id, UserID: p.user, JoinedAt: p.joinedAt, State: p.state, SelfState: p.self, Restriction: p.restriction, Tracks: []Track{}}
	if p.relay != nil {
		v.EncodedTracks = p.relay.trackList()
	}
	for _, purpose := range trackPurposes {
		if src := p.sources[purpose]; src != nil {
			v.Tracks = append(v.Tracks, Track{Purpose: purpose, ID: p.id + "_" + purpose, Codec: src.codec, Layers: p.availableLayers(purpose)})
		}
	}
	return v
}
func (m *Manager) broadcast(r *room, value any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if event, ok := value.(map[string]any); ok {
		if before, ok := event["participant"].(Snapshot); ok {
			current := r.members[before.ID]
			if current == nil || current.closed {
				return
			}
			event["participant"] = snapshot(current)
		}
	}
	for _, p := range r.members {
		p.send(value)
	}
}
func (m *Manager) get(id string) (*participant, error) {
	m.mu.Lock()
	p := m.participants[id]
	m.mu.Unlock()
	if p == nil {
		return nil, identity.ErrNotFound
	}
	return p, nil
}
func (m *Manager) Validate(ctx context.Context, p identity.Principal, id string) error {
	member, err := m.get(id)
	if errors.Is(err, identity.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if member.session != p.SessionID {
		return identity.ErrForbidden
	}
	if err = m.allowed(ctx, p, member.room.id); err != nil {
		m.Leave(id)
		return err
	}
	member.room.mu.Lock()
	expired := !member.iceExpires.IsZero() && time.Now().After(member.iceExpires)
	notify := !member.iceExpires.IsZero() && !member.iceNotified && time.Until(member.iceExpires) < 2*time.Minute
	if notify {
		member.iceNotified = true
	}
	member.room.mu.Unlock()
	if expired {
		m.Leave(id)
		return identity.ErrUnauthorized
	}
	if notify {
		member.send(map[string]any{"type": "media.reconnect_required", "reason": "ice_credentials_expiring", "expires_at": member.iceExpires})
	}
	return nil
}
func (m *Manager) Room(ctx context.Context, p identity.Principal, id string) ([]Snapshot, error) {
	if err := m.allowed(ctx, p, id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	r := m.rooms[id]
	m.mu.Unlock()
	items := []Snapshot{}
	if r == nil {
		return items, nil
	}
	r.mu.Lock()
	for _, member := range r.members {
		items = append(items, snapshot(member))
	}
	r.mu.Unlock()
	return items, nil
}
func (m *Manager) SetState(id string, state State) error {
	p, err := m.get(id)
	if err != nil {
		return err
	}
	p.room.mu.Lock()
	if p.closed {
		p.room.mu.Unlock()
		return identity.ErrNotFound
	}
	previous := p.state
	p.self = state
	p.state = effectiveState(state, p.restriction)
	resetRelayVideo(p, previous)
	v := snapshot(p)
	p.room.mu.Unlock()
	m.broadcast(p.room, map[string]any{"type": "media.participant.state", "participant": v})
	return nil
}

func (m *Manager) newPeer(contexts ...context.Context) (*webrtc.PeerConnection, error) {
	if m.transport == "websocket" {
		return nil, identity.ErrInvalid
	}
	for {
		count := m.peers.Load()
		if count >= int64(m.ids.PeerLimit(m.maxPeers)) {
			return nil, ErrCapacity
		}
		if m.peers.CompareAndSwap(count, count+1) {
			break
		}
	}
	configuration := webrtc.Configuration{ICEServers: append([]webrtc.ICEServer{}, m.ice...)}
	if m.iceSource != nil {
		ctx := context.Background()
		if len(contexts) > 0 {
			ctx = contexts[0]
		}
		credentials, err := m.iceSource.Get(ctx, "server")
		if err != nil {
			m.peers.Add(-1)
			return nil, err
		}
		for _, server := range credentials.Servers {
			configuration.ICEServers = append(configuration.ICEServers, webrtc.ICEServer{URLs: server.URLs, Username: server.Username, Credential: server.Credential})
		}
	}
	if m.relayOnly {
		configuration.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	pc, err := m.api.NewPeerConnection(configuration)
	if err != nil {
		m.peers.Add(-1)
		return nil, err
	}
	var once sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateClosed {
			once.Do(func() { m.peers.Add(-1) })
		}
	})
	return pc, nil
}
func (m *Manager) worker(fn func()) bool {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() { defer m.wg.Done(); fn() }()
	return true
}

// Bound the peer's resources before Pion allocates transceivers or SCTP state.
func validateOffer(offer webrtc.SessionDescription, layered ...bool) error {
	if offer.Type != webrtc.SDPTypeOffer || len(offer.SDP) > 96<<10 {
		return identity.ErrInvalid
	}
	var description sdp.SessionDescription
	if description.Unmarshal([]byte(offer.SDP)) != nil || len(description.MediaDescriptions) < 1 || len(description.MediaDescriptions) > len(trackPurposes) {
		return identity.ErrInvalid
	}
	for _, media := range description.MediaDescriptions {
		if media.MediaName.Media != "audio" && media.MediaName.Media != "video" {
			return identity.ErrInvalid
		}
		rids := map[string]bool{}
		for _, attribute := range media.Attributes {
			if attribute.Key == "simulcast" || attribute.Key == "rid" {
				if len(layered) == 0 || !layered[0] || media.MediaName.Media != "video" {
					return identity.ErrInvalid
				}
				if attribute.Key == "rid" {
					fields := strings.Fields(attribute.Value)
					if len(fields) < 2 || fields[1] != "send" || (fields[0] != "h" && fields[0] != "m" && fields[0] != "l") || rids[fields[0]] {
						return identity.ErrInvalid
					}
					rids[fields[0]] = true
				}
			}
		}
	}
	return nil
}
func answer(ctx context.Context, pc *webrtc.PeerConnection, offer webrtc.SessionDescription, layered ...bool) (webrtc.SessionDescription, error) {
	if err := validateOffer(offer, layered...); err != nil {
		return webrtc.SessionDescription{}, err
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, identity.ErrInvalid
	}
	result, err := pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(result); err != nil {
		return webrtc.SessionDescription{}, err
	}
	select {
	case <-gathered:
		if pc.GetConfiguration().ICETransportPolicy == webrtc.ICETransportPolicyRelay && !strings.Contains(pc.LocalDescription().SDP, " typ relay") {
			return webrtc.SessionDescription{}, turnclient.ErrUnavailable
		}
		return *pc.LocalDescription(), nil
	case <-ctx.Done():
		return webrtc.SessionDescription{}, ctx.Err()
	}
}
func (m *Manager) Publish(ctx context.Context, id string, offer webrtc.SessionDescription, tracks map[string]string, publishedLayers ...map[string][]VideoLayer) (string, webrtc.SessionDescription, error) {
	p, err := m.get(id)
	if err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	if len(tracks) < 1 || len(tracks) > len(trackPurposes) {
		return "", webrtc.SessionDescription{}, identity.ErrInvalid
	}
	layers := map[string][]VideoLayer{}
	if len(publishedLayers) > 0 {
		if err := validateLayers(tracks, publishedLayers[0]); err != nil {
			return "", webrtc.SessionDescription{}, err
		}
		for purpose, values := range publishedLayers[0] {
			layers[purpose] = append([]VideoLayer{}, values...)
			sort.Slice(layers[purpose], func(i, j int) bool {
				a, b := layers[purpose][i], layers[purpose][j]
				return a.Width*a.Height > b.Width*b.Height || (a.Width*a.Height == b.Width*b.Height && a.FPS > b.FPS)
			})
		}
	}
	trackIDs := map[string]string{}
	for purpose, trackID := range tracks {
		if purposeIndex(purpose) < 0 || len(trackID) < 1 || len(trackID) > 128 || trackIDs[trackID] != "" {
			return "", webrtc.SessionDescription{}, identity.ErrInvalid
		}
		trackIDs[trackID] = purpose
	}
	if err := validateOffer(offer, len(layers) > 0); err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	peerID := identity.NewID()
	pc, err := m.newPeer(ctx)
	if err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	p.room.mu.Lock()
	if p.closed {
		p.room.mu.Unlock()
		pc.Close()
		return "", webrtc.SessionDescription{}, identity.ErrConflict
	}
	previous := p.publish
	oldSubscriptions := []*subscription{}
	for _, member := range p.room.members {
		if sub := member.subscriptions[p.id]; sub != nil {
			oldSubscriptions = append(oldSubscriptions, sub)
			delete(member.subscriptions, p.id)
		}
	}
	p.publish = pc
	p.publishID = peerID
	p.trackIDs = trackIDs
	p.videoLayers = layers
	p.sources = map[string]*source{}
	initial := snapshot(p)
	p.room.mu.Unlock()
	for _, sub := range oldSubscriptions {
		sub.cancel()
		sub.pc.Close()
	}
	if previous != nil {
		previous.Close()
		m.broadcast(p.room, map[string]any{"type": "media.tracks", "participant": initial})
	}
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected {
			m.worker(func() { m.leave(id, p, pc) })
		}
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		purpose := trackIDs[remote.ID()]
		codec := remote.Codec().RTPCodecCapability
		if purpose == "" || (audioPurpose(purpose) && remote.Kind() != webrtc.RTPCodecTypeAudio) || (!audioPurpose(purpose) && remote.Kind() != webrtc.RTPCodecTypeVideo) {
			m.worker(func() { m.leave(id, p, pc) })
			return
		}
		// No decode/re-encode: retain the negotiated MIME, clock, channels,
		// fmtp, and feedback. Auxiliary RTX is handled by Pion's interceptors.
		src := &source{purpose: purpose, codec: codec, ssrc: uint32(remote.SSRC()), peer: pc, extensions: receiver.GetParameters().HeaderExtensions}
		rid := remote.RID()
		if rid == "" && len(layers[purpose]) == 1 {
			rid = layers[purpose][0].ID
		}
		if rid != "" {
			for _, layer := range layers[purpose] {
				if layer.ID == rid {
					src.rid = rid
					src.layer = layer
					break
				}
			}
			if src.rid == "" {
				m.worker(func() { m.leave(id, p, pc) })
				return
			}
		} else if len(layers[purpose]) > 0 {
			m.worker(func() { m.leave(id, p, pc) })
			return
		}
		p.room.mu.Lock()
		group := p.sources[purpose]
		if p.closed || p.publish != pc || (group != nil && (src.rid == "" || group.layers[src.rid] != nil || !codecEqual(group.codec, src.codec))) {
			p.room.mu.Unlock()
			m.worker(func() { m.leave(id, p, pc) })
			return
		}
		if group == nil {
			p.sources[purpose] = src
			group = src
		}
		if src.rid != "" {
			if group.layers == nil {
				group.layers = map[string]*source{}
			}
			group.layers[src.rid] = src
		}
		v := snapshot(p)
		p.room.mu.Unlock()
		m.broadcast(p.room, map[string]any{"type": "media.tracks", "participant": v})
		if !m.worker(func() { m.forward(p, src, remote) }) {
			pc.Close()
		}
	})
	result, err := answer(ctx, pc, offer, len(layers) > 0)
	if err != nil {
		m.leave(id, p, pc)
		return "", webrtc.SessionDescription{}, err
	}
	return peerID, result, nil
}
func codecEqual(a, b webrtc.RTPCodecCapability) bool {
	return strings.EqualFold(a.MimeType, b.MimeType) && a.ClockRate == b.ClockRate && a.Channels == b.Channels && normalizedFMTP(a.SDPFmtpLine) == normalizedFMTP(b.SDPFmtpLine)
}
func normalizedFMTP(line string) string {
	values := []string{}
	for _, value := range strings.Split(line, ";") {
		key, v, ok := strings.Cut(strings.TrimSpace(value), "=")
		if key == "" {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if ok {
			key += "=" + strings.TrimSpace(v)
		}
		values = append(values, key)
	}
	sort.Strings(values)
	return strings.Join(values, ";")
}

func (m *Manager) forward(p *participant, src *source, remote *webrtc.TrackRemote) {
	defer func() {
		p.room.mu.Lock()
		if group := p.sources[src.purpose]; src.rid != "" && group != nil && group.layers[src.rid] == src {
			delete(group.layers, src.rid)
			if len(group.layers) == 0 {
				delete(p.sources, src.purpose)
			}
		} else if src.rid == "" && p.sources[src.purpose] == src {
			delete(p.sources, src.purpose)
		}
		v := snapshot(p)
		closed := p.closed
		p.room.mu.Unlock()
		if !closed {
			m.broadcast(p.room, map[string]any{"type": "media.tracks", "participant": v})
		}
	}()
	frame := videoFrame{}
	for {
		packet, _, err := remote.ReadRTP()
		if err != nil {
			return
		}
		if !codecEqual(remote.Codec().RTPCodecCapability, src.codec) {
			m.leave(p.connection, p, src.peer)
			return
		}
		if src.rid != "" {
			if complete := frame.push(packet, src.codec.MimeType); complete != nil {
				m.forwardLayer(p, src, complete)
			}
			continue
		}
		p.room.mu.Lock()
		receivers := []*subscription{}
		blocked := p.closed || p.publish != src.peer || blockedSource(p.state, State{}, src.purpose)
		if !blocked {
			for _, member := range p.room.members {
				if member.closed || (audioPurpose(src.purpose) && member.state.Deafened) {
					continue
				}
				if sub := member.subscriptions[p.id]; sub != nil && sub.tracks[src.purpose] != nil {
					receivers = append(receivers, sub)
				}
			}
		}
		p.room.mu.Unlock()
		for _, sub := range receivers {
			select {
			case sub.packets <- packetMessage(src.purpose, packet, sub.extensions[src.purpose]):
			case <-sub.ctx.Done():
			default:
				m.drops.Add(1)
			}
		}
	}
}
func packetMessage(purpose string, p *rtp.Packet, mapping map[uint8]uint8) packet {
	copy := p.Clone()
	copy.Extension = false
	copy.ExtensionProfile = 0
	copy.Extensions = nil
	for sourceID, targetID := range mapping {
		if value := p.GetExtension(sourceID); value != nil {
			_ = copy.SetExtension(targetID, append([]byte{}, value...))
		}
	}
	return packet{purpose: purpose, rtp: copy}
}
func (m *Manager) Subscribe(ctx context.Context, id, publisherID string, offer webrtc.SessionDescription, initial ...map[string]string) (string, webrtc.SessionDescription, error) {
	owner, err := m.get(id)
	if err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	if publisherID == owner.id {
		return "", webrtc.SessionDescription{}, identity.ErrInvalid
	}
	owner.room.mu.Lock()
	publisher := owner.room.members[publisherID]
	sources := []*source{}
	if publisher != nil {
		for _, purpose := range trackPurposes {
			if src := publisher.sources[purpose]; src != nil {
				sources = append(sources, src)
			}
		}
	}
	owner.room.mu.Unlock()
	if publisher == nil || len(sources) == 0 {
		return "", webrtc.SessionDescription{}, identity.ErrNotFound
	}
	pc, err := m.newPeer(ctx)
	if err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	subCtx, cancel := context.WithCancel(context.Background())
	sub := &subscription{id: identity.NewID(), owner: owner, publisher: publisher, pc: pc, tracks: map[string]*webrtc.TrackLocalStaticRTP{}, selections: map[string]*layerSelection{}, extensions: map[string]map[uint8]uint8{}, packets: make(chan packet, 2048), ctx: subCtx, cancel: cancel}
	success := false
	defer func() {
		if !success {
			cancel()
			pc.Close()
		}
	}()
	for _, src := range sources {
		local, err := webrtc.NewTrackLocalStaticRTP(src.codec, publisherID+"_"+src.purpose, publisherID)
		if err != nil {
			return "", webrtc.SessionDescription{}, err
		}
		sender, err := pc.AddTrack(local)
		if err != nil {
			return "", webrtc.SessionDescription{}, err
		}
		sub.tracks[src.purpose] = local
		if !m.worker(func() {
			for {
				packets, _, err := sender.ReadRTCP()
				if err != nil {
					return
				}
				for _, packet := range packets {
					switch packet.(type) {
					case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
						m.subscriptionKeyframe(sub, src)
					}
				}
			}
		}) {
			return "", webrtc.SessionDescription{}, ErrCapacity
		}
	}
	description, err := answer(ctx, pc, offer)
	if err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	// Require codec compatibility for every source; an SFU cannot quietly
	// convert a codec unsupported by the subscriber's SDP offer.
	for _, sender := range pc.GetSenders() {
		if sender.Track() != nil {
			parameters := sender.GetParameters()
			found := false
			var actual *source
			for _, src := range sources {
				if sub.tracks[src.purpose] == sender.Track() {
					actual = src
					break
				}
			}
			if actual == nil {
				return "", webrtc.SessionDescription{}, identity.ErrInvalid
			}
			for _, codec := range parameters.Codecs {
				if relayCodecCompatible(codec.RTPCodecCapability, actual.codec) {
					found = true
				}
			}
			if !found {
				return "", webrtc.SessionDescription{}, identity.ErrInvalid
			}
			mapping := map[uint8]uint8{}
			for _, incoming := range actual.extensions {
				if strings.Contains(incoming.URI, "sdes:") || strings.Contains(incoming.URI, "transport-wide-cc") || strings.Contains(incoming.URI, "abs-send-time") {
					continue
				}
				for _, outgoing := range parameters.HeaderExtensions {
					if incoming.URI == outgoing.URI && incoming.ID > 0 && incoming.ID < 256 && outgoing.ID > 0 && outgoing.ID < 256 {
						mapping[uint8(incoming.ID)] = uint8(outgoing.ID)
					}
				}
			}
			sub.extensions[actual.purpose] = mapping
		}
	}
	owner.room.mu.Lock()
	if owner.closed || publisher.closed {
		owner.room.mu.Unlock()
		return "", webrtc.SessionDescription{}, identity.ErrNotFound
	}
	for _, src := range sources {
		if publisher.sources[src.purpose] != src {
			owner.room.mu.Unlock()
			return "", webrtc.SessionDescription{}, identity.ErrConflict
		}
		available := publisher.availableLayers(src.purpose)
		requested := ""
		if len(initial) > 0 {
			requested = initial[0][src.purpose]
		}
		if len(available) > 0 {
			if requested == "" {
				requested = available[0].ID
			}
			if src.layers[requested] == nil {
				owner.room.mu.Unlock()
				return "", webrtc.SessionDescription{}, ErrQuality
			}
			sub.selections[src.purpose] = &layerSelection{wanted: requested}
		} else if requested != "" {
			owner.room.mu.Unlock()
			return "", webrtc.SessionDescription{}, ErrQuality
		}
	}
	if len(initial) > 0 {
		for purpose := range initial[0] {
			if sub.selections[purpose] == nil {
				owner.room.mu.Unlock()
				return "", webrtc.SessionDescription{}, ErrQuality
			}
		}
	}
	previous := owner.subscriptions[publisherID]
	owner.subscriptions[publisherID] = sub
	owner.room.mu.Unlock()
	if previous != nil {
		previous.cancel()
		previous.pc.Close()
	}
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected {
			m.worker(func() { m.dropSubscription(sub) })
		}
	})
	if !m.worker(func() {
		defer cancel()
		for {
			select {
			case <-subCtx.Done():
				return
			case packet := <-sub.packets:
				owner.room.mu.Lock()
				blocked := owner.closed || publisher.closed || blockedSource(publisher.state, owner.state, packet.purpose)
				if selection := sub.selections[packet.purpose]; !blocked && selection != nil {
					blocked = !selection.continuity(packet.rtp, packet.epoch)
				}
				owner.room.mu.Unlock()
				if !blocked {
					if err := sub.tracks[packet.purpose].WriteRTP(packet.rtp); err != nil {
						m.dropSubscription(sub)
						return
					}
				}
			}
		}
	}) {
		m.dropSubscription(sub)
		return "", webrtc.SessionDescription{}, ErrCapacity
	}
	for _, src := range sources {
		if !audioPurpose(src.purpose) {
			m.subscriptionKeyframe(sub, src)
		}
	}
	if len(sub.selections) > 0 && !m.worker(func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-subCtx.Done():
				return
			case <-ticker.C:
				pending := []*source{}
				owner.room.mu.Lock()
				for purpose, choice := range sub.selections {
					if choice.active == choice.wanted {
						continue
					}
					if group := publisher.sources[purpose]; group != nil {
						if source := group.layers[choice.wanted]; source != nil {
							pending = append(pending, source)
						}
					}
				}
				owner.room.mu.Unlock()
				for _, source := range pending {
					m.keyframe(publisher, source)
				}
			}
		}
	}) {
		m.dropSubscription(sub)
		return "", webrtc.SessionDescription{}, ErrCapacity
	}
	success = true
	return sub.id, description, nil
}
func (m *Manager) keyframe(p *participant, src *source) {
	p.room.mu.Lock()
	pc := p.publish
	closed := p.closed
	current := pc == src.peer
	p.room.mu.Unlock()
	if !closed && pc != nil && current {
		_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: src.ssrc}})
	}
}
func (m *Manager) dropSubscription(sub *subscription) {
	sub.owner.room.mu.Lock()
	if sub.owner.subscriptions[sub.publisher.id] == sub {
		delete(sub.owner.subscriptions, sub.publisher.id)
	}
	sub.owner.room.mu.Unlock()
	sub.cancel()
	sub.pc.Close()
}
func (m *Manager) ICE(id, peerID string, candidate webrtc.ICECandidateInit) error {
	p, err := m.get(id)
	if err != nil {
		return err
	}
	var pc *webrtc.PeerConnection
	p.room.mu.Lock()
	if peerID == p.publishID {
		pc = p.publish
	} else {
		for _, sub := range p.subscriptions {
			if sub.id == peerID {
				pc = sub.pc
			}
		}
	}
	p.room.mu.Unlock()
	if pc == nil {
		return identity.ErrNotFound
	}
	if len(candidate.Candidate) > 2048 {
		return identity.ErrInvalid
	}
	return pc.AddICECandidate(candidate)
}
func (m *Manager) Leave(id string) {
	m.leave(id, nil)
}
func (m *Manager) leave(id string, expected *participant, publish ...*webrtc.PeerConnection) {
	m.mu.Lock()
	p := m.participants[id]
	if p == nil || (expected != nil && p != expected) {
		m.mu.Unlock()
		return
	}
	r := p.room
	r.mu.Lock()
	if len(publish) > 0 && p.publish != publish[0] {
		r.mu.Unlock()
		m.mu.Unlock()
		return
	}
	delete(m.participants, id)
	p.closed = true
	delete(r.members, p.id)
	relayPeer := p.relay
	for _, member := range r.members {
		if member.relay != nil {
			delete(member.relay.seen, p.id)
		}
	}
	peers := []*webrtc.PeerConnection{}
	if p.publish != nil {
		peers = append(peers, p.publish)
	}
	subs := []*subscription{}
	for _, sub := range p.subscriptions {
		subs = append(subs, sub)
	}
	p.subscriptions = map[string]*subscription{}
	for _, member := range r.members {
		if sub := member.subscriptions[p.id]; sub != nil {
			subs = append(subs, sub)
			delete(member.subscriptions, p.id)
		}
	}
	if len(r.members) == 0 {
		delete(m.rooms, r.id)
	}
	r.mu.Unlock()
	m.mu.Unlock()
	if relayPeer != nil {
		relayPeer.Close()
	}
	for _, sub := range subs {
		sub.cancel()
		sub.pc.Close()
	}
	for _, pc := range peers {
		pc.Close()
	}
	m.broadcast(r, map[string]any{"type": "media.participant.left", "participant_id": p.id})
}
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	ids := []string{}
	for id := range m.participants {
		ids = append(ids, id)
	}
	m.admissions = map[string]Admission{}
	m.mu.Unlock()
	for _, id := range ids {
		m.Leave(id)
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *Manager) Stats() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{"rooms": len(m.rooms), "participants": len(m.participants), "peer_connections": m.peers.Load(), "dropped_packets": m.drops.Load(), "transport": m.transport, "relay_queued_bytes": m.relayBytes.Load(), "relay_queue_overflows": m.relayOverflows.Load()}
}
