package tools

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/example-git/haunt-server/internal/publichttp"
	"golang.org/x/net/html"
)

type searchParams struct {
	Query      string `json:"query"`
	MaxResults *int   `json:"max_results"`
}
type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func (r *Registry) search(ctx context.Context, p searchParams) ([]searchResult, error) {
	limit := 5
	if p.MaxResults != nil {
		limit = *p.MaxResults
	}
	if strings.TrimSpace(p.Query) == "" || len(p.Query) > 512 || limit < 1 || limit > 10 {
		return nil, errors.New("query must contain 1-512 bytes and max_results must be between 1 and 10")
	}
	b, _, _, err := r.fetch(ctx, r.searchEndpoint+"?q="+url.QueryEscape(p.Query))
	if err != nil {
		return nil, err
	}
	for _, marker := range []string{"anomaly-modal", "/anomaly.js", "Unfortunately, bots use DuckDuckGo too"} {
		if bytes.Contains(b, []byte(marker)) {
			return nil, errors.New("DuckDuckGo blocked this search with a bot challenge; results are unavailable")
		}
	}
	doc, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("invalid DuckDuckGo response")
	}
	results := []searchResult{}
	var current *searchResult
	var walk func(*html.Node) error
	walk = func(n *html.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		classes := " " + attr(n, "class") + " "
		if n.Type == html.ElementNode && n.Data == "a" && strings.Contains(classes, " result-link ") {
			if current != nil {
				results = append(results, *current)
				current = nil
			}
			if len(results) >= limit {
				return nil
			}
			raw := attr(n, "href")
			u, err := url.Parse(raw)
			if err == nil && (u.Hostname() == "duckduckgo.com" || u.Hostname() == "www.duckduckgo.com") && u.Path == "/l/" {
				raw = u.Query().Get("uddg")
			}
			u, err = publichttp.ParseURL(raw)
			if err == nil && publichttp.PublicHost(u.Hostname()) {
				current = &searchResult{Title: bounded(text(n), 512), URL: u.String()}
			}
		}
		if current != nil && n.Type == html.ElementNode && strings.Contains(classes, " result-snippet ") {
			current.Snippet = bounded(text(n), 2048)
		}
		for c := n.FirstChild; c != nil && len(results) < limit; c = c.NextSibling {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err = walk(doc); err != nil {
		return nil, err
	}
	if current != nil && len(results) < limit {
		results = append(results, *current)
	}
	if len(results) == 0 && !bytes.Contains(bytes.ToLower(b), []byte("no results")) {
		return nil, errors.New("DuckDuckGo returned no recognizable results")
	}
	return results, nil
}
func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:min(len(r), n/4)])
}
