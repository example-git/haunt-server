package codex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/ai/clientidentity"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/ai/providers/wire"
)

type Client struct {
	Identity      *clientidentity.Manager
	HTTP          *http.Client
	Endpoint      string
	TokenEndpoint string
}

func New(identity *clientidentity.Manager, httpClient *http.Client) *Client {
	return &Client{Identity: identity, HTTP: wire.Client(httpClient), Endpoint: "wss://chatgpt.com/backend-api/codex/responses", TokenEndpoint: "https://auth.openai.com/oauth/token"}
}
func (c *Client) ID() provider.ID { return provider.Codex }
func (c *Client) Models(ctx context.Context, _ provider.Credentials) ([]provider.Model, error) {
	return append([]provider.Model(nil), catalog...), ctx.Err()
}
func (c *Client) OAuthConfig() provider.OAuthConfig {
	return provider.OAuthConfig{AuthorizationURL: "https://auth.openai.com/oauth/authorize", TokenURL: c.TokenEndpoint, ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Scopes: []string{"openid", "profile", "email", "offline_access", "api.connectors.read", "api.connectors.invoke"}, RedirectMode: "loopback-fixed", RedirectURI: "http://localhost:1455/auth/callback", ExtraParams: map[string]string{"id_token_add_organizations": "true", "codex_cli_simplified_flow": "true"}}
}
func (c *Client) Exchange(ctx context.Context, e provider.OAuthExchange) (provider.Credentials, error) {
	config := c.OAuthConfig()
	if err := wire.ValidateExchange(config, e); err != nil {
		return provider.Credentials{}, err
	}
	return wire.Token(ctx, c.HTTP, c.TokenEndpoint, wire.ExchangeFields(config, e), false, provider.Credentials{})
}
func (c *Client) Refresh(ctx context.Context, old provider.Credentials) (provider.Credentials, error) {
	fields, err := wire.RefreshFields(c.OAuthConfig(), old)
	if err != nil {
		return old, err
	}
	return wire.Token(ctx, c.HTTP, c.TokenEndpoint, fields, false, old)
}
func (c *Client) Generate(ctx context.Context, r provider.Request, emit func(provider.Event) error) (out provider.Response, err error) {
	if err = wire.ValidateModel(r, catalog); err != nil {
		return
	}
	identity, err := c.Identity.Snapshot(provider.Codex)
	if err != nil {
		return out, err
	}
	encodeMessage := func(m provider.Message) json.RawMessage {
		parts := []any{}
		textType := "input_text"
		if m.Role == "assistant" {
			textType = "output_text"
		}
		if m.Author != nil {
			parts = append(parts, map[string]any{"type": textType, "text": provider.AuthorText(m.Author)})
		}
		for _, p := range m.Parts {
			if p.Type == "text" {
				parts = append(parts, map[string]any{"type": textType, "text": p.Text})
			} else {
				parts = append(parts, map[string]any{"type": "input_image", "image_url": "data:" + p.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(p.Data)})
			}
		}
		return wire.JSON(map[string]any{"type": "message", "role": m.Role, "content": parts})
	}
	input := make([]json.RawMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		input = append(input, encodeMessage(m))
	}
	out.Replay = append([]json.RawMessage(nil), r.Replay...)
	for _, v := range r.ToolResults {
		out.Replay, err = wire.AppendReplay(out.Replay, map[string]any{"type": "function_call_output", "call_id": v.ID, "output": v.Content})
		if err != nil {
			return
		}
	}
	out.RoundStart = len(out.Replay)
	input = append(input, out.Replay...)
	if len(r.ToolMedia) > 0 {
		input = append(input, encodeMessage(provider.Message{Role: "user", Parts: r.ToolMedia}))
	}
	tools := []any{}
	for _, t := range r.Tools {
		tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Parameters, "strict": false})
	}
	cache := sha256.Sum256([]byte(r.Credentials.AccountID + "\x00" + r.ThreadID))
	body := map[string]any{"type": "response.create", "model": r.Model, "instructions": provider.System(r), "input": input, "stream": true, "store": false, "parallel_tool_calls": false, "include": []string{"reasoning.encrypted_content"}, "reasoning": map[string]any{"effort": "medium", "summary": "auto"}, "prompt_cache_key": hex.EncodeToString(cache[:]), "client_metadata": map[string]string{"session_id": r.SessionID, "turn_id": r.RequestID}}
	body["text"] = map[string]any{"format": map[string]any{"type": "text"}}
	if r.Model == "gpt-6-astra" {
		instructions := provider.System(r)
		digest := sha256.Sum256([]byte(instructions))
		base := wire.JSON(map[string]any{"type": "message", "id": "msg_" + hex.EncodeToString(digest[:16]), "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": instructions}}, "internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []string{"model.base_instructions"}}})
		body["input"] = append([]json.RawMessage{base}, input...)
		delete(body, "instructions")
		body["reasoning"] = map[string]any{"effort": "medium", "summary": "auto", "context": "all_turns"}
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	data, err := json.Marshal(body)
	if err != nil {
		return out, err
	}
	if len(data) > 32<<20 {
		return out, &provider.Error{Kind: provider.InvalidRequest, Message: "provider request exceeds byte limit"}
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+r.Credentials.AccessToken)
	headers.Set("User-Agent", identity.UserAgent)
	headers.Set("Origin", "https://chatgpt.com")
	headers.Set("openai-beta", "responses_websockets=2026-02-06")
	headers.Set("x-codex-beta-features", "remote_compaction_v2")
	headers.Set("session_id", r.SessionID)
	headers.Set("thread-id", r.ThreadID)
	headers.Set("originator", identity.Originator)
	headers.Set("version", identity.Version)
	headers.Set("x-codex-installation-id", r.InstallationID)
	window := r.WindowID
	if window == "" {
		window = r.SessionID + ":0"
	}
	headers.Set("x-codex-window-id", window)
	headers.Set("x-client-request-id", r.RequestID)
	headers.Set("x-codex-turn-metadata", string(wire.JSON(map[string]any{"installation_id": r.InstallationID, "session_id": r.SessionID, "thread_id": r.ThreadID, "turn_id": r.RequestID, "window_id": window, "workspaces": map[string]any{}, "sandbox": "none"})))
	if r.Credentials.AccountID != "" {
		headers.Set("chatgpt-account-id", r.Credentials.AccountID)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	client := wire.Client(c.HTTP)
	client.Timeout = 0
	conn, resp, err := websocket.Dial(dialCtx, c.Endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if resp != nil {
			return out, wire.Status(resp.StatusCode, resp.Header)
		}
		return out, &provider.Error{Kind: provider.Unavailable, Message: "Codex WebSocket connection failed before submission"}
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	if err = conn.Write(ctx, websocket.MessageText, data); err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, wire.Truncated()
	}
	if err = wire.Emit(emit, provider.Event{Type: "submitted"}); err != nil {
		return out, err
	}
	total := 0
	items := []json.RawMessage{}
	for {
		_, b, readErr := conn.Read(ctx)
		if readErr != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			return out, wire.Truncated()
		}
		total += len(b)
		if total > 16<<20 {
			return out, &provider.Error{Kind: provider.Protocol, Message: "provider output exceeds turn limit"}
		}
		var e struct {
			Type  string          `json:"type"`
			Delta string          `json:"delta"`
			Item  json.RawMessage `json:"item"`
			Code  string          `json:"code"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			Response struct {
				Status string            `json:"status"`
				Output []json.RawMessage `json:"output"`
				Usage  struct {
					Input  int64 `json:"input_tokens"`
					Output int64 `json:"output_tokens"`
				} `json:"usage"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal(b, &e) != nil {
			return out, &provider.Error{Kind: provider.Protocol, Message: "invalid Codex stream frame"}
		}
		switch e.Type {
		case "response.output_text.delta":
			out.Text += e.Delta
			if err = wire.Emit(emit, provider.Event{Type: "text_delta", Text: e.Delta}); err != nil {
				return
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if e.Delta != "" {
				if err = wire.Emit(emit, provider.Event{Type: "reasoning"}); err != nil {
					return
				}
			}
		case "response.function_call_arguments.delta":
			if e.Delta != "" {
				if err = wire.Emit(emit, provider.Event{Type: "tool_arguments"}); err != nil {
					return
				}
			}
		case "response.output_item.done":
			items = append(items, append(json.RawMessage(nil), e.Item...))
		case "error":
			code := e.Error.Code
			if code == "" {
				code = e.Code
			}
			return out, wire.StreamError(code)
		case "response.failed":
			return out, wire.StreamError(e.Response.Error.Code)
		case "response.incomplete":
			return out, &provider.Error{Kind: provider.Protocol, Message: "Codex response was incomplete"}
		case "response.completed":
			if len(e.Response.Output) > 0 {
				items = e.Response.Output
			}
			out.FinishReason = e.Response.Status
			out.Usage = provider.Usage{InputTokens: e.Response.Usage.Input, OutputTokens: e.Response.Usage.Output}
			for _, item := range items {
				var v struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
					Content   []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				}
				if json.Unmarshal(item, &v) != nil {
					return out, &provider.Error{Kind: provider.Protocol, Message: "invalid Codex output item"}
				}
				if v.Type == "function_call" {
					if v.CallID == "" || v.Name == "" || !json.Valid([]byte(v.Arguments)) {
						return out, &provider.Error{Kind: provider.Protocol, Message: "invalid Codex tool call"}
					}
					call := provider.ToolCall{ID: v.CallID, Name: v.Name, Arguments: json.RawMessage(v.Arguments)}
					out.Calls = append(out.Calls, call)
					if err = wire.Emit(emit, provider.Event{Type: "tool_call", Call: &call}); err != nil {
						return
					}
				}
				replayItem, normalizeErr := normalizeReplay(item)
				if normalizeErr != nil {
					return out, normalizeErr
				}
				out.Replay, err = wire.AppendReplay(out.Replay, replayItem)
				if err != nil {
					return
				}
			}
			var canonical strings.Builder
			for _, item := range items {
				var v struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				}
				_ = json.Unmarshal(item, &v)
				for _, part := range v.Content {
					if part.Type == "output_text" {
						canonical.WriteString(part.Text)
					}
				}
			}
			if final := canonical.String(); final != "" {
				if !strings.HasPrefix(final, out.Text) {
					return out, &provider.Error{Kind: provider.Protocol, Message: "Codex terminal output differs from streamed text"}
				}
				suffix := strings.TrimPrefix(final, out.Text)
				out.Text = final
				if suffix != "" {
					err = wire.Emit(emit, provider.Event{Type: "text_delta", Text: suffix})
				}
			}
			return
		}
	}
}

func normalizeReplay(raw json.RawMessage) (json.RawMessage, error) {
	var item struct {
		Type    string `json:"type"`
		ID      string `json:"id,omitempty"`
		Role    string `json:"role,omitempty"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			ImageURL string `json:"image_url,omitempty"`
			Detail   string `json:"detail,omitempty"`
		} `json:"content,omitempty"`
		CallID           string          `json:"call_id,omitempty"`
		Name             string          `json:"name,omitempty"`
		Arguments        string          `json:"arguments,omitempty"`
		EncryptedContent string          `json:"encrypted_content,omitempty"`
		Summary          json.RawMessage `json:"summary,omitempty"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return nil, &provider.Error{Kind: provider.Protocol, Message: "invalid Codex replay item"}
	}
	switch item.Type {
	case "message":
		item.ID = ""
	case "reasoning", "function_call", "compaction":
	default:
		return nil, &provider.Error{Kind: provider.Protocol, Message: "unsupported Codex replay item"}
	}
	return wire.JSON(item), nil
}
