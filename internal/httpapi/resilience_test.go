package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
)

func TestMessageBoundariesRollbackAndReplayExpiry(t *testing.T) {
	f, features := featureFixture(t, 8192)
	ch := textChannel(t, f)
	alice := f.register(t, "alice")
	path := "/api/v1/conversations/" + ch.ID + "/messages"
	for _, body := range []string{"", "  ", "a\x00b", strings.Repeat("x", 1537), strings.Repeat("\x01", 1200)} {
		f.call(t, "POST", path, alice.Token, map[string]any{"request_id": identity.NewID(), "body": body}, 400, nil)
	}
	first := sendMessage(t, f, alice.Token, ch.ID, "kept")
	if first.Seq != 1 {
		t.Fatal("invalid writes consumed ordering sequence")
	}
	f.call(t, "PUT", path+"/"+first.ID, f.owner.Token, map[string]any{"body": "unauthorized edit"}, 403, nil)
	f.call(t, "PUT", path+"/"+first.ID, alice.Token, map[string]any{"body": strings.Repeat("\x01", 1200)}, 400, nil)
	var history chat.History
	f.call(t, "GET", path+"?after=999", alice.Token, nil, 200, &history)
	if !history.Gap {
		t.Fatal("future history cursor did not report gap")
	}
	f.call(t, "GET", path+"?limit=101", alice.Token, nil, 400, nil)
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.messages SET created_at=now()-interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	events, _, gap, err := features.Chat.Events(context.Background(), 0, []string{ch.ID})
	if err != nil || !gap || len(events) != 0 {
		t.Fatal("expired message leaked through replay", err)
	}
	if err := features.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	accounting(t, f)
}

func TestConcurrentDedupeRetentionAndCancellation(t *testing.T) {
	f, features := featureFixture(t, 32768)
	channels := []identity.Channel{textChannel(t, f), textChannel(t, f), textChannel(t, f)}
	ctx := context.Background()
	// Eight clients retry one request concurrently; exactly one committed ID/seq.
	requestID := identity.NewID()
	var wg sync.WaitGroup
	type result struct {
		status int
		data   []byte
		err    error
	}
	results := make(chan result, 36)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, data, err := f.request("POST", "/api/v1/conversations/"+channels[0].ID+"/messages", f.owner.Token, map[string]any{"request_id": requestID, "body": "once"})
			results <- result{status, data, err}
		}()
	}
	wg.Wait()
	var first string
	for range 8 {
		v := <-results
		if v.err != nil || v.status != 201 {
			t.Fatalf("dedupe retry %d %v", v.status, v.err)
		}
		var m chat.Message
		if json.Unmarshal(v.data, &m) != nil {
			t.Fatal("invalid message")
		}
		if first == "" {
			first = m.ID
		}
		if m.ID != first || m.Seq != 1 {
			t.Fatal("duplicate persisted message")
		}
	}
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, data, err := f.request("POST", "/api/v1/conversations/"+channels[i%3].ID+"/messages", f.owner.Token, map[string]any{"request_id": identity.NewID(), "body": strings.Repeat("q", 1000)})
			results <- result{status, data, err}
		}(i)
	}
	wg.Wait()
	for range 30 {
		v := <-results
		if v.err != nil || v.status != 201 {
			t.Fatalf("concurrent quota write %d %v", v.status, v.err)
		}
	}
	accounting(t, f)
	var gaps int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT count(*) n,max(seq)-min(seq)+1 span FROM chatxo.messages GROUP BY conversation_id) q WHERE n<>span`).Scan(&gaps); err != nil || gaps != 0 {
		t.Fatal("retained history is not a contiguous recent suffix", err)
	}
	// Hold only this test database's quota row. A cancelled write must roll back,
	// while history and readiness continue on other pooled connections.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT singleton FROM chatxo.storage FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"request_id": identity.NewID(), "body": "cancelled-write"})
	cancelCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(cancelCtx, "POST", f.server.URL+"/api/v1/conversations/"+channels[0].ID+"/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.owner.Token)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, _ := f.server.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(time.Second)
	for features.Chat.LockStats()["pending_acquires"].(int64) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	f.call(t, "GET", "/readyz", "", nil, 200, nil)
	f.call(t, "GET", "/api/v1/conversations/"+channels[1].ID+"/messages", f.owner.Token, nil, 200, nil)
	<-done
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := features.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	var cancelled int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.messages WHERE body='cancelled-write'`).Scan(&cancelled); err != nil || cancelled != 0 {
		t.Fatal("cancelled write committed", err)
	}
	accounting(t, f)
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func rawUpload(t *testing.T, f *fixture, token, conv string, data []byte, declared int64, chunked bool) (int, attachments.Attachment) {
	t.Helper()
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/conversations/%s/attachments?filename=upload.bin&size=%d", f.server.URL, conv, declared), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if chunked {
		req.ContentLength = -1
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var item attachments.Attachment
	if resp.StatusCode == 201 {
		if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
			t.Fatal(err)
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode, item
}
func attachmentBytes(t *testing.T, f *fixture) int64 {
	t.Helper()
	var used int64
	if err := f.pool.QueryRow(context.Background(), `SELECT attachment_bytes FROM chatxo.storage`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}

func TestAttachmentBoundariesRangesAndRecovery(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	ch := textChannel(t, f)
	data := pngBytes(t)
	for _, tc := range []struct {
		data    []byte
		size    int64
		chunked bool
		want    int
	}{
		{data, int64(len(data) - 1), false, 400}, {data, int64(len(data) + 1), true, 400}, {data, int64(len(data) - 1), true, 400},
		{[]byte("fake PNG"), 8, false, 415}, {append(data, make([]byte, (2<<20)+1-len(data))...), (2 << 20) + 1, false, 415},
	} {
		status, _ := rawUpload(t, f, f.owner.Token, ch.ID, tc.data, tc.size, tc.chunked)
		if status != tc.want {
			t.Fatalf("attachment boundary %d want %d", status, tc.want)
		}
		if attachmentBytes(t, f) != 0 {
			t.Fatal("failed upload retained reservation")
		}
	}
	status, item := rawUpload(t, f, f.owner.Token, ch.ID, data, int64(len(data)), false)
	if status != 201 {
		t.Fatal("valid image failed", status)
	}
	req, _ := http.NewRequest("GET", f.server.URL+"/api/v1/attachments/"+item.ID, nil)
	req.Header.Set("Authorization", "Bearer "+f.owner.Token)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || !bytes.Equal(part, data[2:6]) {
		t.Fatal("authorized range download failed")
	}
	f.call(t, "GET", "/api/v1/attachments/"+item.ID, "", nil, 401, nil)
	if duplicate, err := attachments.New(context.Background(), f.cfg, features.Chat); err == nil {
		duplicate.Close()
		t.Fatal("two processes acquired the attachment directory")
	}
	// Simulate a crash: unfinished reservation + part file, ready file missing,
	// and an orphan final file. Restart reconciliation must release exactly once.
	conv := ch.ID
	incomplete := identity.NewID()
	orphan := identity.NewID()
	root := filepath.Join(f.cfg.DataDir, "attachments")
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO chatxo.attachments(id,conversation_id,user_id,status,filename,bytes,expires_at) VALUES($1,$2,$3,'uploading','interrupted',10,now()+interval '1 day')`, incomplete, conv, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.storage SET attachment_bytes=attachment_bytes+10`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, incomplete+".part"), []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, orphan), []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, item.ID)); err != nil {
		t.Fatal(err)
	}
	if err := features.Attachments.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := attachments.New(context.Background(), f.cfg, features.Chat)
	if err != nil {
		t.Fatal(err)
	}
	features.Attachments = recovered
	if attachmentBytes(t, f) != 0 {
		t.Fatal("recovery retained lost/orphan/incomplete file bytes")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatal("recovery left files", err)
	}
}

