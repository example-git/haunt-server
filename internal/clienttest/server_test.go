package integration

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/httpapi"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/example-git/haunt-server/internal/testutil"
)

const password = "client integration passphrase"

type fixture struct {
	URL            string `json:"url"`
	Name           string `json:"name"`
	Instance       string `json:"instance"`
	General        string `json:"general"`
	Voice          string `json:"voice"`
	Invitation     string `json:"invitation"`
	TLSCertificate string `json:"tlsCertificate,omitempty"`
}

func server(t *testing.T, name string, encoded bool) fixture {
	t.Helper()
	// Database verifies the dedicated Docker container before ANY PG connection.
	pool, id := testutil.Database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := config.Config{InstanceID: id, Name: name, PublicURL: "http://localhost", ClientOrigins: []string{"http://127.0.0.1:5173", "https://app.chatxo.local"}, BootstrapUsername: "owner", BootstrapPassword: password, SessionDays: 30, AuthParallel: 4, DataDir: t.TempDir(), FFprobe: "ffprobe", Cloudflared: encoded, MediaMaxPeers: 64, MediaRoomLimit: 16, Policy: config.Policy{TextMaxBytes: 256_000_000, TextRetentionDays: 30, ImageMaxBytes: 2 << 20, VideoMaxBytes: 10 << 20, AttachmentMaxBytes: 1 << 30, AttachmentRetentionDays: 7}}
	if err := postgres.Migrate(ctx, pool, id); err != nil {
		t.Fatal(err)
	}
	ids, err := identity.New(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("HAUNT_CLIENT_CHECK") == "plugin-repository" {
		cfg.AudioPluginGitHubToken = os.Getenv("HAUNT_AUDIO_PLUGIN_TEST_GITHUB_TOKEN")
	}
	if os.Getenv("HAUNT_CLIENT_CHECK") == "public-downloads" {
		cfg.ClientUpdateManifestURL = "https://github.com/example-git/haunt-client/releases/latest/download/haunt-updates.json"
		cfg.ClientUpdateInstallers = true
	}
	features, err := httpapi.NewFeatures(ctx, cfg, pool, ids, log)
	if err != nil {
		t.Fatal(err)
	}
	if mode := os.Getenv("HAUNT_CLIENT_CHECK"); mode == "narrator" || mode == "ui-refresh" {
		features.Extensions.Close()
		features.Extensions = narratorExtension(t, cfg.DataDir)
	}
	{ // Each isolated test service supplies a redistributable, independent audio plugin.
		path := os.Getenv("HAUNT_AUDIO_PLUGIN_TEST_PACKAGE")
		if path == "" {
			path = "../../client/tests/fixtures/audio-plugin/test-audio.zip"
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = features.AudioPlugins.Install(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := features.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	features.Previews.Close()
	features.Previews = fixturePreviews{}
	login, err := ids.Login(ctx, "owner", password, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := ids.Authenticate(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	general, err := ids.SaveChannel(ctx, principal, "", "general", "text", nil)
	if err != nil {
		t.Fatal(err)
	}
	voice, err := ids.SaveChannel(ctx, principal, "", "lounge", "voice", nil)
	if err != nil {
		t.Fatal(err)
	}
	invite, err := ids.CreateInvite(ctx, principal, 20, 24)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ids.Register(ctx, "alice", password, invite.Token, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err = ids.Register(ctx, "bob", password, invite.Token, "fixture"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 105; i++ {
		message, err := features.Chat.Create(ctx, principal, general.ID, identity.NewID(), "Archive message", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("HAUNT_CLIENT_CHECK") == "ui-refresh" {
			// Real historical rows exercise calendar boundaries and pagination.
			now := time.Now().UTC()
			at := time.Date(now.Year(), now.Month(), now.Day()-3+i/40, 12, i%40, 0, 0, time.UTC)
			if _, err := pool.Exec(ctx, `UPDATE chatxo.messages SET created_at=$2 WHERE id=$1`, message.ID, at); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE chatxo.events SET created_at=$2 WHERE message_id=$1`, message.ID, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	if os.Getenv("HAUNT_CLIENT_CHECK") == "ui-refresh" {
		// Expire historical replay through real maintenance, leaving history intact.
		if err := features.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewUnstartedServer(nil)
	cfg.PublicURL = "http://" + srv.Listener.Addr().String()
	strictDesktop := os.Getenv("HAUNT_CLIENT_CHECK") == "desktop-proxy"
	tls := !encoded && (strictDesktop || os.Getenv("HAUNT_CLIENT_CHECK") == "signin-browser")
	if strictDesktop {
		cfg.ClientOrigins = nil
	}
	if tls {
		cfg.PublicURL = "https://" + srv.Listener.Addr().String()
	}
	cfg.ClientOrigins = append(cfg.ClientOrigins, cfg.PublicURL)
	srv.Config.Handler = httpapi.New(cfg, ids, log, features).Handler
	certificate := ""
	if tls {
		srv.StartTLS()
		certificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return fixture{srv.URL, name, id, general.ID, voice.ID, invite.Token, certificate}
}
func TestClients(t *testing.T) {
	if os.Getenv("HAUNT_CLIENT_E2E") != "1" {
		t.Skip("Run npm run test:integration from client/")
	}
	rtc := server(t, "The Common Room", false)
	ws := server(t, "Night Garden", true)
	manifest := filepath.Join(t.TempDir(), "servers.json")
	data, _ := json.Marshal(map[string]any{"rtc": rtc, "ws": ws, "password": password})
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	timeout := 8 * time.Minute
	if raw, present := os.LookupEnv("HAUNT_CALL_SOAK_SECONDS"); present {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 30 || seconds > 1800 || os.Getenv("HAUNT_CLIENT_CHECK") != "source-audio" {
			t.Fatal("call endurance requires source-audio and 30-1800 seconds per transport")
		}
		timeout += 2 * time.Duration(seconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "scripts/run-e2e.mjs", manifest)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 20 * time.Second
	cmd.Dir = filepath.Join("..", "..", "client")
	if directory := os.Getenv("HAUNT_CLIENT_DIR"); directory != "" {
		cmd.Dir = directory
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal("Client runtime checks failed:", err)
	}
}
