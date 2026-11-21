package wire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/example-git/haunt-server/internal/ai/provider"
)

func Client(c *http.Client) *http.Client {
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	out := *c
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &out
}
func JSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func Status(code int, h http.Header) error {
	kind := provider.InvalidRequest
	switch {
	case code == 401 || code == 403:
		kind = provider.RequiresAuth
	case code == 429:
		kind = provider.RateLimit
	case code >= 500:
		kind = provider.Unavailable
	}
	delay := time.Duration(0)
	if h != nil {
		if seconds, err := strconv.Atoi(h.Get("Retry-After")); err == nil && seconds > 0 {
			delay = time.Duration(seconds) * time.Second
		} else if at, err := http.ParseTime(h.Get("Retry-After")); err == nil {
			delay = time.Until(at)
			if delay < 0 {
				delay = 0
			}
		}
	}
	return &provider.Error{Kind: kind, Status: code, RetryAfter: delay, Message: fmt.Sprintf("provider returned HTTP %d", code)}
}
func Request(ctx context.Context, c *http.Client, endpoint string, body any, headers http.Header) (*http.Response, error) {
	return request(ctx, Client(c), endpoint, body, headers)
}

type streamBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *streamBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

func StreamRequest(ctx context.Context, c *http.Client, endpoint string, body any, headers http.Header, emit func(provider.Event) error) (*http.Response, error) {
	client := Client(c)
	client.Timeout = 0
	stream, cancel := context.WithCancelCause(ctx)
	connection := time.AfterFunc(30*time.Second, func() { cancel(context.DeadlineExceeded) })
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connection.Stop() },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				if err := Emit(emit, provider.Event{Type: "submitted"}); err != nil {
					cancel(err)
				}
			}
		},
	}
	resp, err := request(httptrace.WithClientTrace(stream, trace), client, endpoint, body, headers)
	connection.Stop()
	if err != nil {
		cause := context.Cause(stream)
		cancel(nil)
		if cause == context.DeadlineExceeded && ctx.Err() == nil {
			return nil, &provider.Error{Kind: provider.Unavailable, Message: "provider connection timed out"}
		}
		return nil, err
	}
	resp.Body = &streamBody{resp.Body, func() { cancel(nil) }}
	return resp, nil
}

