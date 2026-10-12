package turnclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
)

const validResponse = `{"iceServers":[{"urls":["stun:stun.cloudflare.com:3478"]},{"urls":["turn:turn.cloudflare.com:3478?transport=udp","turn:turn.cloudflare.com:53?transport=udp","turns:turn.cloudflare.com:443?transport=tcp"],"username":"temporary-user","credential":"temporary-secret"}]}`

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := New(config.Config{CloudflareTURNKeyID: strings.Repeat("a", 32), CloudflareTURNToken: "server-only-token"})
	c.endpoint = server.URL
	t.Cleanup(c.Close)
	return c
}
func TestCloudflareCredentialIsolationCachingAndRefresh(t *testing.T) {
	var requests atomic.Int64
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body struct {
			TTL int `json:"ttl"`
		}
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer server-only-token" || json.NewDecoder(r.Body).Decode(&body) != nil || body.TTL != 86400 {
			t.Error("credential request contract")
		}
		w.WriteHeader(201)
		w.Write([]byte(validResponse))
	})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := c.Get(context.Background(), "session-a")
			if err != nil || len(result.Servers) != 2 || len(result.Servers[1].URLs) != 2 || time.Until(result.ExpiresAt) < 23*time.Hour {
				t.Error("invalid credentials", err)
			} else {
				result.Servers[1].URLs[0] = "mutated"
			}
		}()
	}
	wg.Wait()
	if requests.Load() != 1 {
		t.Fatal("concurrent requests not coalesced", requests.Load())
	}
	result, err := c.Get(context.Background(), "session-a")
	if err != nil || result.Servers[1].URLs[0] == "mutated" {
		t.Fatal("cache exposed mutable state")
	}
	if _, err = c.Get(context.Background(), "session-b"); err != nil || requests.Load() != 2 {
		t.Fatal("sessions shared credentials")
	}
	c.mu.Lock()
	entry := c.cache["session-a"]
	entry.credentials.ExpiresAt = time.Now().Add(59 * time.Minute)
	c.cache["session-a"] = entry
	c.mu.Unlock()
	if _, err = c.Get(context.Background(), "session-a"); err != nil || requests.Load() != 3 {
		t.Fatal("credentials were not refreshed")
	}
}
func TestCloudflareFailuresAreBoundedAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"denied", 401, "secret upstream detail"}, {"limited", 429, "secret upstream detail"}, {"redirect", 302, validResponse}, {"invalid", 201, "{}"}, {"oversize", 201, strings.Repeat("x", 33000)}, {"foreign-host", 201, strings.ReplaceAll(validResponse, "turn.cloudflare.com", "attacker.invalid")}, {"missing-password", 201, strings.ReplaceAll(validResponse, "temporary-secret", "")}} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Location", "http://must-not-follow.invalid")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			for range 2 {
				if _, err := c.Get(context.Background(), "session"); err != ErrUnavailable {
					t.Fatal("unexpected error", err)
				}
			}
			if requests.Load() != 1 {
				t.Fatal("failure cooldown not applied")
			}
		})
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, "cancelled"); err != ErrUnavailable {
		t.Fatal("cancelled request did not terminate")
	}
	for range cap(c.slots) {
		c.slots <- struct{}{}
	}
	if _, err := c.Get(context.Background(), "over-capacity"); err != ErrUnavailable {
		t.Fatal("unbounded upstream requests")
	}
}