func TestActualVideoUploadAndFailedPhysicalDeletion(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	ch := textChannel(t, f)
	path := filepath.Join(t.TempDir(), "video.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=16x16:r=10", "-t", "0.2", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-y", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate video: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	status, item := rawUpload(t, f, f.owner.Token, ch.ID, data, int64(len(data)), false)
	if status != 201 || item.ContentType != "video/mp4" {
		t.Fatal("actual MP4 rejected", status)
	}
	// A filesystem deletion failure must retain its reservation until retry.
	file := filepath.Join(f.cfg.DataDir, "attachments", item.ID)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(file, "busy"), []byte("block deletion"), 0600); err != nil {
		t.Fatal(err)
	}
	f.call(t, "DELETE", "/api/v1/attachments/"+item.ID, f.owner.Token, nil, 500, nil)
	if attachmentBytes(t, f) != int64(len(data)) {
		t.Fatal("failed deletion released storage")
	}
	f.call(t, "GET", "/api/v1/attachments/"+item.ID, f.owner.Token, nil, 404, nil)
	if err := os.Remove(filepath.Join(file, "busy")); err != nil {
		t.Fatal(err)
	}
	if err := features.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attachmentBytes(t, f) != 0 {
		t.Fatal("cleanup did not release capacity after deletion")
	}
}

func TestWebSocketAuthenticationGapsRevocationAndShutdown(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	ch := textChannel(t, f)
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	var dm chat.Conversation
	f.call(t, "POST", "/api/v1/dms", alice.Token, map[string]any{"members": []string{bob.User.ID}}, 201, &dm)
	owner := wsConnect(t, f, f.owner.Token)
	wsSend(t, owner, map[string]any{"type": "subscribe", "conversations": []string{dm.ID}})
	wsRead(t, owner, "error")
	wsSend(t, owner, map[string]any{"type": "subscribe", "conversations": []string{ch.ID}, "after_event": 99999})
	wsRead(t, owner, "subscribed")
	wsRead(t, owner, "sync.required")
	conn := wsConnect(t, f, alice.Token)
	f.call(t, "DELETE", "/api/v1/sessions/"+alice.Session.ID, alice.Token, nil, 204, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("revoked event session stayed connected")
	}
	// Closing the gateway drains active handlers and rejects new admission.
	if err := features.Gateway.close(ctx); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/api/v1/events", "", nil, 503, nil)
	if features.Gateway.stats()["connections"].(int) != 0 {
		t.Fatal("gateway leaked connections")
	}
	bad, _, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "http://", "ws://", 1)+"/api/v1/events", nil)
	if err == nil {
		bad.CloseNow()
		t.Fatal("closed gateway admitted a socket")
	}
}

