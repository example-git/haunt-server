package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/example-git/haunt-server/internal/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testAdapter struct {
	mu        sync.Mutex
	requests  []provider.Request
	generate  func(context.Context, provider.Request, func(provider.Event) error) (provider.Response, error)
	refreshes int
	exchanges int
}

func (*testAdapter) ID() provider.ID { return provider.Claude }
func (*testAdapter) Models(context.Context, provider.Credentials) ([]provider.Model, error) {
	return []provider.Model{{ID: "vision", Name: "Vision", Vision: true, ContextWindow: 200000, MaxOutputTokens: 8192}, {ID: "text", Name: "Text", Vision: false}}, nil
}
func (*testAdapter) OAuthConfig() provider.OAuthConfig {
	return provider.OAuthConfig{AuthorizationURL: "https://example.test/authorize", ClientID: "client", RedirectMode: "loopback-dynamic"}
}
func (a *testAdapter) Exchange(context.Context, provider.OAuthExchange) (provider.Credentials, error) {
	a.mu.Lock()
	a.exchanges++
	a.mu.Unlock()
	return provider.Credentials{AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (a *testAdapter) Refresh(context.Context, provider.Credentials) (provider.Credentials, error) {
	a.mu.Lock()
	a.refreshes++
	a.mu.Unlock()
	return provider.Credentials{AccessToken: "new-test-access", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (a *testAdapter) Generate(ctx context.Context, r provider.Request, emit func(provider.Event) error) (provider.Response, error) {
	a.mu.Lock()
	a.requests = append(a.requests, r)
	a.mu.Unlock()
	if a.generate != nil {
		return a.generate(ctx, r, emit)
	}
	if err := emit(provider.Event{Type: "text_delta", Text: "answer"}); err != nil {
		return provider.Response{}, err
	}
	return provider.Response{Text: "answer", Replay: []json.RawMessage{json.RawMessage(`{"text":"answer"}`)}}, nil
}

type aiFixture struct {
	s       *Service
	ch      *chat.Store
	ids     *identity.Service
	p       identity.Principal
	channel identity.Channel
	adapter *testAdapter
	cfg     config.Config
}

func fixture(t *testing.T, limit int64) *aiFixture {
	t.Helper()
	pool, name := testutil.Database(t)
	ctx := context.Background()
	cfg := config.Config{InstanceID: name, DataDir: t.TempDir(), FFprobe: "ffprobe", BootstrapUsername: "owner", BootstrapPassword: "test passphrase", SessionDays: 30, AuthParallel: 2, Policy: config.Policy{TextMaxBytes: 16 << 20, TextRetentionDays: 30, AIChannelMaxBytes: limit, AITextMaxBytes: limit * 2, ImageMaxBytes: 2 << 20, VideoMaxBytes: 10 << 20, AttachmentMaxBytes: 16 << 20, AttachmentRetentionDays: 7}}
	if err := postgres.Migrate(ctx, pool, name); err != nil {
		t.Fatal(err)
	}
	ids, err := identity.New(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	login, err := ids.Login(ctx, "owner", "test passphrase", "test", true)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ids.Authenticate(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := chat.New(pool, ids, cfg)
	if err != nil {
		t.Fatal(err)
	}
	att, err := attachments.New(ctx, cfg, ch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { att.Close() })
	adapter := &testAdapter{}
	s, err := New(ctx, pool, ids, ch, att, Options{InstanceID: name, DataDir: cfg.DataDir, ChannelMaxBytes: limit, GlobalMaxBytes: limit * 2}, map[provider.ID]provider.Adapter{provider.Claude: adapter})
	if err != nil {
		t.Fatal(err)
	}
	ch.AI = s
	t.Cleanup(s.Close)
	channel, err := ids.SaveChannel(ctx, p, "", "assistant", "ai", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.PrepareOAuth(ctx, p, "https://haunt.test", OAuthPrepareInput{Provider: "claude", RedirectURI: "http://localhost:4545/callback"})
	if err != nil {
		t.Fatal(err)
	}
	account, err := s.CompleteOAuth(ctx, p, "https://haunt.test", session.ID, OAuthCompleteInput{Code: "test-code", State: session.State})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveSettings(ctx, p, channel.ID, SettingsInput{AccountID: account.ID, Provider: "claude", Model: "vision", Name: "Assistant", MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	return &aiFixture{s: s, ch: ch, ids: ids, p: p, channel: channel, adapter: adapter, cfg: cfg}
}
func (f *aiFixture) send(t *testing.T, body string) chat.Message {
	t.Helper()
	m, err := f.ch.Create(context.Background(), f.p, f.channel.ID, identity.NewID(), body, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func (f *aiFixture) claim(t *testing.T) claimedTurn {
	t.Helper()
	turn, err := f.s.claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return turn
}
func (f *aiFixture) accounting(t *testing.T) {
	t.Helper()
	var stored, actual int64
	err := f.s.pool.QueryRow(context.Background(), `SELECT text_bytes,(SELECT COALESCE(sum(bytes),0) FROM chatxo.messages)+(SELECT COALESCE(sum(bytes),0) FROM chatxo.events)+(SELECT COALESCE(sum(bytes),0) FROM chatxo.ai_turns)+(SELECT COALESCE(sum(bytes),0) FROM chatxo.ai_turn_steps)+(SELECT COALESCE(sum(bytes),0) FROM chatxo.ai_channel_settings)+(SELECT COALESCE(sum(bytes),0) FROM chatxo.ai_presets)+(SELECT COALESCE(sum(bytes),0) FROM chatxo.ai_avatars) FROM chatxo.storage WHERE singleton`).Scan(&stored, &actual)
	if err != nil {
		t.Fatal(err)
	}
	if stored != actual {
		t.Fatalf("storage accounting %d != %d", stored, actual)
	}
}
func TestWorkerSnapshotAttributionAndEarlierResponse(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	first := f.send(t, "first human")
	running := f.claim(t)
	second := f.send(t, "second human")
	f.s.execute(ctx, running)
	next := f.claim(t)
	a, c, err := f.s.credentials(ctx, next.snapshot.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.context(ctx, f.p, next, a, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 3 || r.Messages[0].Author.MessageID != first.ID || r.Messages[1].Role != "assistant" || r.Messages[2].Author.MessageID != second.ID {
		t.Fatalf("queued boundary omitted prior completion: %#v", r.Messages)
	}
	if r.StructuralInstruction == "" || r.Messages[0].Author.UserID != f.p.User.ID || r.Messages[0].Author.Username != "owner" {
		t.Fatal("missing structural attribution")
	}
	f.send(t, "future human")
	f.s.execute(ctx, next)
	f.adapter.mu.Lock()
	requests := append([]provider.Request{}, f.adapter.requests...)
	f.adapter.mu.Unlock()
	for _, message := range requests[1].Messages {
		for _, part := range message.Parts {
			if part.Text == "future human" {
				t.Fatal("future human leaked into queued request")
			}
		}
	}
	runs, err := f.s.Runs(ctx, f.p, f.channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runs[1].Status != "completed" || runs[2].Status != "completed" {
		t.Fatalf("worker did not complete: %#v", runs)
	}
	f.accounting(t)
}
func TestRestartDoesNotRetryAmbiguousExecutionAndExplicitRetryPreservesPartial(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	f.send(t, "trigger")
	turn := f.claim(t)
	if err := f.s.checkpoint(ctx, turn, "partial"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.recover(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := f.s.Runs(ctx, f.p, f.channel.ID)
	if runs[0].Status != "interrupted" {
		t.Fatal("restart retried ambiguous turn")
	}
	if _, err := f.s.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("ambiguous turn eligible for automatic retry")
	}
	if _, err := f.s.Retry(ctx, f.p, f.channel.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	retry := f.claim(t)
	if retry.responseRequest == turn.responseRequest {
		t.Fatal("retry reuses original partial response")
	}
	f.s.execute(ctx, retry)
	var count int
	err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.messages WHERE conversation_id=$1 AND author_id IS NULL AND body IN ('partial','answer')`, f.channel.ID).Scan(&count)
	if err != nil || count != 2 {
		t.Fatalf("partial response was overwritten: count=%d err=%v", count, err)
	}
	f.accounting(t)
}
func TestHistoryEpochRejectsLatePersistence(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	m := f.send(t, "original")
	turn := f.claim(t)
	if _, err := f.ch.Edit(ctx, f.p, f.channel.ID, m.ID, "edited", false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.checkpoint(ctx, turn, "stale answer"); !errors.Is(err, ErrHistoryChanged) {
		t.Fatalf("late output accepted: %v", err)
	}
	var count int
	f.s.pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.messages WHERE author_id IS NULL`).Scan(&count)
	if count != 0 {
		t.Fatal("stale response persisted")
	}
	f.accounting(t)
}
func TestRefreshIsSerializedAndPreservesRefreshToken(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	settings, _ := f.s.Settings(ctx, f.p, f.channel.ID)
	sealed, err := f.s.encryptCredentials(settings.AccountID, provider.Credentials{AccessToken: "old", RefreshToken: "retained-token", ExpiresAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.pool.Exec(ctx, `UPDATE chatxo.ai_accounts SET credentials=$2,expires_at=now()-interval '1 minute' WHERE id=$1`, settings.AccountID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, c, err := f.s.credentials(ctx, settings.AccountID)
			if err != nil || c.RefreshToken != "retained-token" {
				t.Error("refresh lost token or failed")
			}
		}()
	}
	wg.Wait()
	if f.adapter.refreshes != 1 {
		t.Fatalf("refresh calls=%d", f.adapter.refreshes)
	}
	if err := os.Remove(filepath.Join(f.cfg.DataDir, "ai-provider.key")); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.loadKey(ctx, f.cfg.DataDir); err == nil {
		t.Fatal("missing key silently regenerated")
	}
}
func TestOAuthStateOriginSingleUse(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	session, err := f.s.PrepareOAuth(ctx, f.p, "https://one.test", OAuthPrepareInput{Provider: "claude", RedirectURI: "http://localhost:4545/callback"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.CompleteOAuth(ctx, f.p, "https://two.test", session.ID, OAuthCompleteInput{Code: "code", State: session.State}); err == nil {
		t.Fatal("wrong origin accepted")
	}
	if _, err = f.s.CompleteOAuth(ctx, f.p, "https://one.test", session.ID, OAuthCompleteInput{Code: "code", State: "wrong"}); err == nil {
		t.Fatal("wrong state accepted")
	}
	if _, err = f.s.CompleteOAuth(ctx, f.p, "https://one.test", session.ID, OAuthCompleteInput{Code: "code", State: session.State}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.CompleteOAuth(ctx, f.p, "https://one.test", session.ID, OAuthCompleteInput{Code: "code", State: session.State}); err == nil {
		t.Fatal("completed setup reused")
	}
}
func TestAIReadsDoNotNotifyDirectory(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	listener, err := f.s.pool.(*pgxpool.Pool).Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err = listener.Exec(ctx, `LISTEN chatxo_gateway`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = f.s.Settings(ctx, f.p, f.channel.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.Usage(ctx, f.p, f.channel.ID); err != nil {
			t.Fatal(err)
		}
	}
	wait, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	notification, err := listener.Conn().WaitForNotification(wait)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AI reads emitted a gateway notification: notification=%+v error=%v", notification, err)
	}
	channel, err := f.ids.SaveChannel(ctx, f.p, "", "new-assistant", "ai", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Settings(ctx, f.p, channel.ID); err != nil {
		t.Fatal(err)
	}
	wait, cancel = context.WithTimeout(ctx, time.Second)
	defer cancel()
	notification, err = listener.Conn().WaitForNotification(wait)
	if err != nil || notification.Payload != "directory" {
		t.Fatalf("new channel did not notify directory: notification=%+v error=%v", notification, err)
	}
}

func TestRepeatedRetentionAndActiveReservation(t *testing.T) {
	f := fixture(t, 64<<10)
	ctx := context.Background()
	before := f.physical(t)
	var beforeRelations int64
	var beforeWAL string
	f.s.pool.QueryRow(ctx, `SELECT sum(pg_total_relation_size(oid)),pg_current_wal_lsn()::text FROM pg_class WHERE relnamespace='chatxo'::regnamespace AND relkind='r'`).Scan(&beforeRelations, &beforeWAL)
	for i := 0; i < 45; i++ {
		if i == 22 {
			restarted, err := New(ctx, f.s.pool, f.ids, f.ch, f.s.attachments, f.s.options, f.s.adapters)
			if err != nil {
				t.Fatal(err)
			}
			if err = restarted.recover(ctx); err != nil {
				t.Fatal(err)
			}
			f.s = restarted
			f.ch.AI = restarted
			t.Logf("storage at service restart: %+v", f.physical(t))
		}
		f.send(t, strings.Repeat("history ", 120))
		turn := f.claim(t)
		f.s.execute(ctx, turn)
		u, err := f.s.Usage(ctx, f.p, f.channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if u.ChannelBytes > u.ChannelMaxBytes {
			t.Fatalf("channel exceeded budget: %#v", u)
		}
		f.accounting(t)
	}
	u, _ := f.s.Usage(ctx, f.p, f.channel.ID)
	var afterRelations, walBytes int64
	f.s.pool.QueryRow(ctx, `SELECT sum(pg_total_relation_size(oid)),pg_wal_lsn_diff(pg_current_wal_lsn(),$1::pg_lsn)::bigint FROM pg_class WHERE relnamespace='chatxo'::regnamespace AND relkind='r'`, beforeWAL).Scan(&afterRelations, &walBytes)
	after := f.physical(t)
	t.Logf("physical storage baseline=%+v after=%+v; WAL delta is cluster-wide and includes concurrent test fixtures", before, after)
	t.Logf("45 fill/prune cycles: logical=%d cap=%d, database tables+indexes baseline=%d after=%d, WAL bytes generated=%d", u.ChannelBytes, u.ChannelMaxBytes, beforeRelations, afterRelations, walBytes)
	if u.PrunedThrough == 0 || u.HistoryEpoch == 0 {
		t.Fatal("fill did not prune coherent prefix")
	}
	if _, err := f.s.pool.Exec(ctx, `VACUUM (ANALYZE) chatxo.messages,chatxo.events,chatxo.ai_turns,chatxo.ai_turn_steps`); err != nil {
		t.Fatal(err)
	}
	vacuumed := f.physical(t)
	for i := 0; i < 45; i++ {
		f.send(t, strings.Repeat("history ", 120))
		current := f.claim(t)
		f.s.execute(ctx, current)
		usage, err := f.s.Usage(ctx, f.p, f.channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if usage.ChannelBytes > usage.ChannelMaxBytes {
			t.Fatal("second cycle exceeded logical limit")
		}
		f.accounting(t)
	}
	second := f.physical(t)
	t.Logf("post-vacuum physical=%+v; after another 45 cycles=%+v; heap delta=%d index delta=%d toast delta=%d blob delta=%d", vacuumed, second, second.Heap-vacuumed.Heap, second.Indexes-vacuumed.Indexes, second.TOAST-vacuumed.TOAST, second.Blobs-vacuumed.Blobs)
	f.send(t, "active bounded reservation")
	turn := f.claim(t)
	oversize := strings.Repeat("x", 65536)
	if err := f.s.checkpoint(ctx, turn, oversize); !errors.Is(err, ErrStorage) {
		t.Fatalf("oversized checkpoint error=%v", err)
	}
	f.s.fail(ctx, turn, ErrStorage)
	runs, _ := f.s.Runs(ctx, f.p, f.channel.ID)
	if runs[0].Status != "failed" || runs[0].ErrorCode != "ai_storage_full" {
		t.Fatalf("missing visible storage error: %#v", runs[0])
	}
	f.accounting(t)
}
func TestAuthorizedImageInputAndUnsupportedModel(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	var content bytes.Buffer
	if err := png.Encode(&content, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	att, err := f.s.attachments.Upload(ctx, f.p, f.channel.ID, "image.png", int64(content.Len()), bytes.NewReader(content.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.ch.Create(ctx, f.p, f.channel.ID, identity.NewID(), "look", "", []string{att.ID}); err != nil {
		t.Fatal(err)
	}
	turn := f.claim(t)
	a, c, _ := f.s.credentials(ctx, turn.snapshot.AccountID)
	r, err := f.s.context(ctx, f.p, turn, a, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 1 || len(r.Messages[0].Parts) != 2 || !bytes.Equal(r.Messages[0].Parts[1].Data, content.Bytes()) {
		t.Fatal("image bytes/order missing")
	}
	turn.snapshot.Model = "text"
	if _, err = f.s.context(ctx, f.p, turn, a, c); err == nil {
		t.Fatal("image silently dropped for unsupported model")
	}
	retained, err := f.s.attachments.Upload(ctx, f.p, f.channel.ID, "retained.png", int64(content.Len()), bytes.NewReader(content.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.ch.Create(ctx, f.p, f.channel.ID, identity.NewID(), "keep this image", "", []string{retained.ID}); err != nil {
		t.Fatal(err)
	}
	before := f.physical(t)
	if err = f.s.transaction(ctx, func(tx pgx.Tx) error { return f.ch.PruneAIHistory(ctx, tx, f.channel.ID, turn.boundary) }); err != nil {
		t.Fatal(err)
	}
	if err = f.s.attachments.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	after := f.physical(t)
	if after.Blobs >= before.Blobs || after.Blobs == 0 {
		t.Fatalf("blob cleanup removed wrong ownership: before=%d after=%d", before.Blobs, after.Blobs)
	}
	if _, err = f.s.attachments.Metadata(ctx, f.p, att.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("pruned attachment remains readable: %v", err)
	}
	if _, err = f.s.attachments.Metadata(ctx, f.p, retained.ID); err != nil {
		t.Fatalf("retained attachment removed: %v", err)
	}
	t.Logf("pruned attachment cleanup: blob bytes %d -> %d with newer image preserved", before.Blobs, after.Blobs)
	f.accounting(t)
}

type testTools struct{ calls int }

func (*testTools) Schemas() []provider.Tool {
	return []provider.Tool{{Name: "read_url", Parameters: json.RawMessage(`{"type":"object"}`)}}
}
func (t *testTools) Execute(context.Context, provider.ToolCall) (provider.ToolResult, error) {
	t.calls++
	return provider.ToolResult{ID: "call", Name: "read_url", Content: "upstream blocked", IsError: true}, errors.New("upstream blocked")
}
func TestToolContinuationPreservesAttributionAndReleasesTurnMemory(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	f.s.tools = &testTools{}
	n := 0
	f.adapter.generate = func(ctx context.Context, r provider.Request, emit func(provider.Event) error) (provider.Response, error) {
		n++
		if r.StructuralInstruction == "" || len(r.Messages) == 0 || r.Messages[0].Author == nil {
			t.Error("continuation lost author")
		}
		if n == 1 {
			if err := emit(provider.Event{Type: "text_delta", Text: "same"}); err != nil {
				return provider.Response{}, err
			}
			return provider.Response{Text: "same", Calls: []provider.ToolCall{{ID: "call", Name: "read_url", Arguments: json.RawMessage(`{}`)}}, Replay: []json.RawMessage{json.RawMessage(`{"first":"native"}`)}}, nil
		}
		if len(r.Replay) != 1 || len(r.ToolResults) != 1 || !r.ToolResults[0].IsError {
			t.Fatal("tool failure did not reach native continuation")
		}
		return provider.Response{Text: "same", Replay: []json.RawMessage{json.RawMessage(`{"first":"native"}`), json.RawMessage(`{"second":"native"}`)}}, nil
	}
	f.send(t, "read URL")
	turn := f.claim(t)
	f.s.execute(ctx, turn)
	runs, _ := f.s.Runs(ctx, f.p, f.channel.ID)
	if runs[0].Status != "completed" || runs[0].PartialText != "samesame" {
		t.Fatalf("continuation output mismatch: %#v", runs[0])
	}
	var rows int
	err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.ai_turn_steps WHERE turn_id=$1`, turn.ID).Scan(&rows)
	if err != nil || rows != 0 {
		t.Fatalf("completed response retained tool payloads: %d %v", rows, err)
	}
	f.accounting(t)
}
func TestPresetSnapshotAndRevokedSessionPreventProviderExecution(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	preset, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "Selected persona", Instructions: "selected instructions"})
	if err != nil {
		t.Fatal(err)
	}
	f.send(t, "first")
	first := f.claim(t)
	f.send(t, "<@ai:"+preset.ID+"> second")
	if first.snapshot.Author.Name != "Assistant" {
		t.Fatal("running preset changed")
	}
	f.s.execute(ctx, first)
	next := f.claim(t)
	if next.snapshot.Author.Name != "Selected persona" || next.snapshot.PresetInstructions != "selected instructions" {
		t.Fatal("selecting message did not use selected preset")
	}
	if err = f.ids.RevokeSession(ctx, f.p, f.p.SessionID); err != nil {
		t.Fatal(err)
	}
	f.s.execute(ctx, next)
	if len(f.adapter.requests) != 1 {
		t.Fatal("revoked session still executed provider")
	}
	var status, code string
	if err = f.s.pool.QueryRow(ctx, `SELECT status,error_code FROM chatxo.ai_turns WHERE id=$1`, next.ID).Scan(&status, &code); err != nil || status != "failed" || code != "permission_revoked" {
		t.Fatalf("missing permission failure: %s/%s %v", status, code, err)
	}
}
func TestAccountDisconnectCancelsActiveAndReleasesReservation(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	f.send(t, "pending")
	turn := f.claim(t)
	if err := f.s.Disconnect(ctx, f.p, turn.snapshot.AccountID); err != nil {
		t.Fatal(err)
	}
	var status string
	var reserve int64
	if err := f.s.pool.QueryRow(ctx, `SELECT status,reservation_bytes FROM chatxo.ai_turns WHERE id=$1`, turn.ID).Scan(&status, &reserve); err != nil || status != "requires_auth" || reserve != 0 {
		t.Fatalf("disconnect retained active reservation: %s/%d %v", status, reserve, err)
	}
	f.s.execute(ctx, turn)
	if len(f.adapter.requests) != 0 {
		t.Fatal("disconnected account executed")
	}
	f.accounting(t)
}

type physicalStorage struct {
	Heap    int64
	Indexes int64
	TOAST   int64
	Blobs   int64
}

func (f *aiFixture) physical(t *testing.T) physicalStorage {
	t.Helper()
	var v physicalStorage
	if err := f.s.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(pg_relation_size(oid)),0),COALESCE(sum(pg_indexes_size(oid)),0),COALESCE(sum(CASE WHEN reltoastrelid=0 THEN 0 ELSE pg_total_relation_size(reltoastrelid) END),0) FROM pg_class WHERE relnamespace='chatxo'::regnamespace AND relkind='r'`).Scan(&v.Heap, &v.Indexes, &v.TOAST); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(f.cfg.DataDir, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == ".lock" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		v.Blobs += info.Size()
	}
	return v
}
func TestWorkerLifecycleSurvivesInitializationContext(t *testing.T) {
	f := fixture(t, 2<<20)
	startup, cancelStartup := context.WithCancel(context.Background())
	s, err := New(startup, f.s.pool, f.ids, f.ch, f.s.attachments, f.s.options, f.s.adapters)
	if err != nil {
		t.Fatal(err)
	}
	cancelStartup()
	f.ch.AI = s
	f.s = s
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f.send(t, "process lifetime")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		turns, err := s.Runs(context.Background(), f.p, f.channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) == 1 && turns[0].Status == "completed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker retained cancelled initialization context")
}

func TestHistoricalAvatarVersionsSurviveReplacementAndPresetDeletion(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	preset, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "Persona"})
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value uint8) []byte {
		var raw bytes.Buffer
		im := image.NewRGBA(image.Rect(0, 0, 8, 8))
		im.Set(0, 0, color.RGBA{R: value, A: 255})
		if err := png.Encode(&raw, im); err != nil {
			t.Fatal(err)
		}
		return raw.Bytes()
	}
	url, err := f.s.SaveAvatar(ctx, f.p, f.channel.ID, preset.ID, encode(100))
	if err != nil {
		t.Fatal(err)
	}
	_, version, _ := strings.Cut(url, "?v=")
	original, err := f.s.Avatar(ctx, f.p, f.channel.ID, preset.ID, version)
	if err != nil {
		t.Fatal(err)
	}
	f.send(t, "<@ai:"+preset.ID+"> hello")
	turn := f.claim(t)
	f.s.execute(ctx, turn)
	replacement, err := f.s.SaveAvatar(ctx, f.p, f.channel.ID, preset.ID, encode(200))
	if err != nil {
		t.Fatal(err)
	}
	if replacement == url {
		t.Fatal("avatar replacement URL unchanged")
	}
	if err = f.s.DeletePreset(ctx, f.p, f.channel.ID, preset.ID); err != nil {
		t.Fatal(err)
	}
	historical, err := f.s.Avatar(ctx, f.p, f.channel.ID, preset.ID, version)
	if err != nil || !bytes.Equal(original, historical) {
		t.Fatal("historical avatar changed or vanished")
	}
	var last int64
	if err = f.s.pool.QueryRow(ctx, `SELECT last_seq FROM chatxo.conversations WHERE id=$1`, f.channel.ID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if err = f.s.transaction(ctx, func(tx pgx.Tx) error { return f.ch.PruneAIHistory(ctx, tx, f.channel.ID, last) }); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.s.pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.ai_avatars WHERE channel_id=$1`, f.channel.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unreferenced avatar retained: %d %v", count, err)
	}
	f.accounting(t)
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(what)
	}
}

func TestCancelRetryFencesOldAttemptAndWaitsForProviderExit(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls, active, maxActive int
	var mu sync.Mutex
	f.adapter.generate = func(ctx context.Context, _ provider.Request, emit func(provider.Event) error) (provider.Response, error) {
		mu.Lock()
		calls++
		call := calls
		active++
		maxActive = max(maxActive, active)
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		if call == 1 {
			if err := emit(provider.Event{Type: "text_delta", Text: "old partial"}); err != nil {
				return provider.Response{}, err
			}
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-release
			emit(provider.Event{Type: "text_delta", Text: "late stale output"})
			return provider.Response{}, ctx.Err()
		}
		return provider.Response{Text: "new answer"}, nil
	}
	f.send(t, "retry trigger")
	old := f.claim(t)
	done := make(chan struct{})
	go func() { f.s.execute(ctx, old); close(done) }()
	awaitSignal(t, entered, "first provider request did not start")
	if _, err := f.s.Cancel(ctx, f.p, f.channel.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, cancelled, "cancel did not reach provider")
	if _, err := f.s.Retry(ctx, f.p, f.channel.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("retried channel claimed before old provider exited: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	awaitSignal(t, done, "old provider did not exit")
	next := f.claim(t)
	if err := f.s.checkpoint(ctx, old, "stale persisted text"); err == nil {
		t.Fatal("old attempt persisted into retry")
	}
	f.s.finish(ctx, old, "failed", "stale_failure", nil)
	var status, code string
	if err := f.s.pool.QueryRow(ctx, `SELECT status,error_code FROM chatxo.ai_turns WHERE id=$1`, next.ID).Scan(&status, &code); err != nil || status != "running" || code != "" {
		t.Fatalf("old finish mutated retry: %s/%s %v", status, code, err)
	}
	f.s.execute(ctx, next)
	var oldText, newText string
	if err := f.s.pool.QueryRow(ctx, `SELECT body FROM chatxo.messages WHERE request_id=$1 AND author_id IS NULL`, old.responseRequest).Scan(&oldText); err != nil {
		t.Fatal(err)
	}
	if err := f.s.pool.QueryRow(ctx, `SELECT body FROM chatxo.messages WHERE request_id=$1 AND author_id IS NULL`, next.responseRequest).Scan(&newText); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if oldText != "old partial" || newText != "new answer" || maxActive != 1 || calls != 2 {
		t.Fatalf("retry provenance or serialization failed: old=%q new=%q calls=%d max_active=%d", oldText, newText, calls, maxActive)
	}
	f.accounting(t)
}

func TestAuthoritativeChangesCancelBlockedProvider(t *testing.T) {
	for _, action := range []string{"disconnect", "edit", "delete"} {
		t.Run(action, func(t *testing.T) {
			f := fixture(t, 2<<20)
			ctx := context.Background()
			entered := make(chan struct{})
			cancelled := make(chan struct{})
			f.adapter.generate = func(ctx context.Context, _ provider.Request, _ func(provider.Event) error) (provider.Response, error) {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				return provider.Response{}, ctx.Err()
			}
			message := f.send(t, "blocked request")
			turn := f.claim(t)
			parent, stop := context.WithCancel(ctx)
			defer stop()
			done := make(chan struct{})
			go func() { f.s.execute(parent, turn); close(done) }()
			awaitSignal(t, entered, "provider did not start")
			var err error
			switch action {
			case "disconnect":
				err = f.s.Disconnect(ctx, f.p, turn.snapshot.AccountID)
			case "edit":
				_, err = f.ch.Edit(ctx, f.p, f.channel.ID, message.ID, "replacement", false)
			case "delete":
				_, err = f.ch.Edit(ctx, f.p, f.channel.ID, message.ID, "", true)
			}
			if err != nil {
				t.Fatal(err)
			}
			awaitSignal(t, cancelled, "authoritative change did not cancel blocked provider")
			awaitSignal(t, done, "cancelled provider execution did not exit")
			var status string
			var remaining int64
			if err = f.s.pool.QueryRow(ctx, `SELECT status,reservation_bytes FROM chatxo.ai_turns WHERE id=$1`, turn.ID).Scan(&status, &remaining); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"disconnect": "requires_auth", "edit": "interrupted", "delete": "cancelled"}[action]
			if status != want || remaining != 0 {
				t.Fatalf("old request changed authoritative state: %s reserve=%d", status, remaining)
			}
			f.accounting(t)
		})
	}
}

func TestDeletedQueuedTriggerNeverExecutes(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	message := f.send(t, "queued trigger")
	if _, err := f.ch.Edit(ctx, f.p, f.channel.ID, message.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted trigger still eligible: %v", err)
	}
	runs, err := f.s.Runs(ctx, f.p, f.channel.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != "cancelled" || runs[0].ErrorCode != "trigger_deleted" {
		t.Fatalf("missing deleted trigger state: %#v %v", runs, err)
	}
	if len(f.adapter.requests) != 0 {
		t.Fatal("deleted trigger reached provider")
	}
	f.accounting(t)
}

func TestExpiredAccountReconnectionNotifiesAndMissingKeyFailsClosed(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	settings, err := f.s.Settings(ctx, f.p, f.channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := f.s.encryptCredentials(settings.AccountID, provider.Credentials{AccessToken: "expired", ExpiresAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.pool.Exec(ctx, `UPDATE chatxo.ai_accounts SET credentials=$2,expires_at=now()-interval '1 minute' WHERE id=$1`, settings.AccountID, sealed); err != nil {
		t.Fatal(err)
	}
	f.send(t, "queued before expiry")
	if _, _, err = f.s.credentials(ctx, settings.AccountID); err == nil {
		t.Fatal("expired account without refresh token succeeded")
	}
	before, err := f.s.Settings(ctx, f.p, f.channel.ID)
	if err != nil || before.ConnectionStatus != "requires_auth" || before.Revision <= settings.Revision {
		t.Fatalf("missing expired account notification: %#v %v", before, err)
	}
	session, err := f.s.PrepareOAuth(ctx, f.p, "https://haunt.test", OAuthPrepareInput{Provider: "claude", RedirectURI: "http://localhost:4545/callback", AccountID: settings.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.CompleteOAuth(ctx, f.p, "https://haunt.test", session.ID, OAuthCompleteInput{Code: "new-code", State: session.State}); err != nil {
		t.Fatal(err)
	}
	after, err := f.s.Settings(ctx, f.p, f.channel.ID)
	if err != nil || after.ConnectionStatus != "connected" || after.Revision <= before.Revision {
		t.Fatalf("missing reconnection notification: %#v %v", after, err)
	}
	f.accounting(t)
	key := filepath.Join(f.cfg.DataDir, "ai-provider.key")
	if err = os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if _, err = New(ctx, f.s.pool, f.ids, f.ch, f.s.attachments, f.s.options, f.s.adapters); err == nil {
		t.Fatal("credential key was silently regenerated")
	}
	if _, err = os.Stat(key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key failure created replacement: %v", err)
	}
}

func TestExactCapWorkerAlwaysPublishesTerminalState(t *testing.T) {
	f := fixture(t, 64<<10)
	ctx := context.Background()
	settings, err := f.s.Settings(ctx, f.p, f.channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	var responses sync.Map
	f.adapter.generate = func(ctx context.Context, request provider.Request, _ func(provider.Event) error) (provider.Response, error) {
		if err := f.s.transaction(ctx, func(tx pgx.Tx) error {
			var used int64
			if err := tx.QueryRow(ctx, channelUsageSQL, request.ThreadID).Scan(&used); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET max_bytes=$2 WHERE channel_id=$1`, request.ThreadID, used)
			return err
		}); err != nil {
			return provider.Response{}, err
		}
		value, ok := responses.Load(request.ThreadID)
		if !ok {
			return provider.Response{}, errors.New("missing fixture response")
		}
		return provider.Response{Text: value.(string)}, nil
	}
	if err = f.s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	completed, storageFailed := 0, 0
	for size := 300; size <= 1100; size += 100 {
		channel, err := f.ids.SaveChannel(ctx, f.p, "", "capacity", "ai", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.SaveSettings(ctx, f.p, channel.ID, settings.SettingsInput); err != nil {
			t.Fatal(err)
		}
		f.channel = channel
		responses.Store(channel.ID, strings.Repeat("<", size))
		f.send(t, "answer at exact storage capacity")
		deadline := time.Now().Add(5 * time.Second)
		var terminal Turn
		for time.Now().Before(deadline) {
			runs, err := f.s.Runs(ctx, f.p, channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) == 1 && (runs[0].Status == "completed" || runs[0].Status == "failed") {
				terminal = runs[0]
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if terminal.ID == "" {
			t.Fatalf("output size %d remained nonterminal at exact capacity", size)
		}
		if terminal.Status == "completed" {
			completed++
		} else {
			if terminal.ErrorCode != "ai_storage_full" {
				t.Fatalf("output size %d unexpected failure: %#v", size, terminal)
			}
			storageFailed++
		}
		var visible bool
		if err = f.s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.events WHERE conversation_id=$1 AND convert_from(payload,'UTF8')::jsonb->>'type'='ai.turn.updated' AND convert_from(payload,'UTF8')::jsonb->'data'->>'status'=$2)`, channel.ID, terminal.Status).Scan(&visible); err != nil || !visible {
			t.Fatalf("terminal state missing from events: %v", err)
		}
		usage, err := f.s.Usage(ctx, f.p, channel.ID)
		if err != nil || usage.ChannelBytes > usage.ChannelMaxBytes {
			t.Fatalf("terminal state exceeded quota: %#v %v", usage, err)
		}
		f.accounting(t)
		var last int64
		if err = f.s.pool.QueryRow(ctx, `SELECT last_seq FROM chatxo.conversations WHERE id=$1`, channel.ID).Scan(&last); err != nil {
			t.Fatal(err)
		}
		if err = f.s.transaction(ctx, func(tx pgx.Tx) error { return f.ch.PruneAIHistory(ctx, tx, channel.ID, last) }); err != nil {
			t.Fatal(err)
		}
	}
	if completed == 0 || storageFailed == 0 {
		t.Fatalf("fixture did not exercise both terminal paths: completed=%d storage_failed=%d", completed, storageFailed)
	}
}

func TestRateLimitRetryRequiresNoPriorOutput(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "no_output"
		if partial {
			name = "partial_output"
		}
		t.Run(name, func(t *testing.T) {
			f := fixture(t, 64<<10)
			ctx := context.Background()
			calls := 0
			f.adapter.generate = func(_ context.Context, _ provider.Request, emit func(provider.Event) error) (provider.Response, error) {
				calls++
				if calls == 1 {
					if partial {
						if err := emit(provider.Event{Type: "text_delta", Text: "partial answer"}); err != nil {
							return provider.Response{}, err
						}
					}
					return provider.Response{}, &provider.Error{Kind: provider.RateLimit, RetryAfter: 10 * time.Millisecond}
				}
				return provider.Response{Text: "retried answer"}, nil
			}
			f.send(t, "rate limited question")
			first := f.claim(t)
			f.s.execute(ctx, first)
			runs, err := f.s.Runs(ctx, f.p, f.channel.ID)
			if err != nil || len(runs) != 1 {
				t.Fatalf("missing turn: %#v %v", runs, err)
			}
			if partial {
				if runs[0].Status != "interrupted" || runs[0].ErrorCode != "remote_outcome_unknown" || runs[0].PartialText != "partial answer" {
					t.Fatalf("partial output was scheduled again: %#v", runs[0])
				}
				if _, err = f.s.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("partial output eligible for automatic retry: %v", err)
				}
				if calls != 1 {
					t.Fatalf("partial output resubmitted %d times", calls)
				}
			} else {
				if runs[0].Status != "waiting" || runs[0].RetryAt == nil {
					t.Fatalf("clean rate limit did not persist wait: %#v", runs[0])
				}
				var visible bool
				if err = f.s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.events WHERE conversation_id=$1 AND convert_from(payload,'UTF8')::jsonb->'data'->>'status'='waiting')`, f.channel.ID).Scan(&visible); err != nil || !visible {
					t.Fatalf("rate limit wait event absent: %v", err)
				}
				var next claimedTurn
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					next, err = f.s.claim(ctx)
					if err == nil {
						break
					}
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatal(err)
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err != nil {
					t.Fatal("clean rate limit never became eligible")
				}
				f.s.execute(ctx, next)
				runs, err = f.s.Runs(ctx, f.p, f.channel.ID)
				if err != nil || runs[0].Status != "completed" || calls != 2 {
					t.Fatalf("clean retry failed: %#v calls=%d err=%v", runs, calls, err)
				}
			}
			f.accounting(t)
		})
	}
}

func TestLongResearchUsesWorkingMemoryAndKeepsQuietTurn(t *testing.T) {
	f := fixture(t, 2<<20)
	ctx := context.Background()
	f.send(t, "research")
	turn := f.claim(t)
	if _, err := f.s.pool.Exec(ctx, `UPDATE chatxo.ai_turns SET updated_at=now()-interval '10 minutes' WHERE id=$1`, turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	f.s.tools = &testTools{}
	const rounds = 15
	count := 0
	f.adapter.generate = func(_ context.Context, r provider.Request, _ func(provider.Event) error) (provider.Response, error) {
		count++
		if count > 1 && (len(r.ToolResults) != 1 || !r.ToolResults[0].IsError) {
			t.Fatalf("step %d lost the previous tool failure", count)
		}
		if count > rounds {
			return provider.Response{Text: "finished"}, nil
		}
		return provider.Response{Calls: []provider.ToolCall{{ID: "call", Name: "read_url", Arguments: json.RawMessage(`{}`)}}, Replay: []json.RawMessage{json.RawMessage(`{"id":"call"}`)}}, nil
	}
	f.s.execute(ctx, turn)
	runs, err := f.s.Runs(ctx, f.p, f.channel.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != "completed" || count != rounds+1 {
		t.Fatalf("long research returned %v (%d requests): %v", runs, count, err)
	}
	f.accounting(t)
}

func TestAgentSwitchCommitsAfterOriginalPersonalityFinishes(t *testing.T) {
	f := fixture(t, 8<<20)
	ctx := context.Background()
	requested, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "Agent choice", Instructions: "Agent instruction"})
	if err != nil {
		t.Fatal(err)
	}
	manual, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "Human choice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.pool.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET allow_agent_switch=true WHERE channel_id=$1`, f.channel.ID); err != nil {
		t.Fatal(err)
	}
	f.send(t, "choose another personality")
	turn := f.claim(t)
	inherited := f.send(t, "already waiting")
	explicit := f.send(t, "<@ai:"+manual.ID+"> human choice")
	after := f.send(t, "after human choice")
	n := 0
	f.adapter.generate = func(_ context.Context, r provider.Request, _ func(provider.Event) error) (provider.Response, error) {
		n++
		if !strings.Contains(r.Instructions, turn.snapshot.Author.Name) {
			t.Fatal("current speaker was changed")
		}
		if n == 1 {
			return provider.Response{Calls: []provider.ToolCall{{ID: "select", Name: "switch_personality", Arguments: json.RawMessage(`{"preset_id":"` + requested.ID + `"}`)}}}, nil
		}
		if len(r.ToolResults) != 1 || r.ToolResults[0].IsError {
			t.Fatal("switch was not staged")
		}
		return provider.Response{Text: "done"}, nil
	}
	f.s.execute(ctx, turn)
	chosen := map[string]string{turn.TriggerMessageID: turn.snapshot.Author.ID, inherited.ID: requested.ID, explicit.ID: manual.ID, after.ID: manual.ID}
	for trigger, want := range chosen {
		var id string
		if err = f.s.pool.QueryRow(ctx, `SELECT snapshot->'author'->>'id' FROM chatxo.ai_turns WHERE trigger_message_id=$1 OR snapshot->'inputs' @> jsonb_build_array(jsonb_build_object('message_id',$1::uuid))`, trigger).Scan(&id); err != nil || id != want {
			t.Fatalf("trigger %s expected %s got %s (%v)", trigger, want, id, err)
		}
	}
	settings, err := f.s.Settings(ctx, f.p, f.channel.ID)
	if err != nil || settings.ActivePresetID != manual.ID {
		t.Fatalf("later explicit selection lost: %+v %v", settings, err)
	}
	f.accounting(t)
}

func TestSameReplyPersonalitySwitch(t *testing.T) {
	f := fixture(t, 8<<20)
	ctx := context.Background()
	target, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "New speaker", Instructions: "Respond in rhymes."})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := f.s.Settings(ctx, f.p, f.channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	immediate := true
	settings.AllowAgentSwitch = true
	settings.SwitchDuringReply = &immediate
	if _, err = f.s.SaveSettings(ctx, f.p, f.channel.ID, settings.SettingsInput); err != nil {
		t.Fatal(err)
	}
	// An older client saving an unrelated setting must retain the selected timing.
	settings.SwitchDuringReply = nil
	saved, err := f.s.SaveSettings(ctx, f.p, f.channel.ID, settings.SettingsInput)
	if err != nil || saved.SwitchDuringReply == nil || !*saved.SwitchDuringReply {
		t.Fatalf("lost switch timing after legacy save: %v", err)
	}
	f.send(t, "Please switch to New speaker and finish the answer.")
	turn := f.claim(t)
	f.send(t, "Queued while the first reply is running")
	calls := 0
	f.adapter.generate = func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Response, error) {
		calls++
		if !strings.Contains(req.StructuralInstruction, "explicitly asks to switch") {
			t.Fatal("missing user-request-only switch guidance")
		}
		advertised := false
		for _, tool := range req.Tools {
			if tool.Name == "switch_personality" && strings.Contains(tool.Description, "explicitly requested") && strings.Contains(tool.Description, "immediately") {
				advertised = true
			}
		}
		if !advertised {
			t.Fatal("missing immediate switch tool guidance")
		}
		if calls == 1 {
			if e := emit(provider.Event{Type: "text_delta", Text: "I will hand this over."}); e != nil {
				return provider.Response{}, e
			}
			return provider.Response{Text: "I will hand this over.", Calls: []provider.ToolCall{{ID: "handoff", Name: "switch_personality", Arguments: json.RawMessage(`{"preset_id":"` + target.ID + `"}`)}}, Replay: []json.RawMessage{json.RawMessage(`{"id":"handoff"}`)}}, nil
		}
		if calls != 2 || !strings.Contains(req.Instructions, target.Instructions) || !strings.Contains(req.StructuralInstruction, "currently writing as New speaker") || len(req.ToolResults) != 1 || req.ToolResults[0].IsError || len(req.Replay) != 1 {
			t.Fatalf("bad handoff instructions or continuation: %v", req.ToolResults)
		}
		current, err := f.s.Settings(ctx, f.p, f.channel.ID)
		if err != nil || current.ActivePresetID != target.ID {
			t.Fatal("switch did not take effect during the turn", err)
		}
		return provider.Response{Text: "Here is my answer."}, nil
	}
	f.s.execute(ctx, turn)
	var bodies, authors []string
	err = f.s.pool.QueryRow(ctx, `SELECT array_agg(body ORDER BY seq),array_agg(ai_author->>'id' ORDER BY seq) FROM chatxo.messages WHERE conversation_id=$1 AND author_id IS NULL`, f.channel.ID).Scan(&bodies, &authors)
	if err != nil || len(bodies) != 2 || bodies[0] != "I will hand this over." || bodies[1] != "Here is my answer." || authors[0] != turn.snapshot.Author.ID || authors[1] != target.ID {
		t.Fatalf("incorrect authored messages: %v %v %v", bodies, authors, err)
	}
	queued := f.claim(t)
	adapter, credentials, err := f.s.credentials(ctx, queued.snapshot.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	req, err := f.s.context(ctx, f.p, queued, adapter, credentials)
	if err != nil || queued.snapshot.Author.ID != target.ID {
		t.Fatal("queued selection:", err)
	}
	previous := []string{}
	for _, msg := range req.Messages {
		if msg.Role == "assistant" {
			previous = append(previous, msg.Parts[0].Text)
		}
	}
	if len(previous) != 2 || !strings.Contains(previous[0], bodies[0]) || !strings.Contains(previous[1], bodies[1]) {
		t.Fatalf("queued response lost handoff history: %v", previous)
	}
	f.accounting(t)
}

func TestActivitySilenceAndRunMemory(t *testing.T) {
	clock := time.Now()
	activity := newRunActivity()
	activity.current.progress = clock.Add(-3 * time.Minute)
	if state, changed := activity.take(clock); !changed || state.action != "waiting_for_provider" {
		t.Fatalf("quiet: %+v", state)
	}
	activity.set("waiting_for_slot", "source", false)
	if state, _ := activity.take(clock.Add(5 * time.Minute)); state.action != "waiting_for_slot" {
		t.Fatalf("queued: %+v", state)
	}
	activity.set("provider", "", true)
	activity.current.progress = clock.Add(5*time.Minute + time.Second)
	if state, _ := activity.take(clock.Add(5*time.Minute + 2*time.Second)); state.action != "provider" {
		t.Fatalf("progress: %+v", state)
	}
	req := provider.Request{ToolResults: []provider.ToolResult{{Name: "read_url", ID: "open", Content: "new result"}}}
	history := make([]json.RawMessage, 129)
	for i := range history {
		history[i] = json.RawMessage(`{"type":"done"}`)
	}
	history[128] = json.RawMessage(`{"pending":"open"}`)
	m := runMemory{notes: []string{"earlier result on the original page"}}
	m.advance(&req, provider.Response{RoundStart: 128, Replay: history})
	if len(req.Replay) != 1 || !bytes.Contains(req.Replay[0], []byte("open")) || !strings.Contains(req.ToolResults[0].Content, "earlier result") || !strings.Contains(req.ToolResults[0].Content, "new result") {
		t.Fatalf("invalid compaction %+v", req)
	}
}

func TestSwitchRequiresEnabledAtRequestAndSuccessfulCompletion(t *testing.T) {
	f := fixture(t, 8<<20)
	ctx := context.Background()
	target, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "Next"})
	if err != nil {
		t.Fatal(err)
	}
	call := provider.ToolCall{ID: "choose", Name: "switch_personality", Arguments: json.RawMessage(`{"preset_id":"` + target.ID + `"}`)}
	for _, scenario := range []string{"disabled", "disabled_after_request", "provider_error", "success"} {
		t.Run(scenario, func(t *testing.T) {
			if _, err = f.s.pool.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET allow_agent_switch=$2 WHERE channel_id=$1`, f.channel.ID, scenario != "disabled"); err != nil {
				t.Fatal(err)
			}
			f.send(t, scenario)
			turn := f.claim(t)
			n := 0
			f.adapter.generate = func(_ context.Context, r provider.Request, _ func(provider.Event) error) (provider.Response, error) {
				n++
				if n == 1 {
					return provider.Response{Calls: []provider.ToolCall{call}}, nil
				}
				if len(r.ToolResults) != 1 || r.ToolResults[0].IsError != (scenario == "disabled") {
					t.Fatalf("unexpected switch result: %+v", r.ToolResults)
				}
				if scenario == "provider_error" {
					return provider.Response{}, errors.New("fixture stream failed")
				}
				if scenario == "disabled_after_request" {
					if _, err := f.s.pool.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET allow_agent_switch=false WHERE channel_id=$1`, f.channel.ID); err != nil {
						t.Fatal(err)
					}
				}
				return provider.Response{Text: "finished as original speaker"}, nil
			}
			f.s.execute(ctx, turn)
			settings, e := f.s.Settings(ctx, f.p, f.channel.ID)
			if e != nil {
				t.Fatal(e)
			}
			if (settings.ActivePresetID == target.ID) != (scenario == "success") {
				t.Fatalf("selected %q", settings.ActivePresetID)
			}
		})
	}
	f.accounting(t)
}

func TestConsultationSharesContextAndKeepsOneReply(t *testing.T) {
	ctx := context.Background()
	f := fixture(t, 8<<20)
	caller, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "Writer", Instructions: "Writer only instruction"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "Reviewer", Instructions: "Reviewer only instruction"})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := f.s.Settings(ctx, f.p, f.channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	settings.AllowConsultations = &enabled
	settings.Instructions = "Shared instruction"
	if _, err = f.s.SaveSettings(ctx, f.p, f.channel.ID, settings.SettingsInput); err != nil {
		t.Fatal(err)
	}
	f.send(t, "<@ai:"+caller.ID+"> Please get a second opinion.")
	turn := f.claim(t)
	call := provider.ToolCall{ID: "consult-1", Name: "consult_personality", Arguments: json.RawMessage(`{"preset_id":"` + target.ID + `","question":"What would you change?"}`)}
	n := 0
	f.adapter.generate = func(_ context.Context, r provider.Request, _ func(provider.Event) error) (provider.Response, error) {
		n++
		if err := provider.Validate(r); err != nil {
			t.Fatal(err)
		}
		switch n {
		case 1:
			return provider.Response{Calls: []provider.ToolCall{call}}, nil
		case 2:
			if len(r.Tools) != 0 || len(r.Replay) != 0 || len(r.ToolResults) != 0 || !strings.Contains(r.Instructions, target.Instructions) || !strings.Contains(r.Instructions, settings.Instructions) || strings.Contains(r.Instructions, caller.Instructions) || len(r.ToolMedia) == 0 || !strings.Contains(r.ToolMedia[0].Text, "Question from Writer") || len(r.Messages) == 0 || r.Messages[len(r.Messages)-1].Author.UserID != f.p.User.ID {
				t.Fatal("invalid consultation context")
			}
			history, err := f.ch.History(ctx, f.p, f.channel.ID, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(history.Messages) != 2 || !history.Messages[1].AIStreaming || len(history.Messages[1].Consultations) != 1 || history.Messages[1].Consultations[0].Status != "waiting" {
				t.Fatal("pending consultation missing from reply", history.Messages)
			}
			return provider.Response{Text: "Check the dates."}, nil
		case 3:
			return provider.Response{Calls: []provider.ToolCall{call}}, nil
		case 4:
			if len(r.ToolResults) != 1 || !strings.Contains(r.ToolResults[0].Content, "Check the dates.") {
				t.Fatal("missing consultant answer")
			}
			return provider.Response{Text: "Here is the answer."}, nil
		default:
			t.Fatal("unbounded consultation")
			return provider.Response{}, nil
		}
	}
	f.s.execute(ctx, turn)
	if n != 4 {
		t.Fatal("wrong number of provider calls", n)
	}
	if err = f.s.DeletePreset(ctx, f.p, f.channel.ID, target.ID); err != nil {
		t.Fatal(err)
	}
	history, err := f.ch.History(ctx, f.p, f.channel.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Messages) != 2 {
		t.Fatal("consultation created extra messages", len(history.Messages))
	}
	reply := history.Messages[1]
	if reply.AIStreaming || reply.AIAuthor.ID != caller.ID || len(reply.Consultations) != 1 || reply.Body != "Here is the answer." {
		t.Fatal("bad final reply", reply)
	}
	c := reply.Consultations[0]
	if c.PresetID != "" || c.Answer != "Check the dates." || c.Status != "answered" || c.CallID != call.ID {
		t.Fatal("bad retained consultation", c)
	}
	f.accounting(t)
}
