package tools

import (
	"context"
	"errors"
	"regexp"
	"unicode/utf8"

	"github.com/example-git/haunt-server/internal/ai/provider"
)

type textSnapshot struct{ URL, Content string }
type pageMemoryKey struct{}

// PageMemory belongs to one executing turn. Calls within that turn are sequential.
type PageMemory struct {
	pages map[string]textSnapshot
	order []string
	bytes int
}

func WithPageMemory(ctx context.Context, m *PageMemory) context.Context {
	return context.WithValue(ctx, pageMemoryKey{}, m)
}
func (m *PageMemory) remember(url string, page textSnapshot) {
	if m == nil || len(page.Content) > maxMarkdownBytes {
		return
	}
	if m.pages == nil {
		m.pages = make(map[string]textSnapshot)
	}
	if prev, ok := m.pages[url]; ok {
		m.bytes -= len(prev.Content)
	} else {
		m.order = append(m.order, url)
	}
	m.pages[url] = page
	m.bytes += len(page.Content)
	for m.bytes > 8<<20 || len(m.order) > 8 {
		id := m.order[0]
		m.order = m.order[1:]
		m.bytes -= len(m.pages[id].Content)
		delete(m.pages, id)
	}
}

type passage struct {
	Offset  int    `json:"offset"`
	End     int    `json:"end"`
	Context string `json:"context"`
}

func findPassages(p readParams, page textSnapshot) (any, []provider.Part, error) {
	chars := []rune(page.Content)
	if p.Offset < 0 || p.Offset > len(chars) {
		return nil, nil, errors.New("offset exceeds source content length")
	}
	text := string(chars[p.Offset:])
	spans := regexp.MustCompile("(?i)"+regexp.QuoteMeta(p.Find)).FindAllStringIndex(text, 9)
	matches := []passage{}
	at, previous := p.Offset, 0
	var next *int
	for i, span := range spans {
		at += utf8.RuneCountInString(text[previous:span[0]])
		if i == 8 {
			offset := at
			next = &offset
			break
		}
		end := at + utf8.RuneCountInString(text[span[0]:span[1]])
		matches = append(matches, passage{at, end, string(chars[max(0, at-160):min(len(chars), end+160)])})
		at, previous = end, span[1]
	}
	return map[string]any{"requested_url": p.URL, "url": page.URL, "matches": matches, "next_offset": next, "total_chars": len(chars)}, nil, nil
}
