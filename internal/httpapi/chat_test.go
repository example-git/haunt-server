package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
)

func featureFixture(t *testing.T, textCap int64, configure ...func(*config.Config)) (*fixture, *Features) {
	t.Helper()
	f := newFixture(t)
	f.server.Close()
	f.cfg.Policy = config.Policy{TextMaxBytes: textCap, TextRetentionDays: 30, ImageMaxBytes: 2 << 20, VideoMaxBytes: 10 << 20, AttachmentMaxBytes: 1 << 30, AttachmentRetentionDays: 7}
	f.cfg.DataDir = t.TempDir()
	f.cfg.FFprobe = "ffprobe"
	for _, apply := range configure {
		apply(&f.cfg)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	features, err := NewFeatures(context.Background(), f.cfg, f.pool, f.identities, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := features.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	f.server = httptest.NewUnstartedServer(nil)
	f.cfg.ClientOrigins = append(f.cfg.ClientOrigins, "http://"+f.server.Listener.Addr().String())
	f.server.Config.Handler = New(f.cfg, f.identities, logger, features).Handler
	f.server.Start()
	t.Cleanup(f.server.Close)
	return f, features
}
func textChannel(t *testing.T, f *fixture) identity.Channel {
	t.Helper()
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "general", "kind": "text"}, 201, &channel)
	return channel
}
func sendMessage(t *testing.T, f *fixture, token, conv, body string) chat.Message {
	t.Helper()
	var message chat.Message
	f.call(t, "POST", "/api/v1/conversations/"+conv+"/messages", token, map[string]any{"body": body, "request_id": identity.NewID()}, 201, &message)
	return message
}
func accounting(t *testing.T, f *fixture) {
	t.Helper()
	var stored, actual, dm, dmActual int64
	if err := f.pool.QueryRow(context.Background(), `SELECT text_bytes,(SELECT COALESCE(sum(bytes),0) FROM chatxo.messages)+(SELECT COALESCE(sum(bytes),0) FROM chatxo.events),dm_text_bytes,(SELECT COALESCE(sum(m.bytes),0) FROM chatxo.messages m JOIN chatxo.conversations c ON c.id=m.conversation_id WHERE c.kind='dm') FROM chatxo.storage WHERE singleton`).Scan(&stored, &actual, &dm, &dmActual); err != nil {
		t.Fatal(err)
	}
	if stored != actual || dm != dmActual || stored-dm > f.cfg.Policy.TextMaxBytes {
		t.Fatalf("text accounting stored=%d actual=%d cap=%d", stored, actual, f.cfg.Policy.TextMaxBytes)
	}
}

