package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/ai"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
)

func TestAIChannelAcceptancePermissionsAndOrdinaryChat(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	member := f.register(t, "aichannelmember")
	other := f.register(t, "aichannelother")
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "assistant", "kind": "ai"}, 201, &channel)
	f.call(t, "PUT", "/api/v1/channels/"+channel.ID, f.owner.Token, map[string]any{"name": "assistant", "kind": "text"}, 404, nil)
	var roles rolesResponse
	f.call(t, "GET", "/api/v1/roles", f.owner.Token, nil, 200, &roles)
	var everyone string
	for _, role := range roles.Roles {
		if role.Default {
			everyone = role.ID
		}
	}
	if everyone == "" {
		t.Fatal("default role missing")
	}
	base := "/api/v1/channels/" + channel.ID
	f.call(t, "PUT", base+"/permissions/"+everyone, f.owner.Token, map[string]any{"allow": 0, "deny": identity.UseAI}, 204, nil)
	sendMessage(t, f, member.Token, channel.ID, "visible without invoking")
	var runs aiTurnsResponse
	f.call(t, "GET", base+"/ai/turns", member.Token, nil, 200, &runs)
	if len(runs.Turns) != 0 {
		t.Fatal("UseAI denied but turn queued")
	}
	f.call(t, "PUT", base+"/permissions/"+everyone, f.owner.Token, map[string]any{"allow": 0, "deny": 0}, 204, nil)
	message := sendMessage(t, f, member.Token, channel.ID, "durably accepted")
	f.call(t, "GET", base+"/ai/turns", member.Token, nil, 200, &runs)
	if len(runs.Turns) != 1 || runs.Turns[0].TriggerMessageID != message.ID || runs.Turns[0].Status != "requires_auth" {
		t.Fatalf("missing durable configuration status: %#v", runs.Turns)
	}
	var preset ai.Preset
	f.call(t, "POST", base+"/ai/presets", f.owner.Token, ai.PresetInput{Name: "Helper", Instructions: "Be concise."}, 200, &preset)
	f.call(t, "PUT", base+"/permissions/"+everyone, f.owner.Token, map[string]any{"allow": 0, "deny": identity.SwitchAIPreset}, 204, nil)
	f.call(t, "POST", "/api/v1/conversations/"+channel.ID+"/messages", member.Token, map[string]any{"body": "<@ai:" + preset.ID + "> switch", "request_id": identity.NewID()}, 403, nil)
	var settings ai.Settings
	f.call(t, "GET", base+"/ai", member.Token, nil, 200, &settings)
	if settings.ActivePresetID != "" {
		t.Fatal("denied preset switch committed")
	}
	var history chat.History
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", member.Token, nil, 200, &history)
	if len(history.Messages) != 2 {
		t.Fatal("failed preset message was committed")
	}
	var dm chat.Conversation
	f.call(t, "POST", "/api/v1/dms", member.Token, map[string]any{"members": []string{other.User.ID}}, 201, &dm)
	sendMessage(t, f, member.Token, dm.ID, "ordinary DM still works")
	text := textChannel(t, f)
	sendMessage(t, f, member.Token, text.ID, "ordinary text still works")
}

func TestAIMessageAuthorshipRepliesAndInternalWriter(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	member := f.register(t, "aimessageuser")
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "assistant", "kind": "ai"}, 201, &channel)
	sendMessage(t, f, member.Token, channel.ID, "hello")
	var preset ai.Preset
	f.call(t, "POST", "/api/v1/channels/"+channel.ID+"/ai/presets", f.owner.Token, ai.PresetInput{Name: strings.Repeat("🧠", 80)}, 200, &preset)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = features.Chat.LockStorage(ctx, tx); err != nil {
		t.Fatal(err)
	}
	author := chat.AIAuthor{ID: preset.ID, Name: preset.Name}
	message, err := features.Chat.WriteAI(ctx, tx, channel.ID, identity.NewID(), "Hello, human.", author)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var history chat.History
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", member.Token, nil, 200, &history)
	if len(history.Messages) != 2 || history.Messages[1].AuthorID != "" || history.Messages[1].AIAuthor == nil || history.Messages[1].AIAuthor.Name != author.Name {
		t.Fatalf("AI authorship missing from history: %#v", history.Messages)
	}
	var reply chat.Message
	f.call(t, "POST", "/api/v1/conversations/"+channel.ID+"/messages", member.Token, map[string]any{"body": "reply to assistant", "request_id": identity.NewID(), "reply_to_id": message.ID}, 201, &reply)
	if len(reply.MentionUserIDs) != 0 {
		t.Fatal("AI reply fabricated a human mention")
	}
	f.call(t, "PUT", "/api/v1/conversations/"+channel.ID+"/messages/"+message.ID, member.Token, map[string]any{"body": "impersonation"}, 403, nil)
	f.call(t, "POST", "/api/v1/conversations/"+channel.ID+"/messages", member.Token, map[string]any{"body": "impersonation", "request_id": identity.NewID(), "ai_author": author}, 400, nil)
	f.call(t, "DELETE", "/api/v1/conversations/"+channel.ID+"/messages/"+message.ID, f.owner.Token, nil, 200, nil)
	accounting(t, f)
	f.call(t, "DELETE", "/api/v1/channels/"+channel.ID, f.owner.Token, nil, 204, nil)
	accounting(t, f)
}

func TestAIOAuthValidationAndRoleBoundaries(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	member := f.register(t, "aioauthmember")
	f.call(t, "POST", "/api/v1/ai/oauth/start", member.Token, ai.OAuthPrepareInput{Provider: "codex", RedirectURI: "http://localhost:1455/auth/callback"}, 403, nil)
	f.call(t, "POST", "/api/v1/ai/oauth/start", f.owner.Token, ai.OAuthPrepareInput{Provider: "codex", RedirectURI: "http://localhost:1456/auth/callback"}, 400, nil)
	var pending ai.OAuthSession
	f.call(t, "POST", "/api/v1/ai/oauth/start", f.owner.Token, ai.OAuthPrepareInput{Provider: "codex", RedirectURI: "http://localhost:1455/auth/callback"}, 200, &pending)
	if pending.State == "" || pending.AuthorizationURL == "" {
		t.Fatal("OAuth PKCE setup missing")
	}
	f.call(t, "POST", "/api/v1/ai/oauth/"+pending.ID+"/complete", f.owner.Token, ai.OAuthCompleteInput{Code: "fixture-code", State: "wrong-state"}, 400, nil)
	f.call(t, "POST", "/api/v1/ai/oauth/"+pending.ID+"/complete", member.Token, ai.OAuthCompleteInput{Code: "fixture-code", State: pending.State}, 403, nil)
}

func TestAIChannelDeletionLocksAccountingBeforeCascade(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "assistant", "kind": "ai"}, 201, &channel)
	f.call(t, "POST", "/api/v1/channels/"+channel.ID+"/ai/presets", f.owner.Token, ai.PresetInput{Name: "Guide"}, 200, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, err := f.identities.Authenticate(ctx, f.owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var backend int
	if err = tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM chatxo.storage WHERE singleton FOR UPDATE`).Scan(&backend); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.identities.DeleteResource(ctx, p, "channel", channel.ID) }()
	for {
		var waiting bool
		if err = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, backend).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err = <-done:
			t.Fatalf("channel deletion did not wait for accounting lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	lockCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	_, err = tx.Exec(lockCtx, `SELECT id FROM chatxo.channels WHERE id=$1 FOR UPDATE`, channel.ID)
	stop()
	if err != nil {
		t.Fatalf("channel deletion acquired channel before accounting: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	accounting(t, f)
}
