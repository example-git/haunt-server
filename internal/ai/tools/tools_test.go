package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/publichttp"
)

func fixture(t *testing.T, handler http.HandlerFunc) (*Registry, *atomic.Int32) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	r := New()
	r.searchEndpoint = "https://search.example.com/lite/"
	calls := &atomic.Int32{}
	transport := r.client.Transport.(*http.Transport)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return publichttp.DialPublic(ctx, network, address, func(_ context.Context, _ string, host string) ([]netip.Addr, error) {
			if ip, err := netip.ParseAddr(host); err == nil {
				return []netip.Addr{ip}, nil
			}
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}, func(ctx context.Context, network, address string) (net.Conn, error) {
			calls.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "https://"))
		})
	}
	return r, calls
}
func execute(t *testing.T, r *Registry, name, args string) (provider.ToolResult, error) {
	t.Helper()
	return r.Execute(t.Context(), provider.ToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(args)})
}
func TestReadURLMarkdownBoundsAndPagination(t *testing.T) {
	r, _ := fixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Error("forwarded credentials")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>hidden head</title></head><body><script>script secret</script><h1>Heading</h1><p>Read <strong>bold</strong> <a href="/source">source</a>.</p><p hidden>hidden text</p><pre>code &lt;tag&gt;</pre><p>`+strings.Repeat("é", 2048)+`</p></body></html>`)
	})
	result, err := execute(t, r, "read_url", `{"url":"https://public.example.com/page","max_chars":1024}`)
	if err != nil {
		t.Fatal(err)
	}
	var first page
	if json.Unmarshal([]byte(result.Content), &first) != nil {
		t.Fatal("invalid result JSON")
	}
	for _, want := range []string{"# Heading", "**bold**", "[source](https://public.example.com/source)", "code <tag>"} {
		if !strings.Contains(first.Content, want) {
			t.Errorf("missing Markdown %q: %s", want, first.Content)
		}
	}
	for _, unwanted := range []string{"script secret", "hidden text", "hidden head"} {
		if strings.Contains(first.Content, unwanted) {
			t.Errorf("hidden HTML leaked %s", unwanted)
		}
	}
	if len([]rune(first.Content)) != 1024 || first.NextOffset == nil || *first.NextOffset != 1024 {
		t.Fatalf("invalid page bounds: %+v", first)
	}
	result, err = execute(t, r, "read_url", `{"url":"https://public.example.com/page","offset":1024,"max_chars":1024}`)
	if err != nil {
		t.Fatal(err)
	}
	var second page
	_ = json.Unmarshal([]byte(result.Content), &second)
	if second.Offset != 1024 || !strings.HasPrefix(second.Content, "é") {
		t.Error("UTF-8 page offset lost")
	}
}
func TestToolRegistryRejectsUnknownInvalidAndPrivateRequests(t *testing.T) {
	r, calls := fixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "not reached") })
	for _, tc := range []struct{ name, args string }{{"image_gen", `{}`}, {"shell", `{}`}, {"read_url", `{"url":"https://user:pass@public.example.com"}`}, {"read_url", `{"url":"http://127.0.0.1/"}`}, {"read_url", `{"url":"http://169.254.169.254/"}`}, {"read_url", `{"url":"https://public.example.com:8443"}`}, {"read_url", `{"url":"https://public.example.com","max_chars":0}`}, {"read_url", `{"url":"https://public.example.com","unknown":true}`}, {"web_search", `{"query":"hello","max_results":0}`}, {"web_search", `{"query":""}`}} {
		t.Run(tc.name+tc.args, func(t *testing.T) {
			result, err := execute(t, r, tc.name, tc.args)
			if err == nil || !result.IsError {
				t.Fatalf("invalid tool input accepted: %+v %v", result, err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Error("invalid request dialed network")
	}
	schemas := r.Schemas()
	if len(schemas) != 2 || schemas[0].Name != "read_url" || schemas[1].Name != "web_search" {
		t.Fatal("unexpected executable tools")
	}
}
func TestReadURLRedirectCannotReachPrivateDestination(t *testing.T) {
	r, calls := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1/metadata", http.StatusFound)
	})
	_, err := execute(t, r, "read_url", `{"url":"https://public.example.com"}`)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("redirect escaped restrictions: %v %d", err, calls.Load())
	}
}
func TestToolInputBoundAndCancellation(t *testing.T) {
	r, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("x", maxInputBytes+1))
	})
	_, err := execute(t, r, "read_url", `{"url":"https://public.example.com"}`)
	if err == nil || !strings.Contains(err.Error(), "2 MiB") {
		t.Fatal("unbounded page accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = r.Execute(ctx, provider.ToolCall{ID: "cancelled", Name: "read_url", Arguments: json.RawMessage(`{"url":"https://public.example.com"}`)})
	if err == nil {
		t.Fatal("cancelled tool executed")
	}
}
func TestSearchParsesBoundedResultsAndReportsChallenges(t *testing.T) {
	var challenge atomic.Bool
	r, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "a & b" {
			t.Error("query not encoded")
		}
		w.Header().Set("Content-Type", "text/html")
		if challenge.Load() {
			fmt.Fprint(w, `<form class="anomaly-modal">challenge</form>`)
			return
		}
		fmt.Fprint(w, `<table><tr><td><a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fone.example.com%2Farticle">One</a></td></tr><tr><td class="result-snippet">First <b>snippet</b></td></tr><tr><td><a class="result-link" href="https://two.example.com/">Two</a></td></tr><tr><td class="result-snippet">Second snippet</td></tr></table>`)
	})
	result, err := execute(t, r, "web_search", `{"query":"a & b","max_results":1}`)
	if err != nil {
		t.Fatal(err)
	}
	var results []searchResult
	if json.Unmarshal([]byte(result.Content), &results) != nil || len(results) != 1 || results[0].URL != "https://one.example.com/article" || results[0].Snippet != "First snippet" {
		t.Fatalf("bad search results: %s", result.Content)
	}
	challenge.Store(true)
	result, err = execute(t, r, "web_search", `{"query":"a & b"}`)
	if err == nil || !result.IsError || !strings.Contains(result.Content, "bot challenge") {
		t.Fatal("challenge reported as empty results")
	}
}