func TestRejectedSecondProcessCannotPruneHistory(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	message := sendMessage(t, f, f.owner.Token, channel.ID, strings.Repeat("x", 16000))
	cfg := f.cfg
	cfg.Policy.TextMaxBytes = 8192
	other, err := NewFeatures(context.Background(), cfg, f.pool, f.identities, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		other.Close(context.Background())
		t.Fatal("second process accepted the active attachment directory")
	}
	var exists bool
	if err := f.pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM chatxo.messages WHERE id=$1)`, message.ID).Scan(&exists); err != nil || !exists {
		t.Fatalf("rejected startup removed active history: exists=%v err=%v", exists, err)
	}
	accounting(t, f)
}

func TestMessagesDMIsolationAndConcurrentOrdering(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	var dm chat.Conversation
	f.call(t, "POST", "/api/v1/dms", alice.Token, map[string]any{"members": []string{bob.User.ID}}, 201, &dm)
	var same chat.Conversation
	f.call(t, "POST", "/api/v1/dms", bob.Token, map[string]any{"members": []string{alice.User.ID}}, 201, &same)
	if same.ID != dm.ID {
		t.Fatal("DM participant set created duplicate conversations")
	}
	sendMessage(t, f, alice.Token, dm.ID, "private message")
	f.call(t, "GET", "/api/v1/conversations/"+dm.ID+"/messages", f.owner.Token, nil, 404, nil)
	f.call(t, "GET", "/api/v1/conversations/"+dm.ID+"/messages", bob.Token, nil, 200, nil)
	message := sendMessage(t, f, alice.Token, channel.ID, "original")
	request := map[string]any{"body": "original", "request_id": message.RequestID}
	var retry chat.Message
	f.call(t, "POST", "/api/v1/conversations/"+channel.ID+"/messages", alice.Token, request, 201, &retry)
	if retry.ID != message.ID || retry.Seq != message.Seq {
		t.Fatal("retry was not idempotent")
	}
	request["body"] = "changed request"
	f.call(t, "POST", "/api/v1/conversations/"+channel.ID+"/messages", alice.Token, request, 409, nil)
	f.call(t, "PUT", "/api/v1/conversations/"+channel.ID+"/messages/"+message.ID, bob.Token, map[string]any{"body": "steal edit"}, 403, nil)
	var edited chat.Message
	f.call(t, "PUT", "/api/v1/conversations/"+channel.ID+"/messages/"+message.ID, alice.Token, map[string]any{"body": "edited"}, 200, &edited)
	if !edited.CreatedAt.Equal(message.CreatedAt) || edited.Seq != message.Seq {
		t.Fatal("edit refreshed acceptance age/order")
	}
	f.call(t, "DELETE", "/api/v1/conversations/"+channel.ID+"/messages/"+message.ID, alice.Token, nil, 200, &edited)
	if !edited.Deleted || edited.Body != "" {
		t.Fatal("delete did not create a content-free tombstone")
	}
	// Writes through separate HTTP connections must produce unique committed
	// ordering keys while another conversation remains independent.
	statuses := make(chan int, 12)
	failures := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, err := f.request("POST", "/api/v1/conversations/"+channel.ID+"/messages", bob.Token, map[string]any{"body": "parallel", "request_id": identity.NewID()})
			statuses <- status
			failures <- err
		}()
	}
	wg.Wait()
	close(statuses)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	for status := range statuses {
		if status != 201 {
			t.Fatalf("parallel write returned %d", status)
		}
	}
	var history chat.History
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", bob.Token, nil, 200, &history)
	if len(history.Messages) != 13 {
		t.Fatalf("got %d messages", len(history.Messages))
	}
	for i, m := range history.Messages {
		if m.Seq != int64(i+1) {
			t.Fatal("noncontiguous conversation ordering")
		}
	}
	accounting(t, f)
}

func TestStrictOldestFirstRetentionAndAgeExpiry(t *testing.T) {
	f, features := featureFixture(t, 8192)
	channel := textChannel(t, f)
	bob := f.register(t, "bob")
	var dm chat.Conversation
	f.call(t, "POST", "/api/v1/dms", f.owner.Token, map[string]any{"members": []string{bob.User.ID}}, 201, &dm)
	messages := []chat.Message{}
	for i := 0; i < 12; i++ {
		conv := channel.ID
		if i%2 == 1 {
			conv = dm.ID
		}
		messages = append(messages, sendMessage(t, f, f.owner.Token, conv, strings.Repeat("x", 900)))
		accounting(t, f)
	}
	rows, err := f.pool.Query(context.Background(), `SELECT id FROM chatxo.messages WHERE conversation_id=$1 ORDER BY created_at,id`, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	retained := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		retained = append(retained, id)
	}
	rows.Close()
	channelMessages := []chat.Message{}
	for _, m := range messages {
		if m.ConversationID == channel.ID {
			channelMessages = append(channelMessages, m)
		}
	}
	if len(retained) == 0 || len(retained) >= len(channelMessages) {
		t.Fatal("quota did not retain a bounded recent suffix")
	}
	for i, id := range retained {
		if id != channelMessages[len(channelMessages)-len(retained)+i].ID {
			t.Fatal("pruning skipped an older victim")
		}
	}
	var history chat.History
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages?after=0", f.owner.Token, nil, 200, &history)
	if !history.Gap || history.PrunedThrough == 0 {
		t.Fatal("retention gap missing")
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.messages SET created_at=now()-interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", f.owner.Token, nil, 200, &history)
	if len(history.Messages) != 0 {
		t.Fatal("expired messages readable before maintenance")
	}
	if err := features.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	accounting(t, f)
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM chatxo.messages`).Scan(&count); err != nil || count != 6 {
		t.Fatalf("channel expiry should preserve 6 indefinite DM rows, got %d rows: %v", count, err)
	}
}

func wsSend(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	b, _ := json.Marshal(value)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}
func wsRead(t *testing.T, conn *websocket.Conn, want string) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]json.RawMessage
		if json.Unmarshal(b, &v) != nil {
			t.Fatal("invalid event")
		}
		var kind string
		json.Unmarshal(v["type"], &kind)
		if kind == want {
			return v
		}
	}
}
func wsConnect(t *testing.T, f *fixture, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "http://", "ws://", 1)+"/api/v1/events", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://client.example.test"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	wsSend(t, conn, map[string]any{"type": "authenticate", "token": token})
	wsRead(t, conn, "ready")
	return conn
}

