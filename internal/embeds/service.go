// Package embeds fetches bounded public metadata independently of messaging and
// the gateway. It never executes provider HTML, forwards credentials or stores
// previews in the retained message/event budget.
package embeds

import (
	"bytes"
	"container/list"
	"context"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "golang.org/x/image/webp"
	"golang.org/x/net/html"
)

const maxImage = 4 << 20
const maxHTML = 1 << 20
const maxCache = 32 << 20

type Preview struct {
	Index       int    `json:"index"`
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	Kind        string `json:"kind"`
	Image       bool   `json:"image"`
	ImageAlt    string `json:"image_alt,omitempty"`
	YouTubeID   string `json:"youtube_id,omitempty"`
	ImageURL    string `json:"-"`
	Asset       *Asset `json:"-"`
}
type Asset struct {
	Data []byte
	Type string
}
type Resolver interface {
	Page(context.Context, string) (Preview, error)
	Image(context.Context, string) (Asset, error)
	Close()
}
type cached struct {
	key     string
	preview Preview
	asset   Asset
	err     error
	expiry  time.Time
	size    int
}
type flight struct {
	done  chan struct{}
	value cached
}
type Service struct {
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	items   map[string]*list.Element
	order   *list.List
	running map[string]*flight
	bytes   int
	wg      sync.WaitGroup
	closed  bool
}

