package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/example-git/haunt-server/internal/publichttp"
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
type searchParamsKey struct {
	query string
	limit int
}
type searchFlight struct {
	started chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	results []searchResult
	err     error
}

func (r *Registry) search(ctx context.Context, p searchParams) ([]searchResult, error) {
	limit := 5
	if p.MaxResults != nil {
		limit = *p.MaxResults
	}
	q := strings.TrimSpace(p.Query)
	if len(q) == 0 || len(p.Query) > 512 || limit < 1 || limit > 10 {
		return nil, errors.New("query must contain 1-512 bytes and max_results must be between 1 and 10")
	}
	key := searchParamsKey{q, limit}
	r.mu.Lock()
	if err := r.ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	flight := r.flights[key]
	if flight == nil {
		shared, cancel := context.WithCancel(r.ctx)
		flight = &searchFlight{started: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
		r.flights[key] = flight
		r.workers.Add(1)
		select {
		case r.slots <- struct{}{}:
			close(flight.started)
			go r.runSearch(shared, key, flight, true)
		default:
			go r.runSearch(shared, key, flight, false)
		}
	}
	flight.waiters++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		flight.waiters--
		if flight.waiters == 0 {
			flight.cancel()
			if r.flights[key] == flight {
				delete(r.flights, key)
			}
		}
	}()
	detail := bounded(q, 160)
	select {
	case <-flight.started:
	default:
		ReportActivity(ctx, "waiting_for_slot", detail)
		select {
		case <-flight.started:
		case <-flight.done:
			return flight.results, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ReportActivity(ctx, "tool_running", detail)
	select {
	case <-flight.done:
		return flight.results, flight.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Registry) runSearch(shared context.Context, key searchParamsKey, flight *searchFlight, hasSlot bool) {
	defer r.workers.Done()
	defer flight.cancel()
	if !hasSlot {
		select {
		case r.slots <- struct{}{}:
			hasSlot = true
			close(flight.started)
		case <-shared.Done():
			flight.err = shared.Err()
		}
	}
	if hasSlot {
		ctx, cancel := context.WithTimeout(shared, 60*time.Second)
		flight.results, flight.err = r.lookup(ctx, key)
		cancel()
		<-r.slots
	}
	r.mu.Lock()
	if r.flights[key] == flight {
		delete(r.flights, key)
	}
	close(flight.done)
	r.mu.Unlock()
}

func (r *Registry) lookup(ctx context.Context, key searchParamsKey) ([]searchResult, error) {
	primary, cancel := context.WithTimeout(ctx, 40*time.Second)
	results, err := r.searx(primary, key)
	cancel()
	if err == nil || ctx.Err() != nil || r.diffbotKey == "" {
		return results, err
	}
	r.mu.Lock()
	cooling := time.Now().Before(r.diffbotAfter)
	r.mu.Unlock()
	if cooling {
		return nil, errors.New("search engines unavailable; fallback rate limited")
	}
	addr := "https://llm.diffbot.com/api/v1/web_search?" + url.Values{"text": {key.query}, "size": {strconv.Itoa(key.limit)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", addr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.diffbotKey)
	cl := *r.client
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var payload struct {
		Results *[]struct {
			Title   string
			PageURL string
			Content string
		} `json:"search_results"`
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, errors.New("search fallback unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		until := retryAfter(resp.Header.Get("Retry-After"))
		if until.IsZero() {
			until = time.Now().Add(time.Minute)
		}
		r.mu.Lock()
		if until.After(r.diffbotAfter) {
			r.diffbotAfter = until
		}
		r.mu.Unlock()
	}
	if err = decodeSearch(resp, &payload); err != nil {
		return nil, fmt.Errorf("search fallback: %w", err)
	}
	if payload.Results == nil {
		return nil, errors.New("malformed search fallback response")
	}
	out := []searchResult{}
	for _, item := range *payload.Results {
		out = appendResult(out, item.Title, item.PageURL, item.Content, key.limit)
	}
	return out, nil
}

func (r *Registry) searx(ctx context.Context, key searchParamsKey) ([]searchResult, error) {
	cl := r.searchClient
	if cl == nil {
		if r.runtime == nil {
			return nil, errors.New("private search service is not running")
		}
		var err error
		cl, err = r.runtime.Client()
		if err != nil {
			return nil, err
		}
	}
	addr := r.searchEndpoint + "?" + url.Values{"q": {key.query}, "format": {"json"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", addr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Real-IP", "127.0.0.1")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("private search service: %w", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Results *[]struct {
			Title   string
			URL     string
			Content string
		} `json:"results"`
		Failed [][2]string `json:"unresponsive_engines"`
	}
	if err = decodeSearch(resp, &payload); err != nil {
		return nil, err
	}
	if payload.Results == nil {
		return nil, errors.New("malformed private search response")
	}
	out := []searchResult{}
	for _, item := range *payload.Results {
		out = appendResult(out, item.Title, item.URL, item.Content, key.limit)
	}
	if len(out) == 0 && len(payload.Failed) > 0 {
		var failures []string
		for _, engine := range payload.Failed {
			failures = append(failures, bounded(engine[0], 48)+": "+bounded(engine[1], 96))
		}
		return nil, fmt.Errorf("no usable search results (%s)", strings.Join(failures, "; "))
	}
	return out, nil
}
func decodeSearch(resp *http.Response, target any) error {
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, maxInputBytes+1))
	if e != nil {
		return e
	}
	if len(b) > maxInputBytes {
		return errors.New("search response too large")
	}
	return json.Unmarshal(b, target)
}
func appendResult(out []searchResult, title, raw, snippet string, limit int) []searchResult {
	if len(out) >= limit {
		return out
	}
	u, err := publichttp.ParseURL(raw)
	if err != nil || !publichttp.PublicHost(u.Hostname()) {
		return out
	}
	return append(out, searchResult{bounded(title, 512), u.String(), bounded(snippet, 2048)})
}
func retryAfter(value string) time.Time {
	if n, err := strconv.ParseUint(value, 10, 31); err == nil {
		return time.Now().Add(time.Duration(n) * time.Second)
	}
	t, _ := http.ParseTime(value)
	return t
}
func bounded(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
