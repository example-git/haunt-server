package antigravity

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/example-git/haunt-server/internal/ai/clientidentity"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/ai/providers/wire"
	"github.com/google/uuid"
)

type Client struct {
	Identity         *clientidentity.Manager
	HTTP             *http.Client
	Endpoint         string
	TokenEndpoint    string
	ProjectEndpoint  string
	IdentityEndpoint string
	agentID          string
	startMS          int64
	sequence         atomic.Uint64
}

func New(identity *clientidentity.Manager, httpClient *http.Client) *Client {
	return &Client{Identity: identity, HTTP: wire.Client(httpClient), Endpoint: "https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse", TokenEndpoint: "https://oauth2.googleapis.com/token", ProjectEndpoint: "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist", IdentityEndpoint: "https://www.googleapis.com/oauth2/v2/userinfo", agentID: uuid.NewString(), startMS: time.Now().UnixMilli()}
}
func (c *Client) ID() provider.ID { return provider.Antigravity }
func (c *Client) Models(ctx context.Context, _ provider.Credentials) ([]provider.Model, error) {
	return append([]provider.Model(nil), catalog...), ctx.Err()
}
func (c *Client) OAuthConfig() provider.OAuthConfig {
	return provider.OAuthConfig{AuthorizationURL: "https://accounts.google.com/o/oauth2/auth", TokenURL: c.TokenEndpoint, ClientID: "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com", Scopes: []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile", "https://www.googleapis.com/auth/cclog", "https://www.googleapis.com/auth/experimentsandconfigs", "openid"}, RedirectMode: "hosted-paste", RedirectURI: "https://antigravity.google/oauth-callback", ExtraParams: map[string]string{"access_type": "offline", "prompt": "consent"}}
}

const installedClientSecret = "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"

func (c *Client) Exchange(ctx context.Context, e provider.OAuthExchange) (provider.Credentials, error) {
	config := c.OAuthConfig()
	if err := wire.ValidateExchange(config, e); err != nil {
		return provider.Credentials{}, err
	}
	fields := wire.ExchangeFields(config, e)
	fields["client_secret"] = installedClientSecret
	credentials, err := wire.Token(ctx, c.HTTP, c.TokenEndpoint, fields, false, provider.Credentials{})
	if err != nil {
		return credentials, err
	}
	identity, err := c.Identity.Snapshot(provider.Antigravity)
	if err != nil {
		return credentials, err
	}
	credentials.ProjectID, err = c.project(ctx, credentials, identity)
	if err != nil {
		return credentials, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.IdentityEndpoint, nil)
	if err != nil {
		return credentials, err
	}
	req.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return credentials, &provider.Error{Kind: provider.Unavailable, Message: "Google account identity unavailable"}
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return credentials, wire.Status(resp.StatusCode, resp.Header)
	}
	var user struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err = wire.ReadJSON(resp, &user); err != nil {
		return credentials, err
	}
	credentials.AccountID = user.ID
	credentials.Metadata = wire.JSON(map[string]string{"email": user.Email})
	return credentials, nil
}
func (c *Client) Refresh(ctx context.Context, old provider.Credentials) (provider.Credentials, error) {
	fields, err := wire.RefreshFields(c.OAuthConfig(), old)
	if err != nil {
		return old, err
	}
	fields["client_secret"] = installedClientSecret
	return wire.Token(ctx, c.HTTP, c.TokenEndpoint, fields, false, old)
}
func (c *Client) project(ctx context.Context, credentials provider.Credentials, identity clientidentity.Snapshot) (string, error) {
	if credentials.ProjectID != "" {
		return credentials.ProjectID, nil
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+credentials.AccessToken)
	headers.Set("User-Agent", identity.UserAgent)
	headers.Set("Accept", "application/json")
	resp, err := wire.Request(ctx, c.HTTP, c.ProjectEndpoint, map[string]any{}, headers)
	if err != nil {
		return "", err
	}
	var result struct {
		Project json.RawMessage `json:"cloudaicompanionProject"`
	}
	if err = wire.ReadJSON(resp, &result); err != nil {
		return "", err
	}
	var id string
	if len(result.Project) == 0 || string(result.Project) == "null" {
		return "", nil
	}
	if json.Unmarshal(result.Project, &id) == nil {
		return id, nil
	}
	var object struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(result.Project, &object) != nil {
		return "", &provider.Error{Kind: provider.Protocol, Message: "invalid Cloud Code project metadata"}
	}
	return object.ID, nil
}
func sanitize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			switch k {
			case "$schema", "additionalProperties", "propertyNames", "exclusiveMinimum", "exclusiveMaximum", "const", "unevaluatedProperties", "patternProperties":
				continue
			}
			out[k] = sanitize(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = sanitize(val)
		}
		return out
	}
	return v
}
func (c *Client) Generate(ctx context.Context, r provider.Request, emit func(provider.Event) error) (out provider.Response, err error) {
	if err = wire.ValidateModel(r, catalog); err != nil {
		return
	}
	identity, err := c.Identity.Snapshot(provider.Antigravity)
	if err != nil {
		return out, err
	}
	project, err := c.project(ctx, r.Credentials, identity)
	if err != nil {
		return out, err
	}
	contents := []json.RawMessage{}
	for _, m := range r.Messages {
		parts := []any{}
		if m.Author != nil {
			parts = append(parts, map[string]any{"text": provider.AuthorText(m.Author)})
		}
		for _, p := range m.Parts {
			if p.Type == "text" {
				parts = append(parts, map[string]any{"text": p.Text})
			} else {
				parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": p.MIMEType, "data": p.Data}})
			}
		}
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, wire.JSON(map[string]any{"role": role, "parts": parts}))
	}
	out.Replay = append([]json.RawMessage(nil), r.Replay...)
	signatures := map[string]string{}
	names := map[string]string{}
	lastSignature := ""
	for _, raw := range out.Replay {
		var v struct {
			Parts []struct {
				Signature string `json:"thoughtSignature"`
				Call      *struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"functionCall"`
			} `json:"parts"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return out, &provider.Error{Kind: provider.InvalidRequest, Message: "invalid Antigravity replay"}
		}
		for _, p := range v.Parts {
			if p.Signature != "" {
				lastSignature = p.Signature
			}
			if p.Call != nil {
				signatures[p.Call.ID] = lastSignature
				names[p.Call.ID] = p.Call.Name
			}
		}
	}
	for _, v := range r.ToolResults {
		sig := signatures[v.ID]
		if sig == "" {
			sig = lastSignature
		}
		name := names[v.ID]
		if name == "" {
			return out, &provider.Error{Kind: provider.InvalidRequest, Message: "tool result has no matching Antigravity call"}
		}
		part := map[string]any{"functionResponse": map[string]any{"id": v.ID, "name": name, "response": map[string]any{"output": v.Content}}}
		if sig != "" {
			part["thoughtSignature"] = sig
		}
		out.Replay, err = wire.AppendReplay(out.Replay, map[string]any{"role": "model", "parts": []any{part}})
		if err != nil {
			return
		}
	}
	out.RoundStart = len(out.Replay)
	contents = append(contents, out.Replay...)
	maxTokens := r.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	config := map[string]any{"maxOutputTokens": maxTokens}
	if strings.Contains(r.Model, "thinking") || strings.Contains(r.Model, "claude") {
		budget := min(4096, maxTokens-1)
		if budget > 0 {
			config["thinkingConfig"] = map[string]any{"includeThoughts": true, "thinkingBudget": budget}
		}
	} else if strings.HasPrefix(r.Model, "gemini-") {
		config["thinkingConfig"] = map[string]any{"includeThoughts": true, "thinkingLevel": "MEDIUM"}
	}
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		return
	}
	session := "-" + strconv.FormatUint(binary.BigEndian.Uint64(random[:])%9_000_000_000_000_000+1_000_000_000_000_000, 10)
	request := map[string]any{"contents": contents, "systemInstruction": map[string]any{"parts": []any{map[string]any{"text": provider.System(r)}}}, "generationConfig": config, "sessionId": session}
	if len(r.Tools) > 0 {
		declarations := []any{}
		for _, t := range r.Tools {
			var schema any
			if json.Unmarshal(t.Parameters, &schema) != nil {
				return out, &provider.Error{Kind: provider.InvalidRequest, Message: "invalid tool schema"}
			}
			declarations = append(declarations, map[string]any{"name": t.Name, "description": t.Description, "parameters": sanitize(schema)})
		}
		request["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
		request["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}
	}
	body := map[string]any{"project": project, "requestId": fmt.Sprintf("agent/%s/%d/%s/%d", c.agentID, c.startMS, r.SessionID, c.sequence.Add(1)), "request": request, "model": r.Model, "userAgent": "antigravity", "requestType": "agent"}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+r.Credentials.AccessToken)
	headers.Set("User-Agent", identity.UserAgent)
	headers.Set("Accept", "*/*")
	resp, err := wire.StreamRequest(ctx, c.HTTP, c.Endpoint, body, headers, emit)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	parts := []map[string]any{}
	terminal := false
	total := 0
	err = wire.SSE(ctx, resp.Body, func(b []byte) error {
		total += len(b)
		if total > 16<<20 {
			return &provider.Error{Kind: provider.Protocol, Message: "provider output exceeds turn limit"}
		}
		var envelope struct {
			Response json.RawMessage `json:"response"`
			Error    json.RawMessage `json:"error"`
		}
		if json.Unmarshal(b, &envelope) != nil {
			return &provider.Error{Kind: provider.Protocol, Message: "invalid Antigravity envelope"}
		}
		if len(envelope.Response) > 0 {
			b = envelope.Response
		}
		var e struct {
			Candidates []struct {
				Content struct {
					Parts []map[string]any `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			Usage struct {
				Input    int64 `json:"promptTokenCount"`
				Output   int64 `json:"candidatesTokenCount"`
				Thoughts int64 `json:"thoughtsTokenCount"`
			} `json:"usageMetadata"`
			Error *struct {
				Code   int    `json:"code"`
				Status string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) != nil {
			return &provider.Error{Kind: provider.Protocol, Message: "invalid Antigravity stream event"}
		}
		if e.Error != nil {
			if e.Error.Code > 0 {
				return wire.Status(e.Error.Code, nil)
			}
			return wire.StreamError(e.Error.Status)
		}
		if e.Usage.Input > 0 {
			out.Usage.InputTokens = e.Usage.Input
		}
		if e.Usage.Output > 0 || e.Usage.Thoughts > 0 {
			out.Usage.OutputTokens = e.Usage.Output + e.Usage.Thoughts
		}
		if len(e.Candidates) > 1 {
			return &provider.Error{Kind: provider.Protocol, Message: "unexpected multiple Antigravity candidates"}
		}
		for _, candidate := range e.Candidates {
			if candidate.FinishReason != "" {
				terminal = true
				out.FinishReason = candidate.FinishReason
			}
			for _, p := range candidate.Content.Parts {
				if sig, _ := p["thoughtSignature"].(string); sig != "" {
					lastSignature = sig
				}
				thought, _ := p["thought"].(bool)
				if text, _ := p["text"].(string); text != "" && !thought {
					out.Text += text
					if err := wire.Emit(emit, provider.Event{Type: "text_delta", Text: text}); err != nil {
						return err
					}
				}
				if call, ok := p["functionCall"].(map[string]any); ok {
					id, _ := call["id"].(string)
					if id == "" {
						id = uuid.NewString()
						call["id"] = id
					}
					name, _ := call["name"].(string)
					args, ok := call["args"].(map[string]any)
					if name == "" || !ok {
						return &provider.Error{Kind: provider.Protocol, Message: "invalid Antigravity tool call"}
					}
					if _, ok := p["thoughtSignature"]; !ok && lastSignature != "" {
						p["thoughtSignature"] = lastSignature
					}
					toolCall := provider.ToolCall{ID: id, Name: name, Arguments: wire.JSON(args), Signature: lastSignature}
					out.Calls = append(out.Calls, toolCall)
					if err := wire.Emit(emit, provider.Event{Type: "tool_call", Call: &toolCall}); err != nil {
						return err
					}
				}
				if thought {
					if value, _ := p["text"].(string); value != "" {
						if err := wire.Emit(emit, provider.Event{Type: "reasoning"}); err != nil {
							return err
						}
					}
					if p["thoughtSignature"] == nil {
						continue
					}
					delete(p, "text")
				}
				parts = append(parts, p)
			}
		}
		return nil
	})
	if err != nil {
		return
	}
	if !terminal {
		return out, wire.Truncated()
	}
	if out.FinishReason != "STOP" && out.FinishReason != "MAX_TOKENS" {
		return out, &provider.Error{Kind: provider.Protocol, Message: "Antigravity stopped without a complete response: " + out.FinishReason}
	}
	if len(parts) > 0 {
		out.Replay, err = wire.AppendReplay(out.Replay, map[string]any{"role": "model", "parts": parts})
	}
	return
}