func New() *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{client: safeClient(), ctx: ctx, cancel: cancel, items: map[string]*list.Element{}, order: list.New(), running: map[string]*flight{}}
}
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
	s.client.CloseIdleConnections()
	s.mu.Lock()
	s.items = map[string]*list.Element{}
	s.order.Init()
	s.bytes = 0
	s.mu.Unlock()
}
func (s *Service) load(ctx context.Context, key string, fetch func(context.Context) cached) (cached, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return cached{}, ErrUnavailable
	}
	if entry := s.items[key]; entry != nil {
		value := entry.Value.(cached)
		if time.Now().Before(value.expiry) {
			s.order.MoveToFront(entry)
			s.mu.Unlock()
			return value, value.err
		}
		s.bytes -= value.size
		s.order.Remove(entry)
		delete(s.items, key)
	}
	task := s.running[key]
	if task == nil {
		if len(s.running) >= 8 {
			s.mu.Unlock()
			return cached{}, ErrBusy
		}
		task = &flight{done: make(chan struct{})}
		s.running[key] = task
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			fetchCtx, cancel := context.WithTimeout(s.ctx, 6*time.Second)
			defer cancel()
			value := fetch(fetchCtx)
			value.key = key
			ttl := 15 * time.Minute
			if value.err != nil {
				ttl = 30 * time.Second
			}
			value.expiry = time.Now().Add(ttl)
			value.size = len(value.asset.Data) + len(value.preview.AssetData()) + 8192
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.running, key)
			task.value = value
			if !s.closed {
				s.items[key] = s.order.PushFront(value)
				s.bytes += value.size
				for s.order.Len() > 512 || s.bytes > maxCache {
					tail := s.order.Back()
					old := tail.Value.(cached)
					delete(s.items, old.key)
					s.bytes -= old.size
					s.order.Remove(tail)
				}
			}
			close(task.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return cached{}, ctx.Err()
	case <-task.done:
		return task.value, task.value.err
	}
}
func (p Preview) AssetData() []byte {
	if p.Asset != nil {
		return p.Asset.Data
	}
	return nil
}
func (s *Service) Page(ctx context.Context, raw string) (Preview, error) {
	u, err := ParseURL(raw)
	if err != nil {
		return Preview{}, err
	}
	value, err := s.load(ctx, "page:"+u.String(), func(ctx context.Context) cached {
		preview, err := s.fetchPage(ctx, u)
		return cached{preview: preview, err: err}
	})
	return value.preview, err
}
func (s *Service) Image(ctx context.Context, raw string) (Asset, error) {
	u, err := ParseURL(raw)
	if err != nil {
		return Asset{}, err
	}
	value, err := s.load(ctx, "image:"+u.String(), func(ctx context.Context) cached {
		response, err := s.get(ctx, u)
		if err != nil {
			return cached{err: err}
		}
		defer response.Body.Close()
		asset, err := readImage(response.Body)
		return cached{asset: asset, err: err}
	})
	return value.asset, err
}
func (s *Service) get(ctx context.Context, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("User-Agent", "Haunt/0.1 (+link-preview)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,image/png,image/jpeg,image/gif,image/webp;q=0.9")
	response, err := s.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, ErrUnavailable
	}
	return response, nil
}
func (s *Service) fetchPage(ctx context.Context, u *url.URL) (Preview, error) {
	p := Preview{URL: u.String(), Kind: "link", YouTubeID: YouTube(u.String())}
	if p.YouTubeID != "" {
		p.Kind = "video"
		p.SiteName = "YouTube"
		p.Title = "YouTube video"
	}
	response, err := s.get(ctx, u)
	if err != nil {
		if p.YouTubeID != "" {
			return p, nil
		}
		return p, err
	}
	defer response.Body.Close()
	kind, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if strings.HasPrefix(kind, "image/") {
		asset, err := readImage(response.Body)
		if err != nil {
			return p, err
		}
		p.Kind = "image"
		p.Image = true
		p.ImageURL = response.Request.URL.String()
		p.Asset = &asset
		return p, nil
	}
	if kind != "text/html" && kind != "application/xhtml+xml" {
		if p.YouTubeID != "" {
			return p, nil
		}
		return p, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxHTML+1))
	if err != nil || len(data) > maxHTML {
		if p.YouTubeID != "" {
			return p, nil
		}
		return p, ErrUnavailable
	}
	meta, title := metadata(data)
	p.Title = first(meta["og:title"], meta["twitter:title"], title, p.Title)
	p.Description = first(meta["og:description"], meta["twitter:description"], meta["description"])
	p.SiteName = first(meta["og:site_name"], p.SiteName)
	p.ImageAlt = clean(meta["og:image:alt"], 160)
	if raw := first(meta["og:image:secure_url"], meta["og:image"], meta["og:image:url"], meta["twitter:image"], meta["twitter:image:src"]); raw != "" {
		if image, err := response.Request.URL.Parse(raw); err == nil {
			if valid, err := ParseURL(image.String()); err == nil {
				p.ImageURL = valid.String()
				p.Image = true
			}
		}
	}
	p.Title = clean(p.Title, 240)
	p.Description = clean(p.Description, 600)
	p.SiteName = clean(p.SiteName, 80)
	return p, nil
}
func readImage(r io.Reader) (Asset, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxImage+1))
	if err != nil || len(data) > maxImage {
		return Asset{}, ErrUnavailable
	}
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
		return Asset{}, ErrUnavailable
	}
	types := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}
	if types[kind] == "" {
		return Asset{}, ErrUnavailable
	}
	return Asset{Data: data, Type: types[kind]}, nil
}
func metadata(data []byte) (map[string]string, string) {
	values := map[string]string{}
	title := ""
	inTitle := false
	tokens := html.NewTokenizer(bytes.NewReader(data))
	tokens.SetMaxBuf(maxHTML)
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			break
		}
		token := tokens.Token()
		switch kind {
		case html.StartTagToken, html.SelfClosingTagToken:
			if token.Data == "body" {
				return values, title
			}
			if token.Data == "title" {
				inTitle = true
			}
			if token.Data == "meta" {
				key, value := "", ""
				for _, attr := range token.Attr {
					if attr.Key == "property" || attr.Key == "name" {
						key = strings.ToLower(attr.Val)
					}
					if attr.Key == "content" {
						value = attr.Val
					}
				}
				switch key {
				case "og:title", "twitter:title", "og:description", "twitter:description", "description", "og:site_name", "og:image:alt", "og:image:secure_url", "og:image", "og:image:url", "twitter:image", "twitter:image:src":
					if values[key] == "" {
						values[key] = clean(value, 2048)
					}
				}
			}
		case html.EndTagToken:
			if token.Data == "title" {
				inTitle = false
			}
			if token.Data == "head" {
				return values, title
			}
		case html.TextToken:
			if inTitle {
				title += token.Data
			}
		}
	}
	return values, clean(title, 240)
}
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func clean(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 {
			return -1
		}
		return r
	}, s)
	chars := []rune(strings.Join(strings.Fields(s), " "))
	if len(chars) > limit {
		chars = chars[:limit]
	}
	return string(chars)
}
