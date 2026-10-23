package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"errors"
	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/audioplugin"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/embeds"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/relay"
	"github.com/example-git/haunt-server/internal/serverextension"
	"github.com/example-git/haunt-server/internal/turnclient"
	"github.com/example-git/haunt-server/internal/voice"
	"github.com/jackc/pgx/v5/pgxpool"
	"mime"
	"time"
)

type Features struct {
	Extensions       *serverextension.Store
	AudioPlugins     *audioplugin.Store
	PluginRepository *audioplugin.RepositoryInstaller
	Defaults         config.RuntimeSettings
	Previews         embeds.Resolver
	Chat             *chat.Store
	Gateway          *gateway
	Attachments      *attachments.Store
	Voice            *voice.Manager
	Relay            *relay.Server
	Media            *mediaGateway
	CloudTURN        *turnclient.Client
}

func NewFeatures(ctx context.Context, c config.Config, pool *pgxpool.Pool, ids *identity.Service, logger *slog.Logger) (*Features, error) {
	store, err := chat.New(pool, ids, c)
	if err != nil {
		return nil, err
	}
	media, err := attachments.New(ctx, c, store)
	if err != nil {
		return nil, err
	}
	// Acquire the installation's file ownership before applying a potentially
	// smaller retention policy. A rejected second process must not prune the
	// active process's history as a side effect of its failed startup.
	if err := store.Maintain(ctx); err != nil {
		media.Close()
		return nil, err
	}
	plugins, err := audioplugin.New(c.DataDir)
	if err != nil {
		media.Close()
		return nil, err
	}
	if c.DefaultAudioPlugin {
		if err := plugins.SeedDefault(); err != nil {
			media.Close()
			return nil, err
		}
	}
	cloudTURN := turnclient.New(c)
	extensions, err := serverextension.New(c.DataDir)
	if err != nil {
		media.Close()
		return nil, err
	}
	var sources []turnclient.Source
	if cloudTURN != nil {
		sources = append(sources, cloudTURN)
	}
	sfu, err := voice.New(c, ids, sources...)
	if err != nil {
		media.Close()
		return nil, err
	}
	turn, err := relay.New(c.TURN)
	if err != nil {
		sfu.Close(ctx)
		media.Close()
		return nil, err
	}
	gateway := newGateway(store, ids, logger)
	gateway.media = sfu
	return &Features{Extensions: extensions, PluginRepository: audioplugin.NewRepositoryInstaller(c.AudioPluginGitHubToken), AudioPlugins: plugins, Defaults: c.RuntimeDefaults(), Previews: embeds.New(), Chat: store, Gateway: gateway, Attachments: media, Voice: sfu, Relay: turn, Media: newMediaGateway(sfu, ids, c.MediaMaxPeers), CloudTURN: cloudTURN}, nil
}
func (f *Features) Maintain(ctx context.Context) error {
	return errors.Join(f.Chat.Maintain(ctx), f.Attachments.Maintain(ctx))
}
func (f *Features) Close(ctx context.Context) error {
	if f.Extensions != nil {
		f.Extensions.Close()
	}
	if f.PluginRepository != nil {
		f.PluginRepository.Close()
	}
	if f.Previews != nil {
		f.Previews.Close()
	}
	defer f.CloudTURN.Close()
	return errors.Join(f.Gateway.close(ctx), f.Media.close(ctx), f.Voice.Close(ctx), f.Relay.Close(), f.Attachments.Close())
}