func request(ctx context.Context, c *http.Client, endpoint string, body any, headers http.Header) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if len(b) > 32<<20 {
		return nil, &provider.Error{Kind: provider.InvalidRequest, Message: "provider request exceeds byte limit"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &provider.Error{Kind: provider.Ambiguous, Message: "provider transport failed after submission; remote outcome is unknown"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, Status(resp.StatusCode, resp.Header)
	}
	return resp, nil
}
func ReadJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return &provider.Error{Kind: provider.Protocol, Message: "provider response exceeds byte limit"}
	}
	if json.Unmarshal(b, v) != nil {
		return &provider.Error{Kind: provider.Protocol, Message: "invalid provider JSON response"}
	}
	return nil
}
func SSE(ctx context.Context, r io.Reader, fn func([]byte) error) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 1<<20)
	var data bytes.Buffer
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		b := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		if bytes.Equal(b, []byte("[DONE]")) {
			data.Reset()
			return nil
		}
		err := fn(b)
		data.Reset()
		return err
	}
	for s.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := s.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if data.Len()+len(value) > 1<<20 {
				return &provider.Error{Kind: provider.Protocol, Message: "provider event exceeds byte limit"}
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := s.Err(); err != nil {
		return &provider.Error{Kind: provider.Ambiguous, Message: "provider stream interrupted; remote outcome is unknown"}
	}
	return flush()
}
func Emit(fn func(provider.Event) error, e provider.Event) error {
	if fn != nil {
		return fn(e)
	}
	return nil
}
func Token(ctx context.Context, c *http.Client, endpoint string, fields map[string]string, asJSON bool, old provider.Credentials) (provider.Credentials, error) {
	var body io.Reader
	contentType := "application/x-www-form-urlencoded"
	if asJSON {
		body = bytes.NewReader(JSON(fields))
		contentType = "application/json"
	} else {
		v := url.Values{}
		for k, x := range fields {
			v.Set(k, x)
		}
		body = strings.NewReader(v.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return old, err
	}
	req.Header.Set("Content-Type", contentType)
	if fields["client_secret"] != "" {
		req.Header.Set("User-Agent", "Go-http-client/2.0")
	}
	resp, err := Client(c).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return old, ctx.Err()
		}
		return old, &provider.Error{Kind: provider.Unavailable, Message: "OAuth token endpoint unavailable"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		if resp.StatusCode == 400 {
			return old, &provider.Error{Kind: provider.RequiresAuth, Status: 400, Message: "OAuth grant rejected; reconnect the account"}
		}
		return old, Status(resp.StatusCode, resp.Header)
	}
	var v struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		IDToken      string `json:"id_token"`
		Account      struct {
			UUID string `json:"uuid"`
		} `json:"account"`
	}
	if err = ReadJSON(resp, &v); err != nil {
		return old, err
	}
	if v.AccessToken == "" {
		return old, &provider.Error{Kind: provider.Protocol, Message: "OAuth response has no access token"}
	}
	old.AccessToken = v.AccessToken
	if v.RefreshToken != "" {
		old.RefreshToken = v.RefreshToken
	}
	old.TokenType = v.TokenType
	if old.TokenType == "" {
		old.TokenType = "Bearer"
	}
	if v.ExpiresIn <= 0 {
		v.ExpiresIn = 3600
	}
	old.ExpiresAt = time.Now().Add(time.Duration(v.ExpiresIn) * time.Second)
	if v.Account.UUID != "" {
		old.AccountID = v.Account.UUID
	}
	for _, token := range []string{v.IDToken, v.AccessToken} {
		pieces := strings.Split(token, ".")
		if len(pieces) != 3 {
			continue
		}
		b, err := base64.RawURLEncoding.DecodeString(pieces[1])
		if err != nil {
			continue
		}
		var claims map[string]json.RawMessage
		if json.Unmarshal(b, &claims) != nil {
			continue
		}
		var auth struct {
			AccountID string `json:"chatgpt_account_id"`
		}
		if json.Unmarshal(claims["https://api.openai.com/auth"], &auth) == nil && auth.AccountID != "" {
			old.AccountID = auth.AccountID
			break
		}
	}
	return old, nil
}
func ExchangeFields(c provider.OAuthConfig, e provider.OAuthExchange) map[string]string {
	return map[string]string{"grant_type": "authorization_code", "client_id": c.ClientID, "code": e.Code, "code_verifier": e.Verifier, "redirect_uri": e.RedirectURI}
}
func RefreshFields(c provider.OAuthConfig, old provider.Credentials) (map[string]string, error) {
	if old.RefreshToken == "" {
		return nil, &provider.Error{Kind: provider.RequiresAuth, Message: "account has no refresh token; reconnect"}
	}
	return map[string]string{"grant_type": "refresh_token", "client_id": c.ClientID, "refresh_token": old.RefreshToken}, nil
}
func ValidateModel(r provider.Request, models []provider.Model) error {
	if err := provider.Validate(r); err != nil {
		return err
	}
	for _, m := range models {
		if m.ID == r.Model {
			for _, message := range r.Messages {
				for _, p := range message.Parts {
					if p.Type == "image" && !m.Vision {
						return &provider.Error{Kind: provider.InvalidRequest, Message: "selected model does not support images"}
					}
				}
			}
			return nil
		}
	}
	return &provider.Error{Kind: provider.InvalidRequest, Message: "selected model is not in this provider's model catalog"}
}
func Truncated() error {
	return &provider.Error{Kind: provider.Ambiguous, Message: "provider stream ended without a terminal response; remote outcome is unknown"}
}
func StreamError(code string) error {
	kind := provider.Protocol
	if strings.Contains(code, "auth") || strings.Contains(code, "permission") || code == "invalid_api_key" || code == "token_expired" || code == "invalid_grant" {
		kind = provider.RequiresAuth
	} else if strings.Contains(code, "rate") || strings.Contains(code, "quota") {
		kind = provider.RateLimit
	} else if strings.Contains(code, "overload") || strings.Contains(code, "server") {
		kind = provider.Unavailable
	}
	return &provider.Error{Kind: kind, Message: "provider stream reported " + safeCode(code)}
}
func safeCode(s string) string {
	if len(s) > 100 {
		return "an error"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return "an error"
		}
	}
	if s == "" {
		return "an error"
	}
	return s
}
func AppendReplay(replay []json.RawMessage, v any) ([]json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return replay, err
	}
	return append(replay, raw), nil
}
func ValidateExchange(c provider.OAuthConfig, e provider.OAuthExchange) error {
	if e.Code == "" || e.Verifier == "" {
		return errors.New("OAuth code and verifier are required")
	}
	return c.ValidateRedirect(e.RedirectURI)
}
