package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/blobstore"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/testutil"
)

func TestCompiledServerRestartCrashBackupRestoreAndOwnerRecovery(t *testing.T) {
	source, instance := testutil.Database(t)
	restoreDB, _ := testutil.Database(t)
	sourceURL := source.Config().ConnConfig.ConnString()
	// The backup tool deliberately accepts URI form only.
	uri := func(database string) string {
		return fmt.Sprintf("postgres://postgres@127.0.0.1:%d/%s?sslmode=disable", source.Config().ConnConfig.Port, database)
	}
	sourceURL = uri(instance)
	targetURL := uri(restoreDB.Config().ConnConfig.Database)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	binary := os.Getenv("HAUNT_TEST_SERVER_BINARY")
	if binary == "" {
		binary = filepath.Join(scratch, "chatxovg")
		build := exec.Command("go", "build", "-race", "-o", binary, "./cmd/haunt")
		build.Dir = root
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build server: %v %s", err, output)
		}
	}
	serverCommand := func(args ...string) *exec.Cmd {
		if runner := os.Getenv("HAUNT_TEST_SERVER_RUNNER"); runner != "" {
			return exec.Command(runner, append([]string{binary}, args...)...)
		}
		return exec.Command(binary, args...)
	}
	if output, err := serverCommand("-version").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "0.1.0" {
		t.Fatal("version command failed")
	}
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listen.Addr().String()
	listen.Close()
	base := "http://" + address
	dataDir := filepath.Join(scratch, "source-data")
	env := []string{}
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(key, "HAUNT_") && !strings.HasPrefix(key, "PG") {
			env = append(env, item)
		}
	}
	environment := func(database, data, bootstrap string) []string {
		values := append([]string{}, env...)
		values = append(values, "HAUNT_INSTANCE_ID="+instance, "HAUNT_PUBLIC_URL="+base, "HAUNT_LISTEN_ADDR="+address, "HAUNT_DATABASE_URL="+database, "HAUNT_DATA_DIR="+data, "HAUNT_FFPROBE=ffprobe")
		if bootstrap != "" {
			values = append(values, "HAUNT_BOOTSTRAP_USERNAME=owner", "HAUNT_BOOTSTRAP_PASSWORD="+bootstrap)
		}
		return values
	}
	password := "a process test owner password"
	var logPaths []string
	type process struct {
		cmd  *exec.Cmd
		done chan struct{}
		err  error
	}
	start := func(values []string, expectReady bool) *process {
		logPath := filepath.Join(scratch, fmt.Sprintf("server-%d.log", len(logPaths)))
		logPaths = append(logPaths, logPath)
		log, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		p := &process{cmd: serverCommand(), done: make(chan struct{})}
		p.cmd.Env = values
		p.cmd.Stdout = log
		p.cmd.Stderr = log
		if err := p.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { p.err = p.cmd.Wait(); log.Close(); close(p.done) }()
		t.Cleanup(func() {
			select {
			case <-p.done:
			default:
				p.cmd.Process.Kill()
				<-p.done
			}
		})
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-p.done:
				if expectReady {
					output, _ := os.ReadFile(logPath)
					t.Fatalf("server exited early: %v %s", p.err, output)
				}
				if p.err == nil {
					t.Fatal("invalid startup succeeded")
				}
				return p
			default:
			}
			if expectReady {
				resp, err := http.Get(base + "/readyz")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == 200 {
						return p
					}
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("process did not reach expected startup state")
		return nil
	}
	stop := func(p *process, signal os.Signal) {
		if err := p.cmd.Process.Signal(signal); err != nil {
			t.Fatal(err)
		}
		select {
		case <-p.done:
		case <-time.After(20 * time.Second):
			t.Fatal("server failed bounded shutdown")
		}
		if signal == syscall.SIGTERM && p.err != nil {
			t.Fatal("graceful shutdown failed", p.err)
		}
	}
	request := func(method, path, token string, body any, want int, out any) {
		var reader io.Reader
		if body != nil {
			data, _ := json.Marshal(body)
			reader = bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, base+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s %s status %d want %d", method, path, resp.StatusCode, want)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		} else {
			io.Copy(io.Discard, resp.Body)
		}
	}
	start(environment(sourceURL, dataDir, ""), false)
	p := start(environment(sourceURL, dataDir, password), true)
	var owner identity.Login
	request("POST", "/api/v1/auth/login", "", map[string]any{"username": "owner", "password": password, "device_name": "process-test"}, 200, &owner)
	var ch identity.Channel
	request("POST", "/api/v1/channels", owner.Token, map[string]any{"name": "persisted", "kind": "text"}, 201, &ch)
	var pngData bytes.Buffer
	png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	// Profile art lives in PostgreSQL, independently of attachment retention and
	// process memory. Verify both metadata and bytes across real process restarts.
	avatarRequest, _ := http.NewRequest("PUT", base+"/api/v1/me/avatar", bytes.NewReader(pngData.Bytes()))
	avatarRequest.Header.Set("Authorization", "Bearer "+owner.Token)
	avatarResponse, err := http.DefaultClient.Do(avatarRequest)
	if err != nil {
		t.Fatal(err)
	}
	var avatarVersion struct {
		Version string `json:"version"`
	}
	decodeErr := json.NewDecoder(avatarResponse.Body).Decode(&avatarVersion)
	avatarResponse.Body.Close()
	if avatarResponse.StatusCode != 200 || decodeErr != nil || avatarVersion.Version == "" {
		t.Fatal("avatar upload failed", decodeErr)
	}
	checkAvatar := func() {
		t.Helper()
		var me identity.Principal
		request("GET", "/api/v1/me", owner.Token, nil, 200, &me)
		if me.User.AvatarVersion != avatarVersion.Version {
			t.Fatal("restart changed avatar metadata")
		}
		r, _ := http.NewRequest("GET", base+"/api/v1/users/"+owner.User.ID+"/avatar", nil)
		r.Header.Set("Authorization", "Bearer "+owner.Token)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || !bytes.Equal(data, pngData.Bytes()) {
			t.Fatal("restart changed avatar bytes", err)
		}
	}
	checkAvatar()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/conversations/%s/attachments?size=%d&filename=image.png", base, ch.ID, pngData.Len()), bytes.NewReader(pngData.Bytes()))
	req.Header.Set("Authorization", "Bearer "+owner.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var item attachments.Attachment
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatal("process attachment failed")
	}
	err = json.NewDecoder(resp.Body).Decode(&item)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var message chat.Message
	request("POST", "/api/v1/conversations/"+ch.ID+"/messages", owner.Token, map[string]any{"request_id": identity.NewID(), "body": "survive restart and backup", "attachment_ids": []string{item.ID}}, 201, &message)
	// A graceful stop must also terminate an upgraded authenticated connection.
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wsCancel()
	socket, _, err := websocket.Dial(wsCtx, strings.Replace(base, "http:", "ws:", 1)+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	auth, _ := json.Marshal(map[string]any{"type": "authenticate", "token": owner.Token})
	if err := socket.Write(wsCtx, websocket.MessageText, auth); err != nil {
		t.Fatal(err)
	}
	if _, _, err := socket.Read(wsCtx); err != nil {
		t.Fatal(err)
	}
	backupRoot := filepath.Join(scratch, "backups")
	backup := func(operation, path, database, data string, success bool) string {
		cmd := exec.Command("python3", filepath.Join(root, "scripts/backup.py"), operation, path, "--keep", "2")
		cmd.Env = append(environment(database, data, ""), "PGHOSTADDR=192.0.2.99", "PGSERVICE=must-not-be-used")
		output, err := cmd.CombinedOutput()
		if success && err != nil {
			t.Fatalf("%s failed: %v %s", operation, err, output)
		}
		if !success && err == nil {
			t.Fatalf("unsafe %s unexpectedly succeeded", operation)
		}
		return strings.TrimSpace(string(output))
	}
	// Register an external provider so the offline backup must retain its key,
	// and confirm a restored process can still decrypt and use those credentials.
	objectStore := testutil.NewObjectStore(t)
	var external blobstore.Provider
	request("POST", "/api/v1/admin/storage", owner.Token, blobstore.RegisterRequest{Name: "backup object store", Configuration: blobstore.Configuration{Kind: "s3", Endpoint: objectStore.Server.URL, Region: "us-east-1", Bucket: "test", PathStyle: true}, Credentials: blobstore.Credentials{AccessKey: "backup-access", SecretKey: "backup-secret"}}, 201, &external)
	pluginData, err := os.ReadFile(filepath.Join(root, "client/tests/fixtures/audio-plugin/test-audio.zip"))
	if err != nil {
		t.Fatal(err)
	}
	pluginRequest, _ := http.NewRequest("POST", base+"/api/v1/admin/audio-plugins", bytes.NewReader(pluginData))
	pluginRequest.Header.Set("Authorization", "Bearer "+owner.Token)
	pluginResponse, err := http.DefaultClient.Do(pluginRequest)
	if err != nil {
		t.Fatal(err)
	}
	pluginResponse.Body.Close()
	if pluginResponse.StatusCode != 201 {
		t.Fatal("plugin install", pluginResponse.StatusCode)
	}
	backup("backup", backupRoot, sourceURL, dataDir, false) // lock held by the server
	stop(p, syscall.SIGTERM)
	p = start(environment(sourceURL, dataDir, ""), true)
	request("GET", "/api/v1/me", owner.Token, nil, 200, nil)
	checkAvatar()
	// Abrupt loss after commit preserves acknowledged history and token state.
	stop(p, syscall.SIGKILL)
	p = start(environment(sourceURL, dataDir, ""), true)
	var history chat.History
	checkAvatar()
	request("GET", "/api/v1/conversations/"+ch.ID+"/messages", owner.Token, nil, 200, &history)
	if len(history.Messages) != 1 || history.Messages[0].ID != message.ID {
		t.Fatal("restart lost committed history")
	}
	// Exercise the actual idle maintenance loop without an administrative call
	// or another write waking retention. Reads already hide expired content.
	var expiring chat.Message
	request("POST", "/api/v1/conversations/"+ch.ID+"/messages", owner.Token, map[string]any{"request_id": identity.NewID(), "body": "expire without traffic"}, 201, &expiring)
	if _, err := source.Exec(context.Background(), `UPDATE chatxo.messages SET created_at=now()-interval '31 days' WHERE id=$1`, expiring.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(context.Background(), `INSERT INTO chatxo.sessions(id,instance_id,user_id,token_hash,device_name,expires_at) VALUES(gen_random_uuid(),$1,$2,decode(repeat('00',32),'hex'),'expired-idle-test',now()-interval '1 second')`, instance, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(70 * time.Second)
	for {
		var pending int
		if err := source.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM chatxo.messages WHERE id=$1)+(SELECT count(*) FROM chatxo.sessions WHERE device_name='expired-idle-test')`, expiring.ID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle maintenance did not physically expire records")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Log("actual one-minute idle maintenance removed expired messages and sessions")
	stop(p, syscall.SIGTERM)
	var latest string
	for range 3 {
		latest = backup("backup", backupRoot, sourceURL, dataDir, true)
	}
	dirs, err := os.ReadDir(backupRoot)
	if err != nil || len(dirs) != 2 {
		t.Fatal("backup rotation did not retain two snapshots", err)
	}
	targetDir := filepath.Join(scratch, "restored-data")
	dump := filepath.Join(latest, "database.dump")
	original, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dump, append(append([]byte{}, original...), 1), 0600); err != nil {
		t.Fatal(err)
	}
	backup("restore", latest, targetURL, targetDir, false)
	if err := os.WriteFile(dump, original, 0600); err != nil {
		t.Fatal(err)
	}
	backup("restore", latest, targetURL, targetDir, true)
	originalKey, err := os.ReadFile(filepath.Join(dataDir, "storage-provider.key"))
	if err != nil {
		t.Fatal(err)
	}
	restoredKey, err := os.ReadFile(filepath.Join(targetDir, "storage-provider.key"))
	if err != nil || !bytes.Equal(originalKey, restoredKey) {
		t.Fatal("backup lost provider encryption key")
	}

	restoredPlugin, err := os.ReadFile(filepath.Join(targetDir, "audio-plugins/test-audio.zip"))
	if err != nil || !bytes.Equal(restoredPlugin, pluginData) {
		t.Fatal("backup lost audio plugin")
	}
	backup("restore", latest, targetURL, filepath.Join(scratch, "empty-other-dir"), false) // populated DB refusal
	p = start(environment(targetURL, targetDir, ""), true)
	request("POST", "/api/v1/admin/storage/"+external.ID+"/check", owner.Token, nil, 204, nil)
	request("GET", "/api/v1/me", owner.Token, nil, 200, nil)
	checkAvatar()
	request("GET", "/api/v1/conversations/"+ch.ID+"/messages", owner.Token, nil, 200, &history)
	if len(history.Messages) != 1 || history.Messages[0].Body != message.Body {
		t.Fatal("restore lost messages")
	}
	req, _ = http.NewRequest("GET", base+"/api/v1/attachments/"+item.ID, nil)
	req.Header.Set("Authorization", "Bearer "+owner.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, pngData.Bytes()) {
		t.Fatal("restore lost attachment bytes")
	}
	stop(p, syscall.SIGTERM)
	recovery := filepath.Join(scratch, "owner-password")
	replacement := "a recovered process password"
	if err := os.WriteFile(recovery, []byte(replacement+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reset := serverCommand("-owner-password-file", recovery)
	reset.Env = environment(targetURL, targetDir, "")
	if output, err := reset.CombinedOutput(); err != nil {
		t.Fatalf("owner CLI recovery: %v %s", err, output)
	}
	p = start(environment(targetURL, targetDir, ""), true)
	request("GET", "/api/v1/me", owner.Token, nil, 401, nil)
	request("POST", "/api/v1/auth/login", "", map[string]any{"username": "owner", "password": replacement, "device_name": "recovered"}, 200, nil)
	stop(p, syscall.SIGTERM)
	for _, path := range logPaths {
		output, _ := os.ReadFile(path)
		for _, secret := range []string{password, replacement, owner.Token} {
			if strings.Contains(string(output), secret) {
				t.Fatal("process logged a credential")
			}
		}
		if strings.Contains(string(output), "DATA RACE") {
			t.Fatal("race in compiled server")
		}
	}
}
