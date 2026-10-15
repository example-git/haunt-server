// Package setup serves a one-time installation wizard on the service listener.
package setup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
)

//go:embed assets/*
var assets embed.FS

type handler struct{ value http.Handler }

// Host owns the listener throughout setup and normal operation.
type Host struct {
	Server   *http.Server
	Listener net.Listener
	Errors   chan error
	current  atomic.Pointer[handler]
}

func (h *Host) Use(next http.Handler) { h.current.Store(&handler{next}) }
func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.current.Load().value.ServeHTTP(w, r)
}
func (h *Host) Close() { _ = h.Server.Close(); _ = h.Listener.Close() }

type diskState struct {
	Version int               `json:"version"`
	Values  map[string]string `json:"values"`
}

// Values merges private saved settings with explicit deployment overrides.
func Values() (map[string]string, string, error) {
	env := config.Environment()
	dir := "data"
	if value, ok := env["HAUNT_DATA_DIR"]; ok {
		if value == "" {
			return nil, "", errors.New("HAUNT_DATA_DIR must not be empty")
		}
		dir = value
	}
	path := filepath.Join(dir, "configuration.json")
	values := map[string]string{}
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
		decoder.DisallowUnknownFields()
		var state diskState
		if decoder.Decode(&state) != nil || decoder.Decode(&struct{}{}) != io.EOF || state.Version != 1 || state.Values == nil {
			return nil, "", errors.New("invalid saved configuration.json; restore or correct the file")
		}
		for key, value := range state.Values {
			if !strings.HasPrefix(key, "HAUNT_") || strings.HasPrefix(key, "HAUNT_BOOTSTRAP_") {
				return nil, "", errors.New("invalid saved setting")
			}
			values[key] = value
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("read saved configuration: %w", err)
	}
	for key, value := range env {
		if key == "HAUNT_INSTANCE_ID" && values[key] != "" && values[key] != value {
			return nil, "", errors.New("HAUNT_INSTANCE_ID differs from the saved installation")
		}
		values[key] = value
	}
	return values, path, nil
}

func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".setup-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err == nil {
		dir, openErr := os.Open(filepath.Dir(path))
		if openErr != nil {
			return openErr
		}
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}
func save(path string, values map[string]string) error {
	data, err := json.MarshalIndent(diskState{1, values}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(data, '\n'))
}

