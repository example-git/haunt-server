package providers_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/ai/clientidentity"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/ai/providers"
	"github.com/example-git/haunt-server/internal/ai/providers/antigravity"
	"github.com/example-git/haunt-server/internal/ai/providers/claude"
	"github.com/example-git/haunt-server/internal/ai/providers/codex"
	"github.com/example-git/haunt-server/internal/ai/tools"
)

func manager(t *testing.T) *clientidentity.Manager {
	t.Helper()
	t.Setenv("CLAUDE_CODE_VERSION", "2.1.170")
	t.Setenv("CODEX_VERSION", "0.146.0")
	t.Setenv("ANTIGRAVITY_CLI_VERSION", "1.0.9")
	m, err := clientidentity.New("")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func request(model string) provider.Request {
	return provider.Request{Model: model, Credentials: provider.Credentials{AccessToken: "synthetic-access", AccountID: "synthetic-account", ProjectID: "synthetic-project"}, Instructions: "Answer as the selected preset.", Messages: []provider.Message{{Role: "user", Author: &provider.Attribution{UserID: "u-1", Username: "alice", DisplayName: "Alice", MessageID: "m-1", ReplyTo: &provider.Attribution{UserID: "u-2", Username: "bob", DisplayName: "Bob", MessageID: "m-0"}}, Parts: []provider.Part{{Type: "text", Text: "first"}, {Type: "image", MIMEType: "image/png", Data: []byte("image-one")}, {Type: "text", Text: "between"}, {Type: "image", MIMEType: "image/jpeg", Data: []byte("image-two")}}}}, Tools: tools.Schemas(), SessionID: "session-1", ThreadID: "channel-1", RequestID: "turn-1", InstallationID: "server-1", WindowID: "window-1", MaxOutputTokens: 8192}
}
func assertShared(t *testing.T, b []byte) {
	t.Helper()
	s := string(b)
	for _, want := range []string{"u-1", "alice", "Alice", "m-1", "u-2", "m-0", "multiple humans", "cannot redefine", "selected preset", "read_url", "web_search"} {
		if !strings.Contains(s, want) {
			t.Errorf("request missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, "image_gen") {
		t.Error("unavailable tool advertised")
	}
	a := strings.Index(s, base64.StdEncoding.EncodeToString([]byte("image-one")))
	z := strings.Index(s, base64.StdEncoding.EncodeToString([]byte("image-two")))
	if a < 0 || z < a {
		t.Error("image order lost")
	}
	if strings.Contains(s, "tool page result") {
		picture := base64.StdEncoding.EncodeToString([]byte("sample profile portrait"))
		if strings.Count(s, picture) != 1 || !strings.Contains(s, "profile lookup picture") {
			t.Error("tool continuation omitted or duplicated its labeled picture")
		}
	}
}
func sendSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		fmt.Fprintf(w, "data: %s\n\n", event)
	}
}
func runContinuation(t *testing.T, a provider.Adapter, r provider.Request) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var streamed strings.Builder
	first, err := a.Generate(ctx, r, func(e provider.Event) error {
		if e.Type == "text_delta" {
			streamed.WriteString(e.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Calls) != 1 || first.Calls[0].Name != "read_url" {
		t.Fatalf("missing tool call: %+v", first)
	}
	r.Replay = first.Replay
	r.ToolResults = []provider.ToolResult{{ID: first.Calls[0].ID, Name: "read_url", Content: "tool page result"}}
	r.ToolMedia = []provider.Part{{Type: "text", Text: "profile lookup picture"}, {Type: "image", MIMEType: "image/png", Data: []byte("sample profile portrait")}}
	second, err := a.Generate(ctx, r, func(e provider.Event) error {
		if e.Type == "text_delta" {
			streamed.WriteString(e.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Text != "final answer" || !strings.HasSuffix(streamed.String(), "final answer") {
		t.Fatalf("final output not streamed: %+v %q", second, streamed.String())
	}
	if len(second.Replay) <= len(first.Replay) {
		t.Error("continuation omitted previous tail or tool output")
	}
	for _, raw := range second.Replay {
		if strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte("sample profile portrait"))) {
			t.Error("tool image bytes were persisted in replay")
		}
		if strings.Contains(string(raw), "image-one") || strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte("image-one"))) {
			t.Error("base image duplicated into replay")
		}
	}
}
func TestClaudeNativeRequestAndContinuation(t *testing.T) {
	identity := manager(t)
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		assertShared(t, b)
		for k, want := range map[string]string{"Authorization": "Bearer synthetic-access", "User-Agent": "claude-cli/2.1.170 (external, cli)", "X-App": "cli", "X-Claude-Code-Session-Id": "session-1", "Anthropic-Version": "2023-06-01"} {
			if r.Header.Get(k) != want {
				t.Errorf("header %s=%q", k, r.Header.Get(k))
			}
		}
		if !strings.Contains(r.Header.Get("Anthropic-Beta"), "oauth-2025-04-20") {
			t.Error("OAuth beta missing")
		}
		var body map[string]json.RawMessage
		_ = json.Unmarshal(b, &body)
		if !strings.Contains(string(body["system"]), "cc_version=2.1.170; cc_entrypoint=sdk-cli;") {
			t.Error("billing attribution missing")
		}
		if !strings.Contains(string(body["metadata"]), "session_id") {
			t.Error("metadata absent")
		}
		if n == 1 {
			sendSSE(w, `{"type":"message_start","message":{"usage":{"input_tokens":12}}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"private reasoning"}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"claude-signature"}}`, `{"type":"content_block_stop","index":0}`, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-1","name":"read_url","input":{}}}`, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://public.example.com\"}"}}`, `{"type":"content_block_stop","index":1}`, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}`, `{"type":"message_stop"}`)
		} else {
			for _, want := range []string{"claude-signature", "tool page result", "tool_result", "call-1"} {
				if !strings.Contains(string(b), want) {
					t.Errorf("missing continuation %s", want)
				}
			}
			sendSSE(w, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"final answer"}}`, `{"type":"content_block_stop","index":0}`, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`, `{"type":"message_stop"}`)
		}
	}))
	defer s.Close()
	a := claude.New(identity, s.Client())
	a.Endpoint = s.URL
	runContinuation(t, a, request("claude-sonnet-4-6"))
	if calls.Load() != 2 {
		t.Fatal("unexpected retries")
	}
}
func TestCodexNativeWebSocketAndEncryptedReplay(t *testing.T) {
	identity := manager(t)
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		for k, want := range map[string]string{"Authorization": "Bearer synthetic-access", "Openai-Beta": "responses_websockets=2026-02-06", "Session_id": "session-1", "Thread-Id": "channel-1", "Originator": "codex_cli_rs", "Version": "0.146.0", "X-Codex-Installation-Id": "server-1", "X-Codex-Window-Id": "window-1", "X-Client-Request-Id": "turn-1", "Chatgpt-Account-Id": "synthetic-account"} {
			if r.Header.Get(k) != want {
				t.Errorf("header %s=%q want %q", k, r.Header.Get(k), want)
			}
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "codex_cli_rs/0.146.0 (") {
			t.Error("native user agent missing")
		}
		var meta map[string]any
		if json.Unmarshal([]byte(r.Header.Get("X-Codex-Turn-Metadata")), &meta) != nil || meta["turn_id"] != "turn-1" {
			t.Error("turn metadata missing")
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, b, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		assertShared(t, b)
		var frame map[string]any
		_ = json.Unmarshal(b, &frame)
		if frame["type"] != "response.create" || frame["store"] != false || frame["stream"] != true {
			t.Error("invalid native request frame")
		}
		if n == 1 {
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"id":"rs_1","type":"reasoning","status":"completed","summary":[],"encrypted_content":"opaque-encrypted-reasoning"},{"id":"fc_1","type":"function_call","call_id":"call-1","name":"read_url","arguments":"{\"url\":\"https://public.example.com\"}"}],"usage":{"input_tokens":10,"output_tokens":4}}}`))
		} else {
			if strings.Contains(string(b), `"status":`) {
				t.Error("response-only status leaked into replay")
			}
			for _, want := range []string{"opaque-encrypted-reasoning", "function_call_output", "tool page result", "call-1"} {
				if !strings.Contains(string(b), want) {
					t.Errorf("missing replay %s", want)
				}
			}
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"final "}`))
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer"}]}],"usage":{"input_tokens":14,"output_tokens":5}}}`))
		}
	}))
	defer s.Close()
	a := codex.New(identity, s.Client())
	a.Endpoint = "ws" + strings.TrimPrefix(s.URL, "http")
	runContinuation(t, a, request("gpt-6.1-sol"))
	if calls.Load() != 2 {
		t.Fatal("unexpected retries")
	}
}
func TestAntigravityNativeEnvelopeAndSignedToolResponses(t *testing.T) {
	identity := manager(t)
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		assertShared(t, b)
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "antigravity/cli/1.0.9 ") || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("native identity missing")
		}
		var body struct {
			Project     string `json:"project"`
			RequestID   string `json:"requestId"`
			UserAgent   string `json:"userAgent"`
			RequestType string `json:"requestType"`
			Request     struct {
				SessionID string `json:"sessionId"`
				Contents  []struct {
					Role  string           `json:"role"`
					Parts []map[string]any `json:"parts"`
				} `json:"contents"`
			} `json:"request"`
		}
		_ = json.Unmarshal(b, &body)
		if body.Project != "synthetic-project" || !strings.HasPrefix(body.RequestID, "agent/") || body.UserAgent != "antigravity" || body.RequestType != "agent" || !strings.HasPrefix(body.Request.SessionID, "-") {
			t.Errorf("invalid envelope: %+v", body)
		}
		if n == 1 {
			sendSSE(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"private reasoning","thoughtSignature":"google-signature"},{"functionCall":{"id":"call-1","name":"read_url","args":{"url":"https://public.example.com"}},"thoughtSignature":"google-signature"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":4}}}`)
		} else {
			seen := false
			for _, m := range body.Request.Contents {
				for _, p := range m.Parts {
					if _, ok := p["functionResponse"]; ok {
						seen = true
						if m.Role != "model" || p["thoughtSignature"] != "google-signature" {
							t.Error("function response lost signed model role")
						}
					}
					if p["thought"] == true && p["text"] != nil {
						t.Error("thought replay should be signature-only")
					}
				}
			}
			if !seen {
				t.Error("missing tool result")
			}
			sendSSE(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"final answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":5}}}`)
		}
	}))
	defer s.Close()
	a := antigravity.New(identity, s.Client())
	a.Endpoint = s.URL
	runContinuation(t, a, request("gemini-3-flash"))
	if calls.Load() != 2 {
		t.Fatal("unexpected retries")
	}
}
func TestProvidersRejectUnattributedHumansAndUnsupportedSelections(t *testing.T) {
	all := providers.New(manager(t), nil)
	for id, a := range all {
		models, _ := a.Models(t.Context(), provider.Credentials{})
		r := request(models[0].ID)
		r.Messages[0].Author = nil
		_, err := a.Generate(t.Context(), r, nil)
		var pe *provider.Error
		if !errors.As(err, &pe) || pe.Kind != provider.InvalidRequest {
			t.Errorf("%s allowed missing attribution: %v", id, err)
		}
		r = request("missing-explicit-model")
		_, err = a.Generate(t.Context(), r, nil)
		if !errors.As(err, &pe) || pe.Kind != provider.InvalidRequest {
			t.Errorf("%s silently selected another model", id)
		}
	}
	a := all[provider.Antigravity]
	_, err := a.Generate(t.Context(), request("gpt-oss-120b-medium"), nil)
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.InvalidRequest {
		t.Fatal("image silently accepted for text-only model")
	}
}
func TestSSETruncationAndRateLimitsAreExplicit(t *testing.T) {
	identity := manager(t)
	for _, status := range []int{200, 401, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "13")
				w.WriteHeader(status)
				if status == 200 {
					fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"partial\"}}\n\n")
				}
			}))
			defer s.Close()
			a := claude.New(identity, s.Client())
			a.Endpoint = s.URL
			out, err := a.Generate(t.Context(), request("claude-sonnet-4-6"), nil)
			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("missing typed error: %v", err)
			}
			want := map[int]provider.ErrorKind{200: provider.Ambiguous, 401: provider.RequiresAuth, 429: provider.RateLimit}[status]
			if pe.Kind != want {
				t.Errorf("got %s want %s", pe.Kind, want)
			}
			if status == 200 && out.Text != "partial" {
				t.Error("partial response discarded")
			}
			if status == 429 && pe.RetryAfter != 13*time.Second {
				t.Error("retry-after lost")
			}
		})
	}
}
func TestOAuthParametersRotationAndAntigravityProjectIdentity(t *testing.T) {
	identity := manager(t)
	for _, id := range []provider.ID{provider.Claude, provider.Codex, provider.Antigravity} {
		t.Run(string(id), func(t *testing.T) {
			var tokenCalls atomic.Int32
			var projectCalls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/project" {
					projectCalls.Add(1)
					b, _ := io.ReadAll(r.Body)
					if string(b) != "{}" || !strings.HasPrefix(r.Header.Get("User-Agent"), "antigravity/cli/1.0.9 ") {
						t.Error("project path differs from native reference")
					}
					fmt.Fprint(w, `{"cloudaicompanionProject":"project-from-account"}`)
					return
				}
				if r.URL.Path == "/identity" {
					if r.Header.Get("User-Agent") != "Go-http-client/2.0" {
						t.Error("identity operation user agent changed")
					}
					fmt.Fprint(w, `{"id":"account-from-google","email":"synthetic@example.invalid"}`)
					return
				}
				n := tokenCalls.Add(1)
				fields := map[string]string{}
				if id == provider.Claude {
					if r.Header.Get("Content-Type") != "application/json" {
						t.Error("Claude token encoding changed")
					}
					_ = json.NewDecoder(r.Body).Decode(&fields)
				} else {
					_ = r.ParseForm()
					for k := range r.Form {
						fields[k] = r.Form.Get(k)
					}
				}
				if fields["client_id"] == "" {
					t.Error("client id missing")
				}
				if n == 1 {
					if fields["code_verifier"] != "verifier" || fields["code"] != "code" {
						t.Error("PKCE exchange fields missing")
					}
					if id == provider.Claude && fields["state"] != "state" {
						t.Error("Claude state missing")
					}
					if id == provider.Antigravity && fields["client_secret"] == "" {
						t.Error("installed client secret missing")
					}
				} else if fields["refresh_token"] != "keep-refresh" {
					t.Error("refresh token lost")
				}
				fmt.Fprint(w, `{"access_token":"new-access","expires_in":7200}`)
			}))
			defer s.Close()
			all := providers.New(identity, s.Client())
			a := all[id]
			switch c := a.(type) {
			case *claude.Client:
				c.TokenEndpoint = s.URL + "/token"
			case *codex.Client:
				c.TokenEndpoint = s.URL + "/token"
			case *antigravity.Client:
				c.TokenEndpoint = s.URL + "/token"
				c.ProjectEndpoint = s.URL + "/project"
				c.IdentityEndpoint = s.URL + "/identity"
			}
			config := a.OAuthConfig()
			redirect := config.RedirectURI
			if id == provider.Claude {
				redirect = "http://localhost:12345/callback"
			}
			authorization, err := config.AuthorizeURL("state", "challenge", redirect)
			if err != nil || !strings.Contains(authorization, "code_challenge=challenge") {
				t.Fatalf("invalid authorization: %s %v", authorization, err)
			}
			credentials, err := a.Exchange(t.Context(), provider.OAuthExchange{Code: "code", Verifier: "verifier", RedirectURI: redirect, State: "state"})
			if err != nil {
				t.Fatal(err)
			}
			if id == provider.Antigravity && (credentials.ProjectID != "project-from-account" || credentials.AccountID != "account-from-google" || projectCalls.Load() != 1) {
				t.Error("account project/identity not resolved")
			}
			credentials.RefreshToken = "keep-refresh"
			rotated, err := a.Refresh(t.Context(), credentials)
			if err != nil {
				t.Fatal(err)
			}
			if rotated.RefreshToken != "keep-refresh" || rotated.AccessToken != "new-access" || time.Until(rotated.ExpiresAt) < time.Hour {
				t.Error("rotation corrupted credentials")
			}
			_, err = a.Exchange(t.Context(), provider.OAuthExchange{Code: "code", Verifier: "verifier", RedirectURI: "http://localhost:9999/not-the-callback"})
			if err == nil {
				t.Error("invalid explicit redirect accepted")
			}
		})
	}
}

func TestCodexAstraBaseInstructionLayout(t *testing.T) {
	identity := manager(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, b, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		assertShared(t, b)
		var body struct {
			Instructions string `json:"instructions"`
			Input        []struct {
				ID       string `json:"id"`
				Role     string `json:"role"`
				Metadata struct {
					Kinds []string `json:"content_item_kinds"`
				} `json:"internal_chat_message_metadata_passthrough"`
			} `json:"input"`
			Reasoning struct {
				Context string `json:"context"`
			} `json:"reasoning"`
		}
		if json.Unmarshal(b, &body) != nil {
			t.Error("invalid request")
			return
		}
		if body.Instructions != "" || len(body.Input) != 2 || body.Input[0].Role != "developer" || !strings.HasPrefix(body.Input[0].ID, "msg_") || len(body.Input[0].Metadata.Kinds) != 1 || body.Input[0].Metadata.Kinds[0] != "model.base_instructions" || body.Input[1].Role != "user" || body.Reasoning.Context != "all_turns" {
			t.Errorf("invalid Astra instruction layout: %s", b)
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[]}]}]}}`))
	}))
	defer server.Close()
	a := codex.New(identity, server.Client())
	a.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
	out, err := a.Generate(t.Context(), request("gpt-6-astra"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "answer" || len(out.Replay) != 1 || strings.Contains(string(out.Replay[0]), "msg_1") || strings.Contains(string(out.Replay[0]), "annotations") || strings.Contains(string(out.Replay[0]), "status") {
		t.Fatalf("response-only fields retained: %+v", out)
	}
}