func (f *Features) routes(a *api, mux routeRegistrar) {
	f.dmRoutes(a, mux)
	if f.Gateway != nil {
		f.Gateway.audit = a.auditAccess
	}
	f.operations(a, mux)
	f.configurationRoutes(a, mux)
	f.menuRoutes(a, mux)
	f.audioPluginRoutes(a, mux)
	f.serverExtensionRoutes(a, mux)
	f.embedRoutes(a, mux)
	mux.HandleFunc("GET /api/v1/events", f.Gateway.handle)
	mux.HandleFunc("GET /api/v1/media", f.Media.handle)
	mux.HandleFunc("GET /api/v1/voice/{id}/ice", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if _, err := f.Voice.Room(r.Context(), p, r.PathValue("id")); err != nil {
			a.fail(w, r, err)
			return
		}
		credentials, err := f.Voice.ConnectionICE(r.Context(), p)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if f.Relay != nil {
			local, err := f.Relay.Credentials(p)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			credentials.Servers = append(credentials.Servers, local.Servers...)
			if credentials.ExpiresAt.IsZero() || local.ExpiresAt.Before(credentials.ExpiresAt) {
				credentials.ExpiresAt = local.ExpiresAt
			}
		}
		if len(credentials.Servers) == 0 || f.Voice.UsesWebSocket() {
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		var expires *time.Time
		if !credentials.ExpiresAt.IsZero() {
			expires = &credentials.ExpiresAt
		}
		writeJSON(w, 200, iceResponse{ICE: credentials.Servers, Expires: expires, Policy: f.Voice.ICEPolicy()})
	}))
	mux.HandleFunc("POST /api/v1/voice/{id}/admissions", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		item, err := f.Voice.Admit(r.Context(), p, r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if f.Relay != nil && item.Transport == "webrtc" {
			credentials, err := f.Relay.Credentials(p)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			item.ICE = append(append([]config.ICEServer{}, item.ICE...), credentials.Servers...)
			if item.ICEExpiresAt == nil || credentials.ExpiresAt.Before(*item.ICEExpiresAt) {
				item.ICEExpiresAt = &credentials.ExpiresAt
			}
		}
		writeJSON(w, 201, item)
	}))
	mux.HandleFunc("GET /api/v1/voice/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := f.Voice.Room(r.Context(), p, r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, participantsResponse{Participants: items})
	}))
	mux.HandleFunc("GET /api/v1/voice/{id}/restrictions", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := a.identity.VoiceRestrictions(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("after"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, items)
	}))
	mux.HandleFunc("PATCH /api/v1/voice/{id}/restrictions/{user}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v VoiceRestrictionRequest
		if err := decodeLimit(w, r, &v, 4096); err != nil {
			a.fail(w, r, err)
			return
		}
		item, err := f.Voice.Moderate(r.Context(), p, r.PathValue("id"), r.PathValue("user"), v.Muted, v.Deafened)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, item)
	}))
	mux.HandleFunc("POST /api/v1/conversations/{id}/attachments", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !a.limit(w, "upload:"+p.User.ID, 10, time.Minute) {
			return
		}
		size, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
		if err != nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		if r.ContentLength >= 0 && r.ContentLength != size {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(9 * time.Minute))
		ctx, cancel := context.WithTimeout(r.Context(), 9*time.Minute)
		defer cancel()
		r = r.WithContext(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, size+1)
		item, err := f.Attachments.Upload(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("filename"), size, r.Body, r.URL.Query().Get("encrypted") == "1")
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 201, item)
	}))
	mux.HandleFunc("GET /api/v1/attachments/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		item, file, err := f.Attachments.Get(r.Context(), p, r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		defer file.Close()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(9 * time.Minute))
		w.Header().Set("Content-Type", item.ContentType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": item.Filename}))
		http.ServeContent(w, r, item.Filename, time.Time{}, file)
	}))
	mux.HandleFunc("DELETE /api/v1/attachments/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := f.Attachments.Delete(r.Context(), p, r.PathValue("id")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /api/v1/read-state", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		page, err := f.Chat.ReadPage(r.Context(), p, r.URL.Query().Get("after"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, page)
	}))
	mux.HandleFunc("PUT /api/v1/conversations/{id}/read", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var body MarkReadRequest
		if err := decode(w, r, &body); err != nil {
			a.fail(w, r, err)
			return
		}
		if !a.limit(w, "read:"+p.User.ID, 240, time.Minute) {
			return
		}
		state, err := f.Chat.MarkRead(r.Context(), p, r.PathValue("id"), body.Seq)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, state)
	}))
	mux.HandleFunc("GET /api/v1/conversations", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := f.Chat.Conversations(r.Context(), p, r.URL.Query().Get("after"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, conversationsResponse{Conversations: items})
	}))
	mux.HandleFunc("POST /api/v1/dms", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var body CreateDMRequest
		if err := decode(w, r, &body); err != nil {
			a.fail(w, r, err)
			return
		}
		item, err := f.Chat.DM(r.Context(), p, body.Members)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 201, item)
	}))
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		after := int64(0)
		limit := 100
		var err error
		if value := r.URL.Query().Get("after"); value != "" {
			after, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				a.fail(w, r, identity.ErrInvalid)
				return
			}
		}
		if value := r.URL.Query().Get("limit"); value != "" {
			limit, err = strconv.Atoi(value)
			if err != nil {
				a.fail(w, r, identity.ErrInvalid)
				return
			}
		}
		history, err := f.Chat.History(r.Context(), p, r.PathValue("id"), after, limit)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, history)
	}))
	mux.HandleFunc("POST /api/v1/conversations/{id}/messages", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var body CreateMessageRequest
		if err := decodeMessage(w, r, &body); err != nil {
			a.fail(w, r, err)
			return
		}
		item, err := f.Chat.Create(r.Context(), p, r.PathValue("id"), body.Request, body.Body, body.Reply, body.Attachments)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 201, item)
	}))
	mux.HandleFunc("PUT /api/v1/conversations/{id}/messages/{message}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var body EditMessageRequest
		if err := decodeMessage(w, r, &body); err != nil {
			a.fail(w, r, err)
			return
		}
		item, err := f.Chat.Edit(r.Context(), p, r.PathValue("id"), r.PathValue("message"), body.Body, false)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, item)
	}))
	mux.HandleFunc("DELETE /api/v1/conversations/{id}/messages/{message}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		item, err := f.Chat.Edit(r.Context(), p, r.PathValue("id"), r.PathValue("message"), "", true)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, item)
	}))
}

func decodeMessage(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeLimit(w, r, dst, 512<<10)
}