func hasOwner(ctx context.Context, c config.Config) (bool, error) {
	pool, err := postgres.Open(ctx, c.DatabaseURL, c.DBMaxConns)
	if err != nil {
		return false, err
	}
	defer pool.Close()
	var table *string
	if err = pool.QueryRow(ctx, `SELECT to_regclass('chatxo.users')::text`).Scan(&table); err != nil {
		return false, err
	}
	if table == nil {
		return false, nil
	}
	var exists bool
	err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.users WHERE instance_id=$1 AND is_owner)`, c.InstanceID).Scan(&exists)
	return exists, err
}

// Begin starts a wizard only when the installation has no owner. Existing
// installations and explicitly supplied unattended bootstrap credentials use
// the ordinary startup path. Once submitted, the caller installs its normal
// handler with Host.Use; no port is closed or rebound.
func Begin(ctx context.Context, logger *slog.Logger) (config.Config, *Host, error) {
	values, path, err := Values()
	if err != nil {
		return config.Config{}, nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return config.Config{}, nil, err
	}
	claimLock, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".setup-lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return config.Config{}, nil, err
	}
	defer claimLock.Close()
	if err = syscall.Flock(int(claimLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return config.Config{}, nil, errors.New("another setup process owns this data directory")
	}
	defer syscall.Flock(int(claimLock.Fd()), syscall.LOCK_UN)
	// Re-read after taking ownership; a competing process may have completed
	// setup after our first read. This is a startup-only filesystem lock.
	values, path, err = Values()
	if err != nil {
		return config.Config{}, nil, err
	}
	cfg, loadErr := config.LoadValues(values)
	if loadErr == nil {
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		exists, err := hasOwner(checkCtx, cfg)
		cancel()
		if err != nil {
			return cfg, nil, err
		}
		if exists || cfg.BootstrapUsername != "" {
			return cfg, nil, nil
		}
	}
	// Validate all supplied values before opening a listener. Placeholders only
	// cover missing fields displayed in the form; explicit bad values still fail.
	trial := clone(values)
	if _, ok := trial["HAUNT_INSTANCE_ID"]; !ok {
		trial["HAUNT_INSTANCE_ID"] = "pending-installation"
	}
	if _, ok := trial["HAUNT_PUBLIC_URL"]; !ok {
		trial["HAUNT_PUBLIC_URL"] = "http://localhost"
	}
	if _, ok := trial["HAUNT_DATABASE_URL"]; !ok {
		trial["HAUNT_DATABASE_URL"] = "postgres://pending@localhost/pending"
	}
	cfg, err = config.LoadValues(trial)
	if err != nil {
		return cfg, nil, err
	}
	if _, ok := values["HAUNT_INSTANCE_ID"]; !ok {
		values["HAUNT_INSTANCE_ID"] = randomSecret()
		// Persist identity immediately so a failed/abandoned setup cannot change it.
		saved := map[string]string{"HAUNT_INSTANCE_ID": values["HAUNT_INSTANCE_ID"]}
		if file, readErr := os.ReadFile(path); readErr == nil {
			var state diskState
			if json.Unmarshal(file, &state) == nil {
				saved = state.Values
				saved["HAUNT_INSTANCE_ID"] = values["HAUNT_INSTANCE_ID"]
			}
		}
		if err = save(path, saved); err != nil {
			return cfg, nil, err
		}
	}
	tokenPath := filepath.Join(cfg.DataDir, "setup-token")
	tokenBytes, err := os.ReadFile(tokenPath)
	if errors.Is(err, os.ErrNotExist) {
		tokenBytes = []byte(randomSecret())
		err = writePrivate(tokenPath, tokenBytes)
	}
	if err != nil || len(tokenBytes) != 43 {
		return cfg, nil, errors.New("cannot load private setup token")
	}
	w := &wizard{values: values, path: path, token: string(tokenBytes), tokenPath: tokenPath, result: make(chan config.Config, 1), slots: make(chan struct{}, 1), logger: logger}
	h := &Host{Errors: make(chan error, 1)}
	h.Use(w)
	h.Server = &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	h.Listener, err = net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return cfg, nil, fmt.Errorf("setup listen: %w", err)
	}
	go func() { h.Errors <- h.Server.Serve(h.Listener) }()
	address := values["HAUNT_PUBLIC_URL"]
	if address == "" {
		host, port, _ := net.SplitHostPort(h.Listener.Addr().String())
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "localhost"
		}
		address = "http://" + net.JoinHostPort(host, port)
	}
	logger.Info("First-time setup: open this private installer link", "setup_url", address+"/setup#"+w.token)
	select {
	case cfg := <-w.result:
		return cfg, h, nil
	case err := <-h.Errors:
		h.Close()
		return cfg, nil, fmt.Errorf("setup HTTP server: %w", err)
	case <-ctx.Done():
		h.Close()
		return cfg, nil, ctx.Err()
	}
}
func randomSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func clone(values map[string]string) map[string]string {
	result := map[string]string{}
	for k, v := range values {
		result[k] = v
	}
	return result
}

type wizard struct {
	values                 map[string]string
	path, token, tokenPath string
	result                 chan config.Config
	slots                  chan struct{}
	submitted              atomic.Bool
	logger                 *slog.Logger
}

var editable = map[string]string{
	"HAUNT_NAME": "Haunt", "HAUNT_PUBLIC_URL": "", "HAUNT_DATABASE_URL": "",
	"HAUNT_MEDIA_PUBLIC_IP": "", "HAUNT_MEDIA_PORT_MIN": "0", "HAUNT_MEDIA_PORT_MAX": "0",
	"HAUNT_TEXT_RETENTION_DAYS": "30", "HAUNT_ATTACHMENT_RETENTION_DAYS": "7",
	"HAUNT_IMAGE_MAX_BYTES": "2097152", "HAUNT_VIDEO_MAX_BYTES": "10485760", "HAUNT_ATTACHMENT_MAX_BYTES": "1073741824",
	"HAUNT_SESSION_DAYS": "30", "HAUNT_MEDIA_ROOM_LIMIT": "16",
}

func output(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *wizard) authorized(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(s.token)) == 1 && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
}
func (s *wizard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	if r.Method == "GET" && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") {
		if s.submitted.Load() {
			output(w, 503, map[string]string{"status": "starting"})
		} else {
			output(w, 200, map[string]string{"status": "setup"})
		}
		return
	}
	if r.Method == "GET" && r.URL.Path == "/" {
		http.Redirect(w, r, "/setup", http.StatusTemporaryRedirect)
		return
	}
	if r.Method == "GET" && (r.URL.Path == "/setup" || r.URL.Path == "/setup/" || r.URL.Path == "/setup/style.css" || r.URL.Path == "/setup/main.js") {
		name := "index.html"
		contentType := "text/html; charset=utf-8"
		if strings.HasSuffix(r.URL.Path, "style.css") {
			name = "style.css"
			contentType = "text/css"
		}
		if strings.HasSuffix(r.URL.Path, "main.js") {
			name = "main.js"
			contentType = "text/javascript"
		}
		data, _ := assets.ReadFile("assets/" + name)
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(data)
		return
	}
	if r.URL.Path != "/setup/config" && r.URL.Path != "/setup/complete" {
		http.Error(w, "Complete first-time setup before using Haunt.", 503)
		return
	}
	if !s.authorized(r) {
		output(w, 401, map[string]string{"error": "Open the private setup link shown in the server startup log."})
		return
	}
	if s.submitted.Load() {
		output(w, 409, map[string]string{"error": "Setup has finished. Wait for the server to start."})
		return
	}
	if r.Method == "GET" && r.URL.Path == "/setup/config" {
		fields := map[string]any{}
		env := config.Environment()
		for key, fallback := range editable {
			value := fallback
			if configured, ok := s.values[key]; ok {
				value = configured
			}
			_, locked := env[key]
			// A deployment-supplied database URL may contain secrets. Never send it to
			// the browser; an unchanged locked field is retained server-side.
			if key == "HAUNT_DATABASE_URL" && locked {
				value = ""
			}
			fields[key] = map[string]any{"value": value, "locked": locked}
		}
		output(w, 200, map[string]any{"fields": fields})
		return
	}
	if r.Method != "POST" || r.URL.Path != "/setup/complete" {
		w.WriteHeader(405)
		return
	}
	// Origin is mandatory for this browser-only operation. Do not trust arbitrary
	// forwarding headers when deciding whether credentials have HTTPS transport.
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host != r.Host || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || (origin.Scheme != "https" && origin.Scheme != "http") {
		output(w, 403, map[string]string{"error": "Setup must be submitted from this server's own page."})
		return
	}
	secure := r.TLS != nil
	if !secure && r.Header.Get("X-Forwarded-Proto") == "https" {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip, _ := netip.ParseAddr(host)
		for _, raw := range strings.Split(s.values["HAUNT_TRUSTED_PROXIES"], ",") {
			prefix, e := netip.ParsePrefix(raw)
			if e == nil && prefix.Contains(ip) {
				secure = true
			}
		}
	}
	local := strings.EqualFold(origin.Hostname(), "localhost")
	if ip := net.ParseIP(origin.Hostname()); ip != nil {
		local = ip.IsLoopback() || ip.IsPrivate()
	}
	if !secure && !local {
		output(w, 403, map[string]string{"error": "Use HTTPS for setup on a public address."})
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		output(w, 409, map[string]string{"error": "Another setup submission is running."})
		return
	}
	if s.submitted.Load() {
		output(w, 409, map[string]string{"error": "Setup has already finished."})
		return
	}
	var submission struct {
		Values   map[string]string `json:"values"`
		Username string            `json:"username"`
		Password string            `json:"password"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&submission) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		output(w, 400, map[string]string{"error": "Invalid setup form."})
		return
	}
	values := clone(s.values)
	env := config.Environment()
	for key, value := range submission.Values {
		if _, ok := editable[key]; !ok {
			output(w, 400, map[string]string{"error": "Unknown setup setting."})
			return
		}
		if existing, locked := env[key]; locked {
			if value != existing {
				output(w, 400, map[string]string{"error": key + " is controlled by the deployment environment."})
				return
			}
		} else if value == "" && key == "HAUNT_MEDIA_PUBLIC_IP" {
			delete(values, key)
		} else {
			values[key] = value
		}
	}
	values["HAUNT_BOOTSTRAP_USERNAME"] = submission.Username
	values["HAUNT_BOOTSTRAP_PASSWORD"] = submission.Password
	cfg, err := config.LoadValues(values)
	if err != nil {
		output(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err = identity.ValidateFirstOwner(submission.Username, submission.Password); err != nil {
		output(w, 400, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		output(w, 400, map[string]string{"error": err.Error()})
		return
	}
	defer pool.Close()
	if err = postgres.Migrate(ctx, pool, cfg.InstanceID); err != nil {
		s.logger.Error("setup migration failed", "error", err)
		output(w, 400, map[string]string{"error": "Database migration failed. Check the server log; existing installation IDs must be preserved."})
		return
	}
	var exists bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.users WHERE instance_id=$1 AND is_owner)`, cfg.InstanceID).Scan(&exists); err != nil {
		output(w, 500, map[string]string{"error": "Could not check the owner account."})
		return
	}
	if exists {
		output(w, 409, map[string]string{"error": "This database already has an owner. Setup will not change their password."})
		return
	}
	// Save before creating the owner: a write failure must leave setup available.
	// Passwords are never included in the saved configuration.
	saved := clone(values)
	for key := range saved {
		if _, supplied := env[key]; supplied && key != "HAUNT_INSTANCE_ID" {
			delete(saved, key)
		}
	}
	for key := range saved {
		if strings.HasPrefix(key, "HAUNT_BOOTSTRAP_") {
			delete(saved, key)
		}
	}
	if err = save(s.path, saved); err != nil {
		output(w, 500, map[string]string{"error": "Could not save configuration. Check data-directory permissions."})
		return
	}
	if err = identity.CreateFirstOwner(ctx, cfg, pool); err != nil {
		output(w, 400, map[string]string{"error": err.Error()})
		return
	}
	cfg.BootstrapUsername = ""
	cfg.BootstrapPassword = ""
	s.submitted.Store(true)
	_ = os.Remove(s.tokenPath)
	output(w, 202, map[string]string{"status": "starting"})
	s.result <- cfg
}
