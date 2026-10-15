package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/example-git/haunt-server/internal/testutil"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/blobstore"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/voice"
)

func testImage(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func configUpload(t *testing.T, f *fixture, channel string, data []byte, want int) attachments.Attachment {
	t.Helper()
	req, _ := http.NewRequest("POST", f.server.URL+"/api/v1/conversations/"+channel+"/attachments?filename=sample.png&size="+strconv64(int64(len(data))), bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+f.owner.Token)
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != want {
		t.Fatalf("upload: status %d want %d: %s", response.StatusCode, want, raw)
	}
	var a attachments.Attachment
	if want == 201 && json.Unmarshal(raw, &a) != nil {
		t.Fatal("invalid attachment JSON")
	}
	return a
}
func configDownload(t *testing.T, f *fixture, id string, want []byte, byteRange string) {
	t.Helper()
	req, _ := http.NewRequest("GET", f.server.URL+"/api/v1/attachments/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+f.owner.Token)
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	status := 200
	if byteRange != "" {
		status = 206
	}
	if response.StatusCode != status || !bytes.Equal(raw, want) {
		t.Fatalf("download status=%d expected=%d bytes=%d/%d", response.StatusCode, status, len(raw), len(want))
	}
}
func getConfiguration(t *testing.T, f *fixture) ConfigurationResponse {
	t.Helper()
	var c ConfigurationResponse
	f.call(t, "GET", "/api/v1/instance/configuration", f.owner.Token, nil, 200, &c)
	return c
}
func saveConfiguration(t *testing.T, f *fixture, c identity.RuntimeConfiguration) identity.RuntimeConfiguration {
	t.Helper()
	var saved identity.RuntimeConfiguration
	f.call(t, "PUT", "/api/v1/instance/configuration", f.owner.Token, c, 200, &saved)
	return saved
}
func TestRuntimeConfigurationPermissionsPersistenceAndLiveLimits(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	alice := f.register(t, "config_alice")
	channel := textChannel(t, f)
	payload := testImage(t)
	initial := getConfiguration(t, f)
	for _, method := range []string{"GET", "PUT"} {
		f.call(t, method, "/api/v1/instance/configuration", alice.Token, initial.RuntimeConfiguration, 403, nil)
	}
	changed := initial.RuntimeConfiguration
	changed.Settings.Limits.ImageMaxBytes = int64(len(payload) - 1)
	changed.Settings.SessionDays = 2
	changed.Settings.Limits.AttachmentRetentionDays = 3
	saved := saveConfiguration(t, f, changed)
	if saved.Revision != 1 {
		t.Fatal("revision did not advance")
	}
	configUpload(t, f, channel.ID, payload, 415)
	f.call(t, "PUT", "/api/v1/instance/configuration", f.owner.Token, changed, 409, nil)
	bad := saved
	bad.Settings.MediaRoomLimit = 0
	f.call(t, "PUT", "/api/v1/instance/configuration", f.owner.Token, bad, 400, nil)
	f.call(t, "PUT", "/api/v1/instance/configuration", f.owner.Token, map[string]any{"settings": saved.Settings}, 400, nil)
	f.call(t, "PUT", "/api/v1/instance/configuration", f.owner.Token, map[string]any{"revision": saved.Revision, "settings": saved.Settings, "database_url": "bad"}, 400, nil)
	saved.Settings.Limits.ImageMaxBytes = int64(len(payload))
	saved = saveConfiguration(t, f, saved)
	item := configUpload(t, f, channel.ID, payload, 201)
	if time.Until(*item.ExpiresAt) < 71*time.Hour || time.Until(*item.ExpiresAt) > 73*time.Hour {
		t.Fatal("new retention not applied")
	}
	var discovery instanceResponse
	f.call(t, "GET", "/api/v1/instance", "", nil, 200, &discovery)
	if discovery.Limits.ImageMaxBytes != int64(len(payload)) || discovery.Authentication.SessionDays != 2 {
		t.Fatal("discovery is stale")
	}
	next := f.register(t, "config_next")
	if next.Session.ExpiresAt == nil || time.Until(*next.Session.ExpiresAt) > 49*time.Hour {
		t.Fatal("session duration unchanged")
	}
	restarted, err := identity.New(context.Background(), f.cfg, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Runtime(f.cfg.RuntimeDefaults()).Settings != saved.Settings {
		t.Fatal("saved config lost on restart")
	}
	saved.Settings.RegistrationEnabled = false
	saved = saveConfiguration(t, f, saved)
	var invite identity.Invite
	f.call(t, "POST", "/api/v1/invites", f.owner.Token, map[string]int{"uses": 1, "expires_in_hours": 1}, 201, &invite)
	f.call(t, "POST", "/api/v1/auth/register", "", map[string]any{"username": "blocked_registration", "password": testPassword, "device_name": "test", "invitation": invite.Token}, 403, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "config_next", "password": testPassword, "device_name": "still allowed", "remember": true}, 200, &next)
	if next.Session.ExpiresAt != nil {
		t.Fatal("remembered session expired")
	}
	configDownload(t, f, item.ID, payload, "")
	// Concurrent edits have exactly one winner and never roll the in-memory policy back.
	saved.Settings.MediaRoomLimit = 3
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, err := f.request("PUT", "/api/v1/instance/configuration", f.owner.Token, saved)
			if err != nil {
				status = 0
			}
			statuses <- status
		}()
	}
	wg.Wait()
	close(statuses)
	results := map[int]int{}
	for status := range statuses {
		results[status]++
	}
	if results[200] != 1 || results[409] != 1 {
		t.Fatalf("concurrent edits: %v", results)
	}
	if features.Chat.CurrentPolicy() != saved.Settings.Limits {
		t.Fatal("runtime snapshot changed incorrectly")
	}
	var count int
	if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM chatxo.audit_log WHERE action='server.configuration.updated'`).Scan(&count); err != nil || count < 4 {
		t.Fatalf("audit missing: %d %v", count, err)
	}
}
func TestRuntimeConfigurationRetentionBudgetAndAdmission(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	payload := testImage(t)
	a := configUpload(t, f, channel.ID, payload, 201)
	c := getConfiguration(t, f).RuntimeConfiguration
	c.Settings.Limits.ImageMaxBytes = 1
	c.Settings.Limits.VideoMaxBytes = 1
	c.Settings.Limits.AttachmentMaxBytes = int64(len(payload) - 1)
	f.call(t, "PUT", "/api/v1/instance/configuration", f.owner.Token, c, 409, nil)
	// Reducing text retention applies to reads immediately and maintenance reclaims it.
	message := sendMessage(t, f, f.owner.Token, channel.ID, "aged message")
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.messages SET created_at=now()-interval '3 days' WHERE id=$1`, message.ID); err != nil {
		t.Fatal(err)
	}
	c = getConfiguration(t, f).RuntimeConfiguration
	c.Settings.Limits.TextRetentionDays = 1
	c.Settings.MediaRoomLimit = 2
	c = saveConfiguration(t, f, c)
	var history chat.History
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", f.owner.Token, nil, 200, &history)
	if len(history.Messages) != 0 {
		t.Fatal("old message still visible")
	}
	if err := features.Chat.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var exists bool
	_ = f.pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM chatxo.messages WHERE id=$1)`, message.ID).Scan(&exists)
	if exists {
		t.Fatal("retention not enforced")
	}
	configDownload(t, f, a.ID, payload, "")
	alice := f.register(t, "capacity_alice")
	bob := f.register(t, "capacity_bob")
	var room identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]string{"name": "runtime voice", "kind": "voice"}, 201, &room)
	joinVoice(t, f, f.owner.Token, room.ID)
	joinVoice(t, f, alice.Token, room.ID)
	var admission voice.Admission
	f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", bob.Token, nil, 201, &admission)
	conn := wsConnect(t, f, bob.Token)
	wsSend(t, conn, map[string]string{"type": "media.join", "admission_id": admission.ID})
	wsRead(t, conn, "error")
	c.Settings.MediaRoomLimit = 3
	c = saveConfiguration(t, f, c)
	joinVoice(t, f, bob.Token, room.ID)
	c.Settings.MediaRoomLimit = 2
	saveConfiguration(t, f, c)
	var participants participantsResponse
	f.call(t, "GET", "/api/v1/voice/"+room.ID, f.owner.Token, nil, 200, &participants)
	if len(participants.Participants) != 3 {
		t.Fatal("capacity decrease interrupted calls")
	}
}
func TestRuntimeUploadKeepsReservationPolicyAndProvider(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	payload := testImage(t)
	var provider blobstore.Provider
	f.call(t, "POST", "/api/v1/admin/storage", f.owner.Token, blobstore.RegisterRequest{Name: "mounted disk", Configuration: blobstore.Configuration{Kind: "local", Path: t.TempDir()}}, 201, &provider)
	p, err := f.identities.Authenticate(context.Background(), f.owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	var item attachments.Attachment
	go func() {
		var err error
		item, err = features.Attachments.Upload(context.Background(), p, channel.ID, "held.png", int64(len(payload)), reader)
		reader.Close()
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM chatxo.attachments WHERE status='uploading'`).Scan(&n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			writer.Close()
			t.Fatal("upload did not reserve")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := getConfiguration(t, f).RuntimeConfiguration
	c.Settings.Limits.ImageMaxBytes = 1
	saveConfiguration(t, f, c)
	f.call(t, "PUT", "/api/v1/admin/storage/active", f.owner.Token, map[string]string{"provider_id": provider.ID}, 204, nil)
	_, err = writer.Write(payload)
	writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var providerID *string
	if err = f.pool.QueryRow(context.Background(), `SELECT storage_provider_id FROM chatxo.attachments WHERE id=$1`, item.ID).Scan(&providerID); err != nil || providerID != nil {
		t.Fatal("in-flight upload moved providers")
	}
	configDownload(t, f, item.ID, payload, "")
}
func TestMountedStorageSwitchAuthorizationAndRecovery(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	payload := testImage(t)
	original := configUpload(t, f, channel.ID, payload, 201)
	alice := f.register(t, "storage_alice")
	f.call(t, "GET", "/api/v1/admin/storage", alice.Token, nil, 403, nil)
	var p blobstore.Provider
	request := blobstore.RegisterRequest{Name: "NAS mount", Configuration: blobstore.Configuration{Kind: "local", Path: t.TempDir()}}
	f.call(t, "POST", "/api/v1/admin/storage", alice.Token, request, 403, nil)
	f.call(t, "POST", "/api/v1/admin/storage", f.owner.Token, request, 201, &p)
	f.call(t, "PUT", "/api/v1/admin/storage/active", f.owner.Token, map[string]string{"provider_id": p.ID}, 204, nil)
	current := configUpload(t, f, channel.ID, payload, 201)
	configDownload(t, f, original.ID, payload, "")
	configDownload(t, f, current.ID, payload[4:16], "bytes=4-15")
	f.call(t, "DELETE", "/api/v1/admin/storage/"+p.ID, f.owner.Token, nil, 409, nil)
	f.call(t, "PUT", "/api/v1/admin/storage/active", f.owner.Token, map[string]string{"provider_id": ""}, 204, nil)
	f.call(t, "DELETE", "/api/v1/admin/storage/"+p.ID, f.owner.Token, nil, 409, nil)
	restarted, err := blobstore.New(context.Background(), f.pool, f.identities, f.cfg.DataDir, f.cfg.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	features.Attachments.Providers = restarted
	configDownload(t, f, current.ID, payload, "")
	f.call(t, "DELETE", "/api/v1/attachments/"+current.ID, f.owner.Token, nil, 204, nil)
	f.call(t, "DELETE", "/api/v1/admin/storage/"+p.ID, f.owner.Token, nil, 204, nil)
	configDownload(t, f, original.ID, payload, "")
	// Local provider configuration cannot smuggle credentials or relative paths.
	request.Configuration.Path = "../bad"
	f.call(t, "POST", "/api/v1/admin/storage", f.owner.Token, request, 400, nil)
	request.Configuration.Path = t.TempDir()
	request.Credentials.SecretKey = "unexpected"
	f.call(t, "POST", "/api/v1/admin/storage", f.owner.Token, request, 400, nil)
	var body StorageProvidersResponse
	f.call(t, "GET", "/api/v1/admin/storage", f.owner.Token, nil, 200, &body)
	if len(body.Providers) != 0 || !body.DefaultActive || body.DefaultAttachments != 1 {
		t.Fatal("provider removal damaged default storage")
	}
	status, raw, err := f.request("GET", "/api/v1/admin/storage", f.owner.Token, nil)
	if err != nil || status != 200 || strings.Contains(string(raw), "secret_key") {
		t.Fatal("storage listing leaked credentials")
	}
}

func TestS3ProviderSwitchSecretsOutageAndRecovery(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	payload := testImage(t)
	server := testutil.NewObjectStore(t)
	v := blobstore.RegisterRequest{Name: "Private S3", Configuration: blobstore.Configuration{Kind: "s3", Endpoint: server.Server.URL, Region: "us-east-1", Bucket: "test", PathStyle: true}, Credentials: blobstore.Credentials{AccessKey: "s3-test-access", SecretKey: "very-private-s3-secret"}}
	var provider blobstore.Provider
	f.call(t, "POST", "/api/v1/admin/storage", f.owner.Token, v, 201, &provider)
	if server.Count() != 0 {
		t.Fatal("connection test left an object")
	}
	status, raw, err := f.request("GET", "/api/v1/admin/storage", f.owner.Token, nil)
	if err != nil || status != 200 || strings.Contains(string(raw), v.Credentials.SecretKey) || strings.Contains(string(raw), v.Credentials.AccessKey) {
		t.Fatal("API exposed credentials")
	}
	var encrypted []byte
	if err = f.pool.QueryRow(context.Background(), `SELECT credentials FROM chatxo.storage_providers WHERE id=$1`, provider.ID).Scan(&encrypted); err != nil || bytes.Contains(encrypted, []byte(v.Credentials.SecretKey)) {
		t.Fatal("database credentials not encrypted")
	}
	f.call(t, "PUT", "/api/v1/admin/storage/active", f.owner.Token, map[string]string{"provider_id": provider.ID}, 204, nil)
	item := configUpload(t, f, channel.ID, payload, 201)
	if server.Count() != 1 {
		t.Fatal("upload not sent to S3")
	}
	configDownload(t, f, item.ID, payload[8:20], "bytes=8-19")
	// Database and key survive process restart; unavailable remote files remain tracked.
	restarted, err := blobstore.New(context.Background(), f.pool, f.identities, f.cfg.DataDir, f.cfg.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	features.Attachments.Providers = restarted
	server.Fail.Store(true)
	f.call(t, "GET", "/api/v1/attachments/"+item.ID, f.owner.Token, nil, 503, nil)
	f.call(t, "DELETE", "/api/v1/attachments/"+item.ID, f.owner.Token, nil, 503, nil)
	var count int
	var used int64
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM chatxo.attachments WHERE id=$1`, item.ID).Scan(&count)
	_ = f.pool.QueryRow(context.Background(), `SELECT attachment_bytes FROM chatxo.storage WHERE singleton`).Scan(&used)
	if count != 1 || used != int64(len(payload)) {
		t.Fatal("outage lost file metadata or accounting")
	}
	// Reconciliation must start successfully even when cleanup cannot reach S3.
	if err = features.Attachments.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := attachments.New(context.Background(), f.cfg, features.Chat)
	if err != nil {
		t.Fatal(err)
	}
	features.Attachments = recovered
	server.Fail.Store(false)
	if err = features.Attachments.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if server.Count() != 0 {
		t.Fatal("recovered cleanup failed")
	}
	item = configUpload(t, f, channel.ID, payload, 201)
	configDownload(t, f, item.ID, payload, "")
	f.call(t, "PUT", "/api/v1/admin/storage/"+provider.ID+"/credentials", f.owner.Token, blobstore.Credentials{AccessKey: "rotated-access", SecretKey: "rotated-secret"}, 204, nil)
	configDownload(t, f, item.ID, payload, "")
	// Losing the encryption key must not silently generate a replacement.
	keyPath := filepath.Join(f.cfg.DataDir, "storage-provider.key")
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("key permissions")
	}
	key, _ := os.ReadFile(keyPath)
	if err = os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err = blobstore.New(context.Background(), f.pool, f.identities, f.cfg.DataDir, f.cfg.InstanceID); err == nil {
		t.Fatal("silently replaced missing key")
	}
	if err = os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationPermissionIsSeparateFromAppearance(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	admin := f.register(t, "configuration_admin")
	var role identity.Role
	f.call(t, "POST", "/api/v1/roles", f.owner.Token, map[string]any{"name": "appearance", "permissions": identity.ManageServer, "position": 1}, 201, &role)
	f.call(t, "PUT", "/api/v1/members/"+admin.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{role.ID}}, 204, nil)
	f.call(t, "GET", "/api/v1/instance/configuration", admin.Token, nil, 403, nil)
	f.call(t, "PUT", "/api/v1/roles/"+role.ID, f.owner.Token, map[string]any{"name": "configuration", "permissions": identity.ManageConfiguration, "position": 1}, 200, nil)
	var configuration ConfigurationResponse
	f.call(t, "GET", "/api/v1/instance/configuration", admin.Token, nil, 200, &configuration)
	configuration.Settings.SessionDays = 7
	f.call(t, "PUT", "/api/v1/instance/configuration", admin.Token, configuration.RuntimeConfiguration, 200, nil)
	f.call(t, "GET", "/api/v1/admin/storage", admin.Token, nil, 200, nil)
	f.call(t, "PUT", "/api/v1/instance/settings", admin.Token, map[string]string{"name": "unauthorized appearance", "accent_color": "#123456"}, 403, nil)
}
