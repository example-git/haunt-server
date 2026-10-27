package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/embeds"
	"github.com/example-git/haunt-server/internal/identity"
)

type embedsResponse struct {
	Embeds []embeds.Preview `json:"embeds"`
}
type embedsQuery struct {
	ClientPosts bool `query:"client_posts"`
}
type embedImageQuery struct {
	Index  int  `query:"index" minimum:"0" maximum:"2" required:"true"`
	Asset  *int `query:"asset" minimum:"0" maximum:"3"`
	Avatar bool `query:"avatar"`
}

func (f *Features) embedRoutes(a *api, mux routeRegistrar) {
	f.embedVideoRoutes(a, mux)
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages/{message}/embeds", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !a.limit(w, "embeds:"+p.User.ID, 240, time.Minute) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		message, err := f.Chat.Message(ctx, p, r.PathValue("id"), r.PathValue("message"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		links := embeds.Links(message.Body)
		results := make([]embeds.Preview, len(links))
		failures := make([]error, len(links))
		var wg sync.WaitGroup
		for index, link := range links {
			if r.URL.Query().Get("client_posts") == "true" && embeds.ClientPost(link) {
				continue
			}
			wg.Add(1)
			go func(index int, link string) {
				defer wg.Done()
				results[index], failures[index] = f.Previews.Page(ctx, link)
				results[index].Index = index
			}(index, link)
		}
		wg.Wait()
		if !f.sameEmbedMessage(w, r, a, message) {
			return
		}
		response := embedsResponse{Embeds: []embeds.Preview{}}
		for i, result := range results {
			if errors.Is(failures[i], embeds.ErrBusy) {
				w.Header().Set("Retry-After", "1")
				writeJSON(w, 503, errorResponse{Error: "embed_busy"})
				return
			}
			if failures[i] != nil {
				result = embeds.Preview{Index: i, URL: links[i], Kind: "link", Status: "unavailable"}
			}
			if result.Status == "unavailable" || result.Title != "" || result.Description != "" || result.Image || result.YouTubeID != "" || result.Player != "" || len(result.Media) > 0 || result.Author != "" {
				response.Embeds = append(response.Embeds, result)
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, response)
	}))
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages/{message}/embeds/image", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !a.limit(w, "embed-images:"+p.User.ID, 240, time.Minute) {
			return
		}
		index, err := strconv.Atoi(r.URL.Query().Get("index"))
		if err != nil || index < 0 || index >= embeds.MaxLinks {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		message, err := f.Chat.Message(ctx, p, r.PathValue("id"), r.PathValue("message"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		links := embeds.Links(message.Body)
		if index >= len(links) {
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		preview, err := f.Previews.Page(ctx, links[index])
		if errors.Is(err, embeds.ErrBusy) {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 503, errorResponse{Error: "embed_busy"})
			return
		}
		if err != nil {
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		imageURL := preview.ImageURL
		embedded := preview.Asset
		if raw := r.URL.Query().Get("asset"); raw != "" {
			item, parseErr := strconv.Atoi(raw)
			if parseErr != nil || item < 0 || item >= len(preview.Media) {
				a.fail(w, r, identity.ErrInvalid)
				return
			}
			media := preview.Media[item]
			imageURL = media.URL
			if media.Kind != "image" {
				imageURL = media.PosterURL
			}
			embedded = nil
		}
		if r.URL.Query().Get("avatar") == "true" {
			imageURL = preview.AvatarURL
			embedded = nil
		}
		if imageURL == "" && embedded == nil {
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		var asset embeds.Asset
		if embedded != nil {
			asset = *embedded
		} else {
			asset, err = f.Previews.Image(ctx, imageURL)
		}
		if err != nil {
			if errors.Is(err, embeds.ErrBusy) {
				w.Header().Set("Retry-After", "1")
				writeJSON(w, 503, errorResponse{Error: "embed_busy"})
				return
			}
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		if !f.sameEmbedMessage(w, r, a, message) {
			return
		}
		w.Header().Set("Content-Type", asset.Type)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		http.ServeContent(w, r, "preview", time.Time{}, bytes.NewReader(asset.Data))
	}))
	mux.HandleFunc("GET /api/v1/attachments/{id}/metadata", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		item, err := f.Attachments.Metadata(r.Context(), p, r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, item)
	}))
}
func (f *Features) sameEmbedMessage(w http.ResponseWriter, r *http.Request, a *api, before chat.Message) bool {
	// Recheck access and body after network I/O so deletion/edit/revocation does
	// not expose an old preview while a remote origin was responding slowly.
	fresh, err := a.identity.Authenticate(r.Context(), strings.Fields(r.Header.Get("Authorization"))[1])
	if err != nil {
		a.fail(w, r, err)
		return false
	}
	current, err := f.Chat.Message(r.Context(), fresh, before.ConversationID, before.ID)
	if err != nil {
		a.fail(w, r, err)
		return false
	}
	if current.Body != before.Body {
		a.fail(w, r, identity.ErrConflict)
		return false
	}
	return true
}
