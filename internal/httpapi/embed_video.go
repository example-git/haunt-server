package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/embeds"
	"github.com/example-git/haunt-server/internal/identity"
)

type embedVideoQuery struct {
	Index int `query:"index" minimum:"0" maximum:"2" required:"true"`
	Asset int `query:"asset" minimum:"0" maximum:"3" required:"true"`
}
type embedTicketQuery struct {
	Ticket string `query:"ticket" required:"true"`
	Range  string `header:"Range"`
}
type embedTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}
type videoResolver interface {
	Video(context.Context, string, string) (*http.Response, error)
}
type embedTicket struct {
	token   string
	message chat.Message
	url     string
	expires time.Time
}

func (f *Features) embedVideoRoutes(a *api, mux routeRegistrar) {
	var mu sync.Mutex
	tickets := map[string]embedTicket{}
	slots := make(chan struct{}, 16)
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages/{message}/embeds/video-ticket", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !a.limit(w, "embed-video-ticket:"+p.User.ID, 60, time.Minute) {
			return
		}
		index, e1 := strconv.Atoi(r.URL.Query().Get("index"))
		asset, e2 := strconv.Atoi(r.URL.Query().Get("asset"))
		if e1 != nil || e2 != nil || index < 0 || index >= embeds.MaxLinks || asset < 0 || asset >= 4 {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		message, err := f.Chat.Message(r.Context(), p, r.PathValue("id"), r.PathValue("message"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		links := embeds.Links(message.Body)
		if index >= len(links) {
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		preview, err := f.Previews.Page(r.Context(), links[index])
		if err != nil || asset >= len(preview.Media) || (preview.Media[asset].Kind != "video" && preview.Media[asset].Kind != "audio") {
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		if !f.sameEmbedMessage(w, r, a, message) {
			return
		}
		tokenBytes := make([]byte, 24)
		if _, err = rand.Read(tokenBytes); err != nil {
			a.fail(w, r, err)
			return
		}
		ticket := hex.EncodeToString(tokenBytes)
		expires := time.Now().Add(30 * time.Minute)
		mu.Lock()
		defer mu.Unlock()
		for key, value := range tickets {
			if time.Now().After(value.expires) {
				delete(tickets, key)
			}
		}
		if len(tickets) >= 1024 {
			w.Header().Set("Retry-After", "30")
			writeJSON(w, 503, errorResponse{Error: "embed_busy"})
			return
		}
		tickets[ticket] = embedTicket{token: strings.Fields(r.Header.Get("Authorization"))[1], message: message, url: preview.Media[asset].URL, expires: expires}
		writeJSON(w, 200, embedTicketResponse{Ticket: ticket, ExpiresAt: expires})
	}))
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages/{message}/embeds/video", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ticket, ok := tickets[r.URL.Query().Get("ticket")]
		mu.Unlock()
		if !ok || time.Now().After(ticket.expires) || ticket.message.ConversationID != r.PathValue("id") || ticket.message.ID != r.PathValue("message") {
			a.fail(w, r, identity.ErrUnauthorized)
			return
		}
		r.Header.Set("Authorization", "Bearer "+ticket.token)
		if !f.sameEmbedMessage(w, r, a, ticket.message) {
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 503, errorResponse{Error: "embed_busy"})
			return
		}
		resolver, ok := f.Previews.(videoResolver)
		if !ok {
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		response, err := resolver.Video(r.Context(), ticket.url, r.Header.Get("Range"))
		if err != nil {
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		defer response.Body.Close()
		if !f.sameEmbedMessage(w, r, a, ticket.message) {
			return
		}
		for _, key := range []string{"Content-Type", "Content-Length", "Content-Range"} {
			if value := response.Header.Get(key); value != "" {
				w.Header().Set(key, value)
			}
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(response.StatusCode)
		if r.Method != "HEAD" {
			_, _ = io.Copy(w, io.LimitReader(response.Body, embeds.VideoChunk))
		}
	})
}
