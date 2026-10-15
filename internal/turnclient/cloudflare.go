// Package turnclient obtains temporary ICE credentials from Cloudflare Realtime
// TURN. The account credential remains on the server, never in admissions.
package turnclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/pion/stun/v4"
)

var ErrUnavailable = errors.New("media_relay_unavailable")

type Credentials struct {
	Servers   []config.ICEServer
	ExpiresAt time.Time
}
type Source interface {
	Get(context.Context, string) (Credentials, error)
}
type cached struct {
	credentials Credentials
	used        time.Time
	retry       time.Time
}
type Client struct {
	http            *http.Client
	endpoint, token string
	mu              sync.Mutex
	cache           map[string]cached
	pending         map[string]chan struct{}
	slots           chan struct{}
	requests        atomic.Uint64
	failures        atomic.Uint64
}

func New(c config.Config) *Client {
	if c.CloudflareTURNKeyID == "" {
		return nil
	}
	return &Client{http: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, endpoint: "https://rtc.live.cloudflare.com/v1/turn/keys/" + url.PathEscape(c.CloudflareTURNKeyID) + "/credentials/generate-ice-servers", token: c.CloudflareTURNToken, cache: map[string]cached{}, pending: map[string]chan struct{}{}, slots: make(chan struct{}, 8)}
}
func clone(value Credentials) Credentials {
	result := Credentials{ExpiresAt: value.ExpiresAt, Servers: make([]config.ICEServer, len(value.Servers))}
	for i, server := range value.Servers {
		result.Servers[i] = server
		result.Servers[i].URLs = append([]string{}, server.URLs...)
	}
	return result
}
func (c *Client) Get(ctx context.Context, key string) (Credentials, error) {
	if len(key) == 0 || len(key) > 128 {
		return Credentials{}, ErrUnavailable
	}
	for {
		c.mu.Lock()
		entry, exists := c.cache[key]
		if exists && time.Now().Before(entry.retry) {
			c.mu.Unlock()
			return Credentials{}, ErrUnavailable
		}
		if exists && time.Until(entry.credentials.ExpiresAt) > time.Hour {
			entry.used = time.Now()
			c.cache[key] = entry
			c.mu.Unlock()
			return clone(entry.credentials), nil
		}
		if wait := c.pending[key]; wait != nil {
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return Credentials{}, ctx.Err()
			}
		}
		select {
		case c.slots <- struct{}{}:
		default:
			c.mu.Unlock()
			return Credentials{}, ErrUnavailable
		}
		wait := make(chan struct{})
		c.pending[key] = wait
		c.mu.Unlock()
		credentials, err := c.fetch(ctx)
		c.mu.Lock()
		if len(c.cache) >= 1024 {
			oldest := ""
			var at time.Time
			for id, entry := range c.cache {
				if oldest == "" || entry.used.Before(at) {
					oldest, at = id, entry.used
				}
			}
			delete(c.cache, oldest)
		}
		entry = cached{credentials: credentials, used: time.Now()}
		if err != nil {
			entry.retry = time.Now().Add(5 * time.Second)
		}
		c.cache[key] = entry
		delete(c.pending, key)
		<-c.slots
		close(wait)
		c.mu.Unlock()
		return clone(credentials), err
	}
}
func (c *Client) fetch(ctx context.Context) (Credentials, error) {
	c.requests.Add(1)
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewBufferString(`{"ttl":86400}`))
	if err != nil {
		return Credentials{}, ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		c.failures.Add(1)
		return Credentials{}, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		c.failures.Add(1)
		return Credentials{}, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 32769))
	var result struct {
		Servers []config.ICEServer `json:"iceServers"`
	}
	if err != nil || len(data) > 32768 || json.Unmarshal(data, &result) != nil || len(result.Servers) == 0 || len(result.Servers) > 8 {
		c.failures.Add(1)
		return Credentials{}, ErrUnavailable
	}
	hasTURN := false
	for i := range result.Servers {
		server := &result.Servers[i]
		if len(server.URLs) < 1 || len(server.URLs) > 8 || len(server.Username) > 512 || len(server.Credential) > 512 {
			c.failures.Add(1)
			return Credentials{}, ErrUnavailable
		}
		urls := []string{}
		for _, address := range server.URLs {
			parsed, err := stun.ParseURI(address)
			if len(address) > 512 || err != nil || (parsed.Host != "turn.cloudflare.com" && parsed.Host != "stun.cloudflare.com") || parsed.Port < 1 || parsed.Port > 65535 {
				c.failures.Add(1)
				return Credentials{}, ErrUnavailable
			}
			if parsed.Port == 53 {
				continue
			} // Browsers reject this alternate port.
			if strings.HasPrefix(address, "turn:") || strings.HasPrefix(address, "turns:") {
				if server.Username == "" || server.Credential == "" {
					c.failures.Add(1)
					return Credentials{}, ErrUnavailable
				}
				hasTURN = true
			}
			urls = append(urls, address)
		}
		server.URLs = urls
	}
	filtered := []config.ICEServer{}
	for _, server := range result.Servers {
		if len(server.URLs) > 0 {
			filtered = append(filtered, server)
		}
	}
	if !hasTURN {
		c.failures.Add(1)
		return Credentials{}, ErrUnavailable
	}
	return Credentials{Servers: filtered, ExpiresAt: started.Add(24 * time.Hour)}, nil
}
func (c *Client) Stats() map[string]any {
	if c == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "credential_requests": c.requests.Load(), "credential_failures": c.failures.Load()}
}
func (c *Client) Close() {
	if c != nil {
		c.http.CloseIdleConnections()
	}
}