func TestWebSocketCommittedUpdatesAndPermissionLoss(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	alice := f.register(t, "alice")
	conn := wsConnect(t, f, alice.Token)
	wsSend(t, conn, map[string]any{"type": "subscribe", "conversations": []string{channel.ID}})
	wsRead(t, conn, "subscribed")
	message := sendMessage(t, f, f.owner.Token, channel.ID, "live message")
	event := wsRead(t, conn, "message.created")
	var received chat.Message
	if json.Unmarshal(event["data"], &received) != nil || received.ID != message.ID {
		t.Fatal("incorrect committed event")
	}
	conn.CloseNow()
	var history chat.History
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", alice.Token, nil, 200, &history)
	next := sendMessage(t, f, f.owner.Token, channel.ID, "while disconnected")
	reconnect := wsConnect(t, f, alice.Token)
	wsSend(t, reconnect, map[string]any{"type": "subscribe", "conversations": []string{channel.ID}, "after_event": history.EventCursor})
	wsRead(t, reconnect, "subscribed")
	event = wsRead(t, reconnect, "message.created")
	if json.Unmarshal(event["data"], &received) != nil || received.ID != next.ID {
		t.Fatal("persisted replay lost committed message")
	}
	var roles struct {
		Roles []identity.Role `json:"roles"`
	}
	f.call(t, "GET", "/api/v1/roles", f.owner.Token, nil, 200, &roles)
	f.call(t, "PUT", "/api/v1/channels/"+channel.ID+"/permissions/"+roles.Roles[0].ID, f.owner.Token, map[string]any{"allow": 0, "deny": 1}, 204, nil)
	wsRead(t, reconnect, "subscription.revoked")
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", alice.Token, nil, 404, nil)
}

func TestAttachmentContentQuotaAndExpiry(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	payload := imageBytes.Bytes()
	upload := func(data []byte) (int, attachments.Attachment, error) {
		path := f.server.URL + "/api/v1/conversations/" + channel.ID + "/attachments?size=" + strconv64(int64(len(data))) + "&filename=" + url.QueryEscape("image.png")
		req, err := http.NewRequest("POST", path, bytes.NewReader(data))
		if err != nil {
			return 0, attachments.Attachment{}, err
		}
		req.Header.Set("Authorization", "Bearer "+f.owner.Token)
		resp, err := f.server.Client().Do(req)
		if err != nil {
			return 0, attachments.Attachment{}, err
		}
		defer resp.Body.Close()
		var a attachments.Attachment
		if resp.StatusCode == 201 {
			err = json.NewDecoder(resp.Body).Decode(&a)
		}
		return resp.StatusCode, a, err
	}
	status, item, err := upload(payload)
	if err != nil || status != 201 {
		t.Fatalf("valid image upload got %d: %v", status, err)
	}
	req, _ := http.NewRequest("GET", f.server.URL+"/api/v1/attachments/"+item.ID, nil)
	req.Header.Set("Authorization", "Bearer "+f.owner.Token)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(data, payload) || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatal("download content mismatch")
	}
	var metadata attachments.Attachment
	f.call(t, "GET", "/api/v1/attachments/"+item.ID+"/metadata", f.owner.Token, nil, 200, &metadata)
	if metadata.ID != item.ID || metadata.ContentType != "image/png" {
		t.Fatal("attachment metadata mismatch")
	}
	f.call(t, "GET", "/api/v1/attachments/"+item.ID+"/metadata", "", nil, 401, nil)
	status, _, err = upload([]byte("pretend-image"))
	if err != nil || status != 415 {
		t.Fatalf("spoofed media got %d: %v", status, err)
	}
	var message chat.Message
	f.call(t, "POST", "/api/v1/conversations/"+channel.ID+"/messages", f.owner.Token, map[string]any{"body": "attached", "request_id": identity.NewID(), "attachment_ids": []string{item.ID}}, 201, &message)
	f.call(t, "POST", "/api/v1/conversations/"+channel.ID+"/messages", f.owner.Token, map[string]any{"body": "reuse", "request_id": identity.NewID(), "attachment_ids": []string{item.ID}}, 400, nil)
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.attachments SET expires_at=now()-interval '1 second' WHERE id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/api/v1/attachments/"+item.ID, f.owner.Token, nil, 404, nil)
	f.call(t, "GET", "/api/v1/attachments/"+item.ID+"/metadata", f.owner.Token, nil, 404, nil)
	if err := features.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var used int64
	if err := f.pool.QueryRow(context.Background(), `SELECT attachment_bytes FROM chatxo.storage WHERE singleton`).Scan(&used); err != nil || used != 0 {
		t.Fatalf("failed/expired uploads retained %d capacity: %v", used, err)
	}
	// Change the test service's immutable policy before issuing parallel HTTP
	// requests; exactly one reservation fits, regardless of upload scheduling.
	features.Chat.Policy.AttachmentMaxBytes = int64(len(payload))
	results := make(chan int, 2)
	failures := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); status, _, err := upload(payload); results <- status; failures <- err }()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[201] != 1 || counts[507] != 1 {
		t.Fatalf("concurrent attachment quota returned %v", counts)
	}
}

func strconv64(n int64) string { return fmt.Sprintf("%d", n) }
