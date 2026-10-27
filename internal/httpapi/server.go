// Package httpapi exposes the server's HTTP boundary. Handlers are concurrent;
// configuration is captured at construction and never mutated by requests.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	haunt "github.com/example-git/haunt-server"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
)

func New(c config.Config, identities *identity.Service, logger *slog.Logger, features ...*Features) *http.Server {
	mux := http.NewServeMux()
	// Installer routes cease to exist once an owner is configured, even in
	// embedded-client builds whose GET fallback would otherwise serve the SPA.
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		mux.HandleFunc(method+" /setup", http.NotFound)
		mux.HandleFunc(method+" /setup/", http.NotFound)
	}
	a := &api{capacity: newAdmission(c), identity: identities, logger: logger, limits: newLimiter(), trustedProxies: c.TrustedProxies, cloudflared: c.Cloudflared}
	a.routes(mux)
	a.clientUpdateRoutes(mux, c)
	for _, f := range features {
		f.routes(a, mux)
	}
	publicRoutes(mux, c, identities, len(features) > 0)
	serveDocumentation(mux)
	if files := haunt.WebClient(); files != nil {
		mux.Handle("GET /", webClientHandler(files))
	}
	origins := append(append([]string{}, c.ClientOrigins...), c.PublicURL)
	return &http.Server{
		Addr:              c.ListenAddr,
		Handler:           cors(origins, requestContext(a.capacity.wrap(mux, a.moderationBoundary(mux)))),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

func publicRoutes(mux routeRegistrar, c config.Config, identities *identity.Service, hasFeatures bool) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := identities.Ready(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, unavailableResponse{Status: "unavailable", Dependency: "postgres"})
			return
		}
		writeJSON(w, http.StatusOK, statusResponse{Status: "ok", Check: "identity_schema"})
	})
	mux.HandleFunc("GET /api/v1/instance", func(w http.ResponseWriter, r *http.Request) {
		community, err := identities.Community(r.Context(), c.Name)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "instance_unavailable"})
			return
		}
		capabilities := []string{"instance.discovery", "auth.invitation", "auth.password", "auth.password_reset", "sessions.devices", "sessions.persistent", "sessions.restore", "community.permissions", "community.role_sections", "community.channel_icons", "community.moderation", "community.ban_appeals", "community.emojis", "community.animated_art"}
		if hasFeatures {
			capabilities = append(capabilities, "server.extensions.v1")
			capabilities = append(capabilities, "audio.plugins.v1", "menus.declarative.v1", "instance.configuration", "storage.providers", "messages.channels", "messages.voice_channels", "messages.dms", "dms.storage.v1", "dms.mls.v1", "messages.retention", "events.websocket", "events.account_feed", "presence.online", "messages.read_state", "messages.replies", "messages.history_pages.v1", "messages.embeds", "attachments.media", "media.voice", "media.camera", "media.screen", "media.presence", "media.source_audio", "media.joined_at", "media.moderation.v1", "media.layers.v1", "media.levels.v1")
		}
		capabilities = append(capabilities, "community.administrator")
		if c.TURN.Enabled || c.HasExternalTURN() {
			capabilities = append(capabilities, "media.relay")
		}
		if hasFeatures && c.MediaTransport() == "websocket" {
			capabilities = append(capabilities, "media.websocket")
		}
		if err := identities.LoadRuntime(r.Context()); err != nil {
			writeJSON(w, 503, errorResponse{Error: "instance_unavailable"})
			return
		}
		runtime := identities.Runtime(c.RuntimeDefaults()).Settings
		registration := "invitation_only"
		if !runtime.RegistrationEnabled {
			registration = "closed"
		}
		writeJSON(w, http.StatusOK, instanceResponse{
			ClientUpdates: clientUpdateSource(c), InstanceID: c.InstanceID, Name: community.Name, Description: community.Description, AccentColor: community.AccentColor, IconVersion: community.IconVersion, BannerVersion: community.BannerVersion, PublicURL: c.PublicURL, APIVersion: "v1", Capabilities: capabilities, MediaTransport: c.MediaTransport(),
			Authentication: authenticationInfo{Scheme: "Bearer", Registration: registration, SessionDays: runtime.SessionDays}, Limits: runtime.Limits,
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	// Marshal before sending headers so encoding failure cannot produce an
	// apparent successful response with an incomplete body.
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "response encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func cors(origins []string, next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		allowed[origin] = struct{}{}
	}
	// allowed is immutable after construction; concurrent reads need no mutex.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" {
			if _, ok := allowed[origin]; !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin_not_allowed"})
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/api/v1/events" || r.URL.Path == "/api/v1/media" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/attachments") || r.URL.Path == "/api/v1/admin/audio-plugins") {
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
