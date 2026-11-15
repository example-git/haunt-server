package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/ai"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/ai/providers/antigravity"
	"github.com/example-git/haunt-server/internal/ai/providers/claude"
	"github.com/example-git/haunt-server/internal/ai/providers/codex"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/httpapi"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
)

func aiFixture(t *testing.T, ctx context.Context, cfg config.Config, pool postgres.Pool, ids *identity.Service, features *httpapi.Features, owner identity.Principal) (string, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "isolated-fixture-access", "refresh_token": "isolated-fixture-refresh", "expires_in": 3600, "token_type": "Bearer"})
			return
		}
		if r.URL.Path == "/project" || r.URL.Path == "/userinfo" {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/project" {
				json.NewEncoder(w).Encode(map[string]string{"cloudaicompanionProject": "isolated-fixture-project"})
			} else {
				json.NewEncoder(w).Encode(map[string]string{"id": "isolated-fixture-user", "email": "fixture@example.invalid"})
			}
			return
		}
		if r.URL.Path != "/messages" || r.Header.Get("Authorization") != "Bearer isolated-fixture-access" {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type, Text, Content string
					IsError             bool `json:"is_error"`
				} `json:"content"`
			} `json:"messages"`
			System []struct{ Type, Text string } `json:"system"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		users, images, persona := map[string]bool{}, 0, ""
		latestInput, toolOK, hasToolResult := "", false, false
		for _, message := range request.Messages {
			if message.Role == "user" {
				latestInput = ""
			}
			for _, part := range message.Content {
				if message.Role == "user" && part.Type == "text" {
					latestInput += part.Text
				}
				if part.Type == "tool_result" {
					hasToolResult = true
					var page struct {
						URL, Content string
						TotalChars   int `json:"total_chars"`
					}
					toolOK = !part.IsError && json.Unmarshal([]byte(part.Content), &page) == nil && page.URL == "https://example.org/" && len(strings.TrimSpace(page.Content)) > 0 && page.TotalChars > 0
				}
				if part.Type == "image" {
					images++
				}
				if raw, found := strings.CutPrefix(part.Text, "<haunt_author>"); found {
					raw, _, _ = strings.Cut(raw, "</haunt_author>")
					var author struct {
						Username string `json:"username"`
					}
					if json.Unmarshal([]byte(raw), &author) == nil && author.Username != "" {
						users[author.Username] = true
					}
				}
			}
		}
		for _, part := range request.System {
			if _, rest, found := strings.Cut(part.Text, "Selected assistant identity: "); found {
				persona, _, _ = strings.Cut(rest, " (")
			}
		}
		names := make([]string, 0, len(users))
		for name := range users {
			names = append(names, name)
		}
		sort.Strings(names)
		reply := fmt.Sprintf("AI fixture reply; users=%s; images=%d; persona=%s", strings.Join(names, ","), images, persona)
		if hasToolResult {
			reply += fmt.Sprintf("; tools=read_url; tool_ok=%t", toolOK)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []map[string]any{
			{"type": "message_start", "message": map[string]any{"id": "fixture", "type": "message", "role": "assistant", "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}},
			{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}},
			{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": reply}},
			{"type": "content_block_stop", "index": 0},
			{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 16}},
			{"type": "message_stop"},
		}
		if !hasToolResult && strings.Contains(latestInput, "Read https://example.org/ using the URL tool") {
			events = []map[string]any{
				{"type": "message_start", "message": map[string]any{"usage": map[string]int{"input_tokens": 1}}},
				{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "fixture_url", "name": "read_url", "input": map[string]any{}}},
				{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": `{"url":"https://example.org/"}`}},
				{"type": "content_block_stop", "index": 0},
				{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}},
				{"type": "message_stop"},
			}
		}
		for _, event := range events {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(upstream.Close)
	adapter := claude.New(features.ClientIdentities, upstream.Client())
	adapter.Endpoint, adapter.TokenEndpoint = upstream.URL+"/messages", upstream.URL+"/token"
	codexAdapter := codex.New(features.ClientIdentities, upstream.Client())
	codexAdapter.TokenEndpoint = upstream.URL + "/token"
	codexAdapter.Endpoint = strings.Replace(upstream.URL, "http://", "ws://", 1) + "/codex"
	antigravityAdapter := antigravity.New(features.ClientIdentities, upstream.Client())
	antigravityAdapter.Endpoint = upstream.URL + "/gemini"
	antigravityAdapter.TokenEndpoint = upstream.URL + "/token"
	antigravityAdapter.ProjectEndpoint = upstream.URL + "/project"
	antigravityAdapter.IdentityEndpoint = upstream.URL + "/userinfo"
	features.AI.Close()
	service, err := ai.New(ctx, pool, ids, features.Chat, features.Attachments, ai.Options{InstanceID: cfg.InstanceID, DataDir: cfg.DataDir}, map[provider.ID]provider.Adapter{provider.Claude: adapter, provider.Codex: codexAdapter, provider.Antigravity: antigravityAdapter})
	if err != nil {
		t.Fatal(err)
	}
	features.AI, features.Chat.AI = service, service
	session, err := service.PrepareOAuth(ctx, owner, cfg.PublicURL, ai.OAuthPrepareInput{Provider: "claude", RedirectURI: "http://localhost:18455/callback"})
	if err != nil {
		t.Fatal(err)
	}
	account, err := service.CompleteOAuth(ctx, owner, cfg.PublicURL, session.ID, ai.OAuthCompleteInput{Code: "isolated-fixture-code", State: session.State})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := ids.SaveChannel(ctx, owner, "", "ai-room", "ai", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.SaveSettings(ctx, owner, channel.ID, ai.SettingsInput{AccountID: account.ID, Provider: "claude", Model: "claude-sonnet-4-6", Name: "Guide", Instructions: "Answer the shared conversation.", MaxBytes: ai.DefaultChannelMaxBytes})
	if err != nil {
		t.Fatal(err)
	}
	preset, err := service.SavePreset(ctx, owner, channel.ID, "", ai.PresetInput{Name: "Sage", Instructions: "Be concise."})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return channel.ID, preset.ID
}
