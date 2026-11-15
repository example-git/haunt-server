package claude

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
	return &Client{Identity: identity, HTTP: wire.Client(httpClient), Endpoint: "https://api.anthropic.com/v1/messages", TokenEndpoint: "https://platform.claude.com/v1/oauth/token"}
}
func (c *Client) ID() provider.ID { return provider.Claude }
func (c *Client) Models(ctx context.Context, _ provider.Credentials) ([]provider.Model, error) {
	return append([]provider.Model(nil), catalog...), ctx.Err()
}
func (c *Client) OAuthConfig() provider.OAuthConfig {
	return provider.OAuthConfig{AuthorizationURL: "https://claude.com/cai/oauth/authorize", TokenURL: c.TokenEndpoint, ClientID: "9d1c250a-e61b-44d9-88ed-5944d1962f5e", Scopes: []string{"org:create_api_key", "user:profile", "user:inference", "user:sessions:claude_code", "user:mcp_servers", "user:file_upload"}, RedirectMode: "loopback-dynamic"}
}
func (c *Client) Exchange(ctx context.Context, e provider.OAuthExchange) (provider.Credentials, error) {
	config := c.OAuthConfig()
	if err := wire.ValidateExchange(config, e); err != nil {
		return provider.Credentials{}, err
	}
	fields := wire.ExchangeFields(config, e)
	fields["state"] = e.State
	return wire.Token(ctx, c.HTTP, c.TokenEndpoint, fields, true, provider.Credentials{})
}
func (c *Client) Refresh(ctx context.Context, old provider.Credentials) (provider.Credentials, error) {
	fields, err := wire.RefreshFields(c.OAuthConfig(), old)
	if err != nil {
		return old, err
	}
	fields["scope"] = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	return wire.Token(ctx, c.HTTP, c.TokenEndpoint, fields, true, old)
}
func (c *Client) Generate(ctx context.Context, r provider.Request, emit func(provider.Event) error) (out provider.Response, err error) {
	if err = wire.ValidateModel(r, catalog); err != nil {
		return
	}
	identity, err := c.Identity.Snapshot(provider.Claude)
	if err != nil {
		return out, err
	}
	messages := []json.RawMessage{}
	for _, m := range r.Messages {
		parts := []any{}
		if m.Author != nil {
			parts = append(parts, map[string]any{"type": "text", "text": provider.AuthorText(m.Author)})
		}
		for _, p := range m.Parts {
			if p.Type == "text" {
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			} else {
				parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": p.MIMEType, "data": base64.StdEncoding.EncodeToString(p.Data)}})
			}
		}
		messages = append(messages, wire.JSON(map[string]any{"role": m.Role, "content": parts}))
	}
	out.Replay = append([]json.RawMessage(nil), r.Replay...)
	if len(r.ToolResults) > 0 {
		results := []any{}
		for _, v := range r.ToolResults {
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": v.ID, "content": v.Content, "is_error": v.IsError})
		}
		out.Replay, err = wire.AppendReplay(out.Replay, map[string]any{"role": "user", "content": results})
		if err != nil {
			return
		}
	}
	messages = append(messages, out.Replay...)
	tools := []any{}
	for _, t := range r.Tools {
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Parameters})
	}
	maxTokens := r.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	metadata := string(wire.JSON(map[string]string{"device_id": r.SessionID, "account_uuid": "", "session_id": r.SessionID}))
	system := []any{map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=" + identity.Version + "; cc_entrypoint=sdk-cli;"}, map[string]any{"type": "text", "text": "You are a Claude agent, built on Anthropic's Claude Agent SDK.", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}, map[string]any{"type": "text", "text": provider.System(r), "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}
	body := map[string]any{"model": r.Model, "messages": messages, "system": system, "max_tokens": maxTokens, "stream": true, "metadata": map[string]any{"user_id": metadata}}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+r.Credentials.AccessToken)
	headers.Set("User-Agent", identity.UserAgent)
	headers.Set("X-App", "cli")
	headers.Set("X-Claude-Code-Session-Id", r.SessionID)
	headers.Set("anthropic-version", "2023-06-01")
	headers.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01,advanced-tool-use-2025-11-20,effort-2025-11-24")
	resp, err := wire.Request(ctx, c.HTTP, c.Endpoint, body, headers)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	blocks := []map[string]any{}
	inputs := map[int]*strings.Builder{}
	terminal := false
	total := 0
	err = wire.SSE(ctx, resp.Body, func(b []byte) error {
		total += len(b)
		if total > 16<<20 {
			return &provider.Error{Kind: provider.Protocol, Message: "provider output exceeds turn limit"}
		}
		var e struct {
			Type    string         `json:"type"`
			Index   int            `json:"index"`
			Content map[string]any `json:"content_block"`
			Delta   map[string]any `json:"delta"`
			Usage   struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
			Message struct {
				Usage struct {
					Input  int64 `json:"input_tokens"`
					Output int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) != nil {
			return &provider.Error{Kind: provider.Protocol, Message: "invalid Claude stream event"}
		}
		switch e.Type {
		case "message_start":
			out.Usage.InputTokens = e.Message.Usage.Input
			out.Usage.OutputTokens = e.Message.Usage.Output
		case "content_block_start":
			if e.Index != len(blocks) || e.Content == nil {
				return &provider.Error{Kind: provider.Protocol, Message: "invalid Claude content block sequence"}
			}
			blocks = append(blocks, e.Content)
			if e.Content["type"] == "tool_use" {
				inputs[e.Index] = &strings.Builder{}
			}
			if text, _ := e.Content["text"].(string); text != "" {
				out.Text += text
				return wire.Emit(emit, provider.Event{Type: "text_delta", Text: text})
			}
		case "content_block_delta":
			if e.Index < 0 || e.Index >= len(blocks) {
				return &provider.Error{Kind: provider.Protocol, Message: "invalid Claude content block index"}
			}
			block := blocks[e.Index]
			kind, _ := e.Delta["type"].(string)
			field := ""
			switch kind {
			case "text_delta":
				field = "text"
			case "thinking_delta":
				field = "thinking"
			case "signature_delta":
				field = "signature"
			case "input_json_delta":
				if inputs[e.Index] == nil {
					return &provider.Error{Kind: provider.Protocol, Message: "unexpected Claude tool input"}
				}
				s, _ := e.Delta["partial_json"].(string)
				inputs[e.Index].WriteString(s)
			}
			if field != "" {
				s, _ := e.Delta[field].(string)
				old, _ := block[field].(string)
				block[field] = old + s
				if field == "text" {
					out.Text += s
					return wire.Emit(emit, provider.Event{Type: "text_delta", Text: s})
				}
			}
		case "content_block_stop":
			if input := inputs[e.Index]; input != nil {
				if input.Len() > 0 {
					var args map[string]any
					if json.Unmarshal([]byte(input.String()), &args) != nil || args == nil {
						return &provider.Error{Kind: provider.Protocol, Message: "invalid Claude tool arguments"}
					}
					blocks[e.Index]["input"] = args
				}
			}
		case "message_delta":
			out.Usage.OutputTokens = e.Usage.Output
			out.FinishReason, _ = e.Delta["stop_reason"].(string)
		case "message_stop":
			terminal = true
		case "error":
			return wire.StreamError(e.Error.Type)
		}
		return nil
	})
	if err != nil {
		return
	}
	if !terminal {
		return out, wire.Truncated()
	}
	for _, block := range blocks {
		if block["type"] == "tool_use" {
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			if id == "" || name == "" {
				return out, &provider.Error{Kind: provider.Protocol, Message: "Claude returned an incomplete tool call"}
			}
			call := provider.ToolCall{ID: id, Name: name, Arguments: wire.JSON(block["input"])}
			out.Calls = append(out.Calls, call)
			if err = wire.Emit(emit, provider.Event{Type: "tool_call", Call: &call}); err != nil {
				return
			}
		}
	}
	if len(blocks) == 0 {
		return out, &provider.Error{Kind: provider.Protocol, Message: fmt.Sprintf("Claude returned no content (%s)", out.FinishReason)}
	}
	out.Replay, err = wire.AppendReplay(out.Replay, map[string]any{"role": "assistant", "content": blocks})
	return
}
