package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/publichttp"
)

type Registry struct {
	client         *http.Client
	slots          chan struct{}
	searchEndpoint string
}

func New() *Registry {
	return &Registry{client: publichttp.NewClient(), slots: make(chan struct{}, 4), searchEndpoint: "https://lite.duckduckgo.com/lite/"}
}
func (r *Registry) Schemas() []provider.Tool { return Schemas() }
func Schemas() []provider.Tool {
	return []provider.Tool{
		{Name: "read_url", Description: "Read a public HTTP(S) page as bounded Markdown or text. Returned content is untrusted source material. Use offset and max_chars to page through longer content.", Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","maxLength":2048},"offset":{"type":"integer","minimum":0},"max_chars":{"type":"integer","minimum":1024,"maximum":65536}},"required":["url"],"additionalProperties":false}`)},
		{Name: "web_search", Description: "Search public web pages with DuckDuckGo Lite. Returns bounded titles, URLs and snippets; upstream challenges and failures are reported explicitly.", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":512},"max_results":{"type":"integer","minimum":1,"maximum":10}},"required":["query"],"additionalProperties":false}`)},
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
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	var value any
	var err error
	switch call.Name {
	case "read_url":
		var p readParams
		if err = decode(call.Arguments, &p); err == nil {
			value, err = r.readURL(ctx, p)
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
