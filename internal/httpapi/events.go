package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/turnclient"
	"github.com/example-git/haunt-server/internal/voice"
	"github.com/pion/webrtc/v4"
	"strings"
)

type gateway struct {
	audit          func(*http.Request, string)
	media          *voice.Manager
	limits         *limiter
	store          *chat.Store
	identities     *identity.Service
	logger         *slog.Logger
	mu             sync.Mutex
	closed         bool
	connections    map[string]*gatewayConnection
	listenerCancel context.CancelFunc
	listenerWG     sync.WaitGroup
	listenerOnline atomic.Bool
	sessions       map[string]int
	online         map[string]int // authenticated connections per user; protected by mu
	wg             sync.WaitGroup
	overflows      atomic.Uint64
	queuedBytes    atomic.Int64
}

func newGateway(store *chat.Store, ids *identity.Service, logger *slog.Logger) *gateway {
	ctx, cancel := context.WithCancel(context.Background())
	g := &gateway{store: store, identities: ids, logger: logger, connections: map[string]*gatewayConnection{}, sessions: map[string]int{}, online: map[string]int{}, limits: newLimiter(), listenerCancel: cancel}
	g.listenerWG.Add(1)
	go g.listen(ctx)
	return g
}
func (g *gateway) close(ctx context.Context) error {
	g.mu.Lock()
	g.closed = true
	g.listenerCancel()
	for _, connection := range g.connections {
		connection.cancel()
	}
	g.mu.Unlock()
	done := make(chan struct{})
	go func() { g.wg.Wait(); g.listenerWG.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type eventCommand struct {
	Type          string                     `json:"type"`
	Token         string                     `json:"token,omitempty"`
	Conversations []string                   `json:"conversations,omitempty"`
	Enabled       *bool                      `json:"enabled,omitempty"`
	After         *int64                     `json:"after_event,omitempty"`
	Admission     string                     `json:"admission_id,omitempty"`
	Publisher     string                     `json:"publisher_id,omitempty"`
	Peer          string                     `json:"peer_id,omitempty"`
	SDP           *webrtc.SessionDescription `json:"sdp,omitempty"`
	Tracks        map[string]string          `json:"tracks,omitempty"`
	State         *voice.State               `json:"state,omitempty"`
	ICE           *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
}

func (g *gateway) handle(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	id := identity.NewID()
	g.mu.Lock()
	if g.closed || len(g.connections) >= 512 {
		g.mu.Unlock()
		cancel()
		writeJSON(w, 503, map[string]string{"error": "event_capacity_reached"})
		return
	}
	connection := &gatewayConnection{cancel: cancel, wake: make(chan struct{}, 1)}
	g.connections[id] = connection
	g.wg.Add(1)
	g.mu.Unlock()
	defer func() { cancel(); g.mu.Lock(); delete(g.connections, id); g.mu.Unlock(); g.wg.Done() }()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	// The outer CORS boundary already validates configured browser origins.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(128 << 10)
	if g.media != nil {
		defer g.media.Leave(id)
	}
	authCtx, authCancel := context.WithTimeout(ctx, 5*time.Second)
	kind, data, err := conn.Read(authCtx)
	if err != nil {
		authCancel()
		return
	}
	var auth eventCommand
	if kind != websocket.MessageText || json.Unmarshal(data, &auth) != nil || auth.Type != "authenticate" || (auth.After != nil && *auth.After < 0) {
		authCancel()
		conn.Close(websocket.StatusPolicyViolation, "authentication required")
		return
	}
	p, err := g.identities.Authenticate(authCtx, auth.Token)
	authCancel()
	if err != nil {
		if g.audit != nil {
			g.audit(r, "gateway.unauthorized")
		}
		conn.Close(websocket.StatusPolicyViolation, "invalid session")
		return
	}
	g.mu.Lock()
	if g.sessions[p.SessionID] >= 4 {
		g.mu.Unlock()
		conn.Close(websocket.StatusPolicyViolation, "session connection limit")
		return
	}
	g.sessions[p.SessionID]++
	g.mu.Unlock()
	g.setOnlineUser(connection, p.User.ID)
	defer func() {
		g.setOnlineUser(connection, "")
		g.mu.Lock()
		g.sessions[p.SessionID]--
		if g.sessions[p.SessionID] == 0 {
			delete(g.sessions, p.SessionID)
		}
		g.mu.Unlock()
	}()
	output := newEventQueue(func(delta int64) { g.queuedBytes.Add(delta) })
	defer output.close()
	commands := make(chan eventCommand, 8)
	overflow := make(chan struct{}, 1)
	var overloaded atomic.Bool
	markOverflow := func() {
		if overloaded.CompareAndSwap(false, true) {
			g.overflows.Add(1)
		}
		select {
		case overflow <- struct{}{}:
		default:
		}
	}
	send := func(value any) bool {
		if ctx.Err() != nil {
			return false
		}
		b, err := json.Marshal(value)
		if err != nil {
			return false
		}
		if output.push(b) {
			return true
		}
		markOverflow()
		return false
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		defer cancel()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-overflow:
				conn.Close(websocket.StatusPolicyViolation, "resynchronization required")
				return
			case b := <-output.frames:
				output.received(b)
				writeCtx, done := context.WithTimeout(ctx, 5*time.Second)
				err := conn.Write(writeCtx, websocket.MessageText, b)
				done()
				if err != nil {
					return
				}
			case <-ticker.C:
				pingCtx, done := context.WithTimeout(ctx, 5*time.Second)
				err := conn.Ping(pingCtx)
				done()
				if err != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer workers.Done()
		defer cancel()
		for {
			kind, b, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if kind != websocket.MessageText {
				return
			}
			var command eventCommand
			if json.Unmarshal(b, &command) != nil {
				return
			}
			select {
			case commands <- command:
			case <-ctx.Done():
				return
			default:
				return
			}
		}
	}()
	defer func() {
		if overloaded.Load() {
			conn.Close(websocket.StatusTryAgainLater, "resynchronization required")
		}
		cancel()
		conn.CloseNow()
		workers.Wait()
	}()
	_, head, _, err := g.store.Events(ctx, 0, nil)
	if err != nil {
		return
	}
	cursor := head
	if auth.After != nil {
		cursor = *auth.After
	}
	if !send(map[string]any{"type": "ready", "instance_id": g.store.InstanceID, "event_cursor": head}) {
		return
	}
	subscriptions := []string{}
	accountFeed := false
	allowedSet := map[string]bool{}
	lastPoll := time.Time{}
	lastCheckpoint := time.Time{}
	lastWork := time.Time{}
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	watchVoice := false
	lastPresence := ""
	lastVoice := time.Time{}
	watchPresence := false
	lastOnline := ""
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case command := <-commands:
			if !g.limits.allow("ws:"+id, 120, time.Minute) {
				return
			}
			checkCtx, done := context.WithTimeout(ctx, 5*time.Second)
			current, err := g.identities.Authenticate(checkCtx, auth.Token)
			if err != nil {
				done()
				return
			}
			p = current
			if command.Type == "presence.watch" {
				watchPresence = command.Enabled == nil || *command.Enabled
				g.mu.Lock()
				connection.watchPresence = watchPresence
				g.mu.Unlock()
				lastOnline = ""
				done()
				if !send(map[string]string{"type": "presence.watching"}) {
					return
				}
				if watchPresence {
					connection.note(hintPresence, "")
				}
				continue
			}
			if command.Type == "events.watch" {
				if command.After != nil && *command.After < 0 {
					done()
					if !send(map[string]string{"type": "error", "error": "invalid_command"}) {
						return
					}
					continue
				}
				visible, err := g.store.VisibleIDs(checkCtx, p)
				done()
				if err != nil {
					return
				}
				subscriptions = visible
				allowedSet = map[string]bool{}
				for _, conv := range visible {
					allowedSet[conv] = true
				}
				accountFeed = true
				cursor = head
				if command.After != nil {
					cursor = *command.After
				}
				if !send(map[string]any{"type": "events.watching", "event_cursor": head}) {
					return
				}
				connection.note(hintEvents, "")
				continue
			}
			if command.Type == "voice.watch" && g.media != nil {
				watchVoice = command.Enabled == nil || *command.Enabled
				lastPresence = ""
				done()
				if !send(map[string]string{"type": "voice.watching"}) {
					return
				}
				continue
			}
			if strings.HasPrefix(command.Type, "media.") && g.media != nil {
				err := g.media.Validate(checkCtx, p, id)
				var result any
				if err == nil {
					switch command.Type {
					case "media.join":
						item, joinErr := g.media.Join(checkCtx, p, id, command.Admission, send)
						err = joinErr
						result = map[string]any{"type": "media.joined", "participant": item}
					case "media.leave":
						g.media.Leave(id)
						result = map[string]any{"type": "media.left"}
					case "media.state":
						if command.State == nil {
							err = identity.ErrInvalid
						} else {
							err = g.media.SetState(id, *command.State)
							result = map[string]string{"type": "media.state.accepted"}
						}
					case "media.publish":
						if command.SDP == nil {
							err = identity.ErrInvalid
						} else {
							peerID, sdp, publishErr := g.media.Publish(checkCtx, id, *command.SDP, command.Tracks)
							err = publishErr
							result = map[string]any{"type": "media.answer", "peer_id": peerID, "sdp": sdp}
						}
					case "media.subscribe":
						if command.SDP == nil {
							err = identity.ErrInvalid
						} else {
							peerID, sdp, subscribeErr := g.media.Subscribe(checkCtx, id, command.Publisher, *command.SDP)
							err = subscribeErr
							result = map[string]any{"type": "media.answer", "peer_id": peerID, "publisher_id": command.Publisher, "sdp": sdp}
						}
					case "media.ice":
						if command.ICE == nil {
							err = identity.ErrInvalid
						} else {
							err = g.media.ICE(id, command.Peer, *command.ICE)
							result = map[string]string{"type": "media.ice.accepted"}
						}
					default:
						err = identity.ErrInvalid
					}
				}
				done()
				if err != nil {
					if !send(map[string]string{"type": "error", "error": mediaError(err)}) {
						return
					}
				} else if !send(result) {
					return
				}
				continue
			}
			if command.Type != "subscribe" || len(command.Conversations) > 64 || (command.After != nil && *command.After < 0) {
				done()
				if !send(map[string]string{"type": "error", "error": "invalid_command"}) {
					return
				}
				continue
			}
			allowed := []string{}
			seen := map[string]bool{}
			valid := true
			for _, conv := range command.Conversations {
				if seen[conv] || g.store.Access(checkCtx, p, conv, false) != nil {
					valid = false
					break
				}
				seen[conv] = true
				allowed = append(allowed, conv)
			}
			done()
			if !valid {
				if !send(map[string]string{"type": "error", "error": "subscription_denied"}) {
					return
				}
				continue
			}
			accountFeed = false
			subscriptions = allowed
			if command.After != nil {
				cursor = *command.After
			}
			if !send(map[string]any{"type": "subscribed", "conversations": subscriptions}) {
				return
			}
		case <-ticker.C:
			connection.note(hintTimer, "")
		case <-retry.C:
			select {
			case connection.wake <- struct{}{}:
			default:
			}
		case <-connection.wake:
			if delay := 100*time.Millisecond - time.Since(lastWork); delay > 0 {
				retry.Reset(delay)
				continue
			}
			lastWork = time.Now()
			flags, readIDs := connection.take()
			if watchPresence && flags&hintPresence != 0 {
				users := g.onlineUsers()
				encoded, _ := json.Marshal(users)
				if string(encoded) != lastOnline {
					if !send(map[string]any{"type": "presence.snapshot", "online_user_ids": users}) {
						return
					}
					lastOnline = string(encoded)
				}
			}
			// A presence-only wake must not add authentication, directory or
			// media queries. Existing timer/session hints still validate access.
			if flags != 0 && flags & ^uint32(hintPresence) == 0 {
				continue
			}
			poll := !accountFeed || flags&(hintEvents|hintDirectory|hintSession|hintSync) != 0 || time.Since(lastPoll) >= 5*time.Second
			if !poll && !watchVoice && flags&hintReads == 0 {
				continue
			}
			checkCtx, done := context.WithTimeout(ctx, 5*time.Second)
			if poll || flags&hintReads != 0 {
				current, err := g.identities.Authenticate(checkCtx, auth.Token)
				if err != nil {
					done()
					return
				}
				p = current
			}
			if accountFeed && (flags&hintDirectory != 0 || !g.listenerOnline.Load()) {
				visible, err := g.store.VisibleIDs(checkCtx, p)
				if err != nil {
					done()
					return
				}
				nextSet := map[string]bool{}
				for _, conv := range visible {
					nextSet[conv] = true
				}
				for conv := range allowedSet {
					if !nextSet[conv] && !send(map[string]any{"type": "subscription.revoked", "conversation_id": conv}) {
						done()
						return
					}
				}
				subscriptions = visible
				allowedSet = nextSet
				if !send(map[string]string{"type": "directory.changed"}) {
					done()
					return
				}
			}
			if accountFeed && flags&hintSync != 0 {
				if !send(map[string]string{"type": "read.sync"}) {
					done()
					return
				}
			}
			if g.media != nil {
				if watchVoice && (time.Since(lastVoice) >= time.Second || flags&hintDirectory != 0) {
					lastVoice = time.Now()
					presence, err := g.media.Presence(checkCtx, p)
					if err != nil {
						done()
						return
					}
					encoded, err := json.Marshal(presence)
					if err != nil {
						done()
						return
					}
					if string(encoded) != lastPresence {
						if !send(map[string]any{"type": "voice.presence", "rooms": presence}) {
							done()
							return
						}
						lastPresence = string(encoded)
					}
				}
				if err := g.media.Validate(checkCtx, p, id); err != nil {
					if !send(map[string]string{"type": "media.revoked"}) {
						done()
						return
					}
				}
			}
			if !accountFeed {
				allowed := []string{}
				for _, conv := range subscriptions {
					if g.store.Access(checkCtx, p, conv, false) == nil {
						allowed = append(allowed, conv)
					} else {
						if !send(map[string]any{"type": "subscription.revoked", "conversation_id": conv}) {
							done()
							return
						}
					}
				}
				subscriptions = allowed
			}
			if accountFeed && len(readIDs) > 0 {
				visibleReads := []string{}
				for _, conv := range readIDs {
					if allowedSet[conv] {
						visibleReads = append(visibleReads, conv)
					}
				}
				states, err := g.store.ReadStates(checkCtx, p, visibleReads)
				if err != nil {
					done()
					return
				}
				if len(states) > 0 && !send(map[string]any{"type": "read.state", "states": states}) {
					done()
					return
				}
			}
			if !poll {
				done()
				continue
			}
			lastPoll = time.Now()
			if len(subscriptions) == 0 && !accountFeed {
				done()
				continue
			}
			events, next, gap, err := g.store.Events(checkCtx, cursor, subscriptions)
			states := []chat.ReadState{}
			if err == nil && !gap && accountFeed && len(events) > 0 {
				affected := map[string]bool{}
				for _, frame := range events {
					var event chat.Event
					if json.Unmarshal(frame, &event) == nil {
						affected[event.ConversationID] = true
					}
				}
				ids := make([]string, 0, len(affected))
				for conv := range affected {
					ids = append(ids, conv)
				}
				states, err = g.store.ReadStates(checkCtx, p, ids)
			}
			done()
			if err != nil {
				g.logger.Error("event replay failed", "error", err)
				return
			}
			if gap {
				if !send(map[string]any{"type": "sync.required", "event_cursor": next}) {
					return
				}
			} else {
				for _, b := range events {
					if !output.push(b) {
						markOverflow()
						return
					}
				}
			}
			if accountFeed && !gap {
				if len(states) > 0 && !send(map[string]any{"type": "read.state", "states": states}) {
					return
				}
				if (len(events) > 0 || time.Since(lastCheckpoint) >= 5*time.Second) && next != cursor {
					if !send(map[string]any{"type": "events.checkpoint", "event_cursor": next}) {
						return
					}
					lastCheckpoint = time.Now()
				}
				// Drain replay in bounded pages, allowing the writer time to
				// consume frames between coalesced rounds.
				if len(events) > 0 {
					connection.note(hintEvents, "")
				}
			}
			cursor = next
		}
	}
}

func (g *gateway) stats() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return map[string]any{"connections": len(g.connections), "queue_overflows": g.overflows.Load(), "queued_bytes": g.queuedBytes.Load(), "listener_connected": g.listenerOnline.Load()}
}
func mediaError(err error) string {
	for _, public := range []error{identity.ErrInvalid, identity.ErrUnauthorized, identity.ErrForbidden, identity.ErrNotFound, identity.ErrConflict, voice.ErrCapacity, turnclient.ErrUnavailable} {
		if errors.Is(err, public) {
			return public.Error()
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request_timeout"
	}
	return "media_operation_failed"
}