func TestLargeRetentionBatchCapReductionAndDirectoryPagination(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	ch := textChannel(t, f)
	f.call(t, "GET", "/api/v1/conversations/"+ch.ID+"/messages", f.owner.Token, nil, 200, nil)
	ctx := context.Background()
	// Seed a substantial committed history in the isolated fixture, then use
	// the real retention transaction to expire it within the maintenance budget.
	if _, err := f.pool.Exec(ctx, `INSERT INTO chatxo.messages(id,conversation_id,author_id,seq,body,created_at,request_id,request_hash,bytes) SELECT gen_random_uuid(),$1,$2,n,repeat('x',1000),now()-interval '31 days'+n*interval '1 microsecond',gen_random_uuid(),decode(repeat('00',32),'hex'),1500 FROM generate_series(1,3000) n`, ch.ID, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE chatxo.storage SET text_bytes=4500000; UPDATE chatxo.conversations SET last_seq=3000`); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	started := time.Now()
	if err := features.Maintain(bounded); err != nil {
		t.Fatal("large age prune failed", err)
	}
	t.Logf("expired 3000 messages in %s", time.Since(started))
	accounting(t, f)
	var remaining int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.messages`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("large age prune left rows", remaining, err)
	}
	for range 15 {
		sendMessage(t, f, f.owner.Token, ch.ID, strings.Repeat("x", 1000))
	}
	smaller := f.cfg
	smaller.Policy.TextMaxBytes = 8192
	reduced, err := chat.New(f.pool, f.identities, smaller)
	if err != nil {
		t.Fatal(err)
	}
	if err := reduced.Maintain(ctx); err != nil {
		t.Fatal("cap reduction failed", err)
	}
	var used int64
	if err := f.pool.QueryRow(ctx, `SELECT text_bytes FROM chatxo.storage`).Scan(&used); err != nil || used > 8192 {
		t.Fatal("new text cap not enforced", used, err)
	}
	// Profile pagination exposes no password/session/role data to members.
	alice := f.register(t, "alice")
	if _, err := f.pool.Exec(ctx, `INSERT INTO chatxo.users(id,instance_id,username,password_hash) SELECT gen_random_uuid(),$1,'seed_'||n,'no-login' FROM generate_series(1,105) n`, f.cfg.InstanceID); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Users []identity.User `json:"users"`
	}
	f.call(t, "GET", "/api/v1/users", alice.Token, nil, 200, &page)
	if len(page.Users) != 100 {
		t.Fatal("user directory not paginated")
	}
	after := page.Users[99].ID
	f.call(t, "GET", "/api/v1/users?after="+after, alice.Token, nil, 200, &page)
	if len(page.Users) != 7 {
		t.Fatal("user directory second page wrong", len(page.Users))
	}
	f.call(t, "GET", "/api/v1/users/"+f.owner.User.ID, alice.Token, nil, 200, nil)
	f.call(t, "GET", "/api/v1/users/"+identity.NewID(), alice.Token, nil, 404, nil)
	f.call(t, "GET", "/api/v1/users?after=bad", alice.Token, nil, 400, nil)
	f.call(t, "GET", "/api/v1/conversations?after=bad", alice.Token, nil, 400, nil)
}

func TestLargeMessageReplayBatchesDoNotSkipEvents(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	ch := textChannel(t, f)
	for range 7 {
		sendMessage(t, f, f.owner.Token, ch.ID, strings.Repeat("<", 65536))
	}
	var cursor int64
	seen := map[string]bool{}
	for attempts := 0; attempts < 10; attempts++ {
		events, next, gap, err := features.Chat.Events(context.Background(), cursor, []string{ch.ID})
		if err != nil || gap {
			t.Fatal("large replay failed", err)
		}
		size := 0
		for _, data := range events {
			size += len(data)
			var event struct {
				Data chat.Message `json:"data"`
			}
			if json.Unmarshal(data, &event) != nil {
				t.Fatal("invalid replay")
			}
			if seen[event.Data.ID] {
				t.Fatal("duplicate replay")
			}
			seen[event.Data.ID] = true
		}
		if size > 1<<20 {
			t.Fatal("replay memory bound exceeded")
		}
		if len(events) == 0 {
			break
		}
		if next <= cursor {
			t.Fatal("replay did not advance")
		}
		cursor = next
	}
	if len(seen) != 7 {
		t.Fatal("byte-bounded replay skipped events", len(seen))
	}
	accounting(t, f)
}
