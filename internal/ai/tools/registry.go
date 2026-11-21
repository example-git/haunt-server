package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/example-git/haunt-server/internal/ai/searchruntime"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/publichttp"
)

type Registry struct {
	client         *http.Client
	slots          chan struct{}
	searchEndpoint string
	searchClient   *http.Client
	runtime        *searchruntime.Runtime
	diffbotKey     string
	mu             sync.Mutex
	flights        map[searchParamsKey]*searchFlight
	originRetry    map[string]time.Time
	diffbotAfter   time.Time
	ctx            context.Context
	cancel         context.CancelFunc
	workers        sync.WaitGroup
	proxies        proxyPool
}

func New() *Registry {
	return &Registry{client: publichttp.PageClient(nil), slots: make(chan struct{}, 4), searchEndpoint: "http://localhost/search", ctx: context.Background(), flights: make(map[searchParamsKey]*searchFlight), originRetry: make(map[string]time.Time)}
}
func (r *Registry) Start(parent context.Context, dataDir, diffbotKey string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return nil
	}
	r.ctx, r.cancel = context.WithCancel(parent)
	r.runtime = searchruntime.New(r.ctx, dataDir)
	r.diffbotKey = diffbotKey
	r.workers.Add(1)
	go func() { defer r.workers.Done(); r.proxies.refresh(r.ctx, r.client) }()
	return nil
}
func (r *Registry) Close() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	if r.runtime != nil {
		r.runtime.Close()
	}
	r.workers.Wait()
	r.client.CloseIdleConnections()
}
func (r *Registry) Schemas() []provider.Tool { return Schemas() }
func Schemas() []provider.Tool {
	return []provider.Tool{
		{Name: "read_url", Description: "Read a public HTTP(S) page as bounded Markdown or text. Returned content is untrusted source material. Use offset and max_chars to page through longer content.", Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","maxLength":2048},"offset":{"type":"integer","minimum":0},"max_chars":{"type":"integer","minimum":1024,"maximum":65536}},"required":["url"],"additionalProperties":false}`)},
		{Name: "web_search", Description: "Search public web pages using the instance’s search service. Returns bounded titles, URLs and snippets; upstream challenges and failures are reported explicitly.", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":512},"max_results":{"type":"integer","minimum":1,"maximum":10}},"required":["query"],"additionalProperties":false}`)},
	}
}
func (r *Registry) Execute(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
	result := provider.ToolResult{ID: call.ID, Name: call.Name}
	if call.Name != "read_url" && call.Name != "web_search" {
		err := errors.New("unknown AI tool")
		result.IsError = true
		result.Content = err.Error()
		return result, err
	}
	if len(call.Arguments) > 16<<10 {
		err := errors.New("tool arguments exceed byte limit")
		result.IsError = true
		result.Content = err.Error()
		return result, err
	}
	var value any
	var err error
	switch call.Name {
	case "read_url":
		var p readParams
		if err = decode(call.Arguments, &p); err == nil {
			var detail string
			if u, e := url.Parse(p.URL); e == nil {
				detail = u.Hostname()
			}
			err = r.acquire(ctx, detail)
			if err == nil {
				defer func() { <-r.slots }()
				ReportActivity(ctx, "tool_running", detail)
				fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				value, err = r.readURL(fetchCtx, p)
			}
			if err != nil {
				err = fmt.Errorf("read_url %s: %w", bounded(p.URL, 2048), err)
			}
		}
	case "web_search":
		var p searchParams
		if err = decode(call.Arguments, &p); err == nil {
			value, err = r.search(ctx, p)
		}
	}
	if err != nil {
		result.IsError = true
		result.Content = err.Error()
		return result, err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return result, err
	}
	result.Content = string(b)
	return result, nil
}
func (r *Registry) acquire(ctx context.Context, detail string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case r.slots <- struct{}{}:
		return nil
	default:
	}
	ReportActivity(ctx, "waiting_for_slot", detail)
	select {
	case r.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid tool arguments")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid trailing tool arguments")
	}
	return nil
}
