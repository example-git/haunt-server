package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/voice"
	"golang.org/x/time/rate"
)

type mediaGateway struct {
	voice       *voice.Manager
	ids         *identity.Service
	mu          sync.Mutex
	closed      bool
	connections map[string]context.CancelFunc
	wg          sync.WaitGroup
	max         int
}

func newMediaGateway(manager *voice.Manager, ids *identity.Service, max int) *mediaGateway {
	if max == 0 {
		max = 512
	}
	return &mediaGateway{voice: manager, ids: ids, connections: map[string]context.CancelFunc{}, max: max}
}

func (g *mediaGateway) handle(w http.ResponseWriter, r *http.Request) {
	if !g.voice.UsesWebSocket() {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	id := identity.NewID()
	g.mu.Lock()
	if g.closed || len(g.connections) >= g.ids.PeerLimit(g.max) {
		g.mu.Unlock()
		cancel()
		writeJSON(w, 503, map[string]string{"error": "media_capacity_reached"})
		return
	}
	g.connections[id] = cancel
	g.wg.Add(1)
	g.mu.Unlock()
	defer func() { cancel(); g.mu.Lock(); delete(g.connections, id); g.mu.Unlock(); g.wg.Done() }()
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(voice.EncodedFrameLimit)
	var auth MediaAuthenticateCommand
	authCtx, done := context.WithTimeout(ctx, 5*time.Second)
	kind, data, err := conn.Read(authCtx)
	if err != nil || len(data) > 4096 || kind != websocket.MessageText || strictMediaJSON(data, &auth) != nil || auth.Type != "authenticate" {
		done()
		return
	}
	p, err := g.ids.Authenticate(authCtx, auth.Token)
	if err != nil {
		done()
		return
	}
	peer, err := g.voice.AttachRelay(ctx, p, auth.Participant)
	done()
	if err != nil {
		conn.Close(websocket.StatusPolicyViolation, mediaError(err))
		return
	}
	defer peer.Close()
	writeCtx, stopWrite := context.WithTimeout(peer.Context(), 5*time.Second)
	err = conn.Write(writeCtx, websocket.MessageText, []byte(`{"type":"ready","protocol":"chatxo-encoded-v1"}`))
	stopWrite()
	if err != nil {
		return
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer cancel()
		for {
			frame, err := peer.Next(ctx)
			if err != nil {
				return
			}
			if !peer.Deliverable(frame) {
				continue
			}
			kind := websocket.MessageText
			if frame.Binary {
				kind = websocket.MessageBinary
			}
			writeCtx, done := context.WithTimeout(ctx, 5*time.Second)
			err = conn.Write(writeCtx, kind, frame.Data)
			done()
			if err != nil {
				return
			}
		}
	}()
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		defer cancel()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-peer.Context().Done():
				return
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
	defer func() { cancel(); conn.CloseNow(); <-writerDone; <-pingDone }()
	bytesLimit := rate.NewLimiter(8<<20, 8<<20)
	framesLimit := rate.NewLimiter(600, 1200)
	controlLimit := rate.NewLimiter(2, 10)
	configureLimit := rate.NewLimiter(1, 4)
	for {
		kind, data, err := conn.Read(peer.Context())
		if err != nil {
			return
		}
		if !bytesLimit.AllowN(time.Now(), len(data)) || !framesLimit.Allow() {
			return
		}
		if kind == websocket.MessageBinary {
			if peer.Forward(data) != nil {
				return
			}
			continue
		}
		if len(data) > 40<<10 || !controlLimit.Allow() {
			return
		}
		var command MediaCommand
		if strictMediaJSON(data, &command) != nil {
			return
		}
		switch command.Type {
		case "configure":
			if !configureLimit.Allow() || peer.Configure(command.Tracks) != nil {
				return
			}
		case "keyframe":
			if peer.RequestKeyframe(command.Publisher, command.Purpose) != nil {
				return
			}
		default:
			return
		}
	}
}
func strictMediaJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return identity.ErrInvalid
	}
	return nil
}
func (g *mediaGateway) close(ctx context.Context) error {
	g.mu.Lock()
	g.closed = true
	for _, cancel := range g.connections {
		cancel()
	}
	g.mu.Unlock()
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
