// Package embeds fetches bounded public metadata independently of messaging and
// the gateway. It never executes provider HTML, forwards credentials or stores
// previews in the retained message/event budget.
package embeds

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
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
	Provider     string  `json:"provider,omitempty"`
	AuthorHandle string  `json:"author_handle,omitempty"`
	Avatar       bool    `json:"avatar,omitempty"`
	AvatarURL    string  `json:"-"`
	Media        []Media `json:"media,omitempty"`
	Status       string  `json:"status,omitempty"`
	Index        int     `json:"index"`
	URL          string  `json:"url"`
	Title        string  `json:"title,omitempty"`
	Description  string  `json:"description,omitempty"`
	SiteName     string  `json:"site_name,omitempty"`
	Kind         string  `json:"kind"`
	Image        bool    `json:"image"`
	ImageAlt     string  `json:"image_alt,omitempty"`
	ImageKey     string  `json:"image_key,omitempty"`
	AvatarKey    string  `json:"avatar_key,omitempty"`
	YouTubeID    string  `json:"youtube_id,omitempty"`
	Player       string  `json:"player,omitempty" enum:"vimeo,streamable,twitch-video,twitch-channel,twitch-clip"`
	PlayerID     string  `json:"player_id,omitempty"`
	PlayerToken  string  `json:"player_token,omitempty"`
	Author       string  `json:"author_name,omitempty"`
	PublishedAt  string  `json:"published_at,omitempty"`
	ThemeColor   string  `json:"theme_color,omitempty"`
	ImageURL     string  `json:"-"`
	Asset        *Asset  `json:"-"`
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
	client      *http.Client
	mediaClient *http.Client
	fetchSlots  [2]chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	items       map[string]*list.Element
	order       *list.List
	running     map[string]*flight
	bytes       int
	wg          sync.WaitGroup
	closed      bool
	cache       *pageCache
}

func New() *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{client: safeClient(), mediaClient: safeClient(), fetchSlots: [2]chan struct{}{make(chan struct{}, 8), make(chan struct{}, 8)}, ctx: ctx, cancel: cancel, items: map[string]*list.Element{}, order: list.New(), running: map[string]*flight{}}
}
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
	s.client.CloseIdleConnections()
	s.mediaClient.CloseIdleConnections()
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
		kind := strings.HasPrefix(key, "image:")
		pending := 0
		for running := range s.running {
			if strings.HasPrefix(running, "image:") == kind {
				pending++
			}
		}
		if pending >= 40 {
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
			var value cached
			lane := 0
			if kind {
				lane = 1
			}
			queueTimer := time.NewTimer(time.Second)
			select {
			case s.fetchSlots[lane] <- struct{}{}:
				value = fetch(fetchCtx)
				<-s.fetchSlots[lane]
			case <-queueTimer.C:
				value.err = ErrBusy
			case <-fetchCtx.Done():
				value.err = fetchCtx.Err()
			}
			queueTimer.Stop()
			value.key = key
			ttl := 15 * time.Minute
			if value.err != nil {
				ttl = 30 * time.Second
			}
			value.expiry = time.Now().Add(ttl)
			pageData, _ := json.Marshal(value.preview)
			value.size = len(value.asset.Data) + len(value.preview.AssetData()) + len(pageData) + len(value.preview.ImageURL) + len(value.preview.AvatarURL) + 8192
			for _, media := range value.preview.Media {
				value.size += len(media.URL) + len(media.PosterURL)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.running, key)
			task.value = value
			if !s.closed && value.err != ErrBusy {
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
	key, post := postIdentity(u)
	value, err := s.load(ctx, "page:"+key, func(ctx context.Context) cached {
		if p, ok := s.cache.get(ctx, key); ok {
			return cached{preview: p}
		}
		var preview Preview
		var err error
		if strings.HasPrefix(key, "x:") && post != nil {
			preview, err = s.fetchX(ctx, u, post[0])
		} else if strings.HasPrefix(key, "bluesky:") && post != nil {
			preview, err = s.fetchBluesky(ctx, u, post[0], post[1])
		} else {
			preview, err = s.fetchPage(ctx, u)
		}
		if err == nil {
			preview.Status = "ready"
			s.cache.put(ctx, key, preview)
		}
		return cached{preview: preview, err: err}
	})
	preview := value.preview
	preview.URL = u.String()
	preview.ImageKey = assetKey(preview.ImageURL)
	preview.AvatarKey = assetKey(preview.AvatarURL)
	preview.Media = append([]Media(nil), preview.Media...)
	for i := range preview.Media {
		media := &preview.Media[i]
		if media.Kind == "image" {
			media.CacheKey = assetKey(media.URL)
		} else {
			media.CacheKey = assetKey(media.PosterURL)
		}
	}
	return preview, err
}
func assetKey(raw string) string {
	if raw == "" {
		return ""
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", time.Now().Unix()/900, raw)))
	return hex.EncodeToString(key[:])
}
func (s *Service) Image(ctx context.Context, raw string) (Asset, error) {
	u, err := ParseURL(raw)
	if err != nil {
		return Asset{}, err
	}
	value, err := s.load(ctx, "image:"+u.String(), func(ctx context.Context) cached {
		response, err := s.getMedia(ctx, u)
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
	return getWithClient(ctx, s.client, u)
}
func (s *Service) getMedia(ctx context.Context, u *url.URL) (*http.Response, error) {
	return getWithClient(ctx, s.mediaClient, u)
}
func getWithClient(ctx context.Context, client *http.Client, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("User-Agent", "Haunt/0.1 (+link-preview)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,image/png,image/jpeg,image/gif,image/webp;q=0.9")
	response, err := client.Do(req)
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
	p := playerPreview(u)
	response, err := s.get(ctx, u)
	if err != nil {
		if p.YouTubeID != "" || p.Player != "" {
			return p, nil
		}
		return p, err
	}
	defer response.Body.Close()
	kind, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaKind(kind) != "" || kind == "application/octet-stream" {
		header := make([]byte, 512)
		n, _ := io.ReadFull(response.Body, header)
		actual := mediaType(header[:n])
		if actual == "" {
			if strings.HasPrefix(http.DetectContentType(header[:n]), "image/") {
				asset, imageErr := readImage(io.MultiReader(bytes.NewReader(header[:n]), response.Body))
				if imageErr != nil {
					return p, imageErr
				}
				p.Kind, p.Image, p.ImageURL, p.Asset = "image", true, response.Request.URL.String(), &asset
				return p, nil
			}
			return p, ErrUnavailable
		}
		p.Kind = mediaKind(actual)
		p.Title = clean(path.Base(response.Request.URL.Path), 240)
		p.SiteName = response.Request.URL.Hostname()
		p.addMedia(p.Kind, response.Request.URL.String(), "", "", 0, 0)
		return p, nil
	}
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
		if p.YouTubeID != "" || p.Player != "" {
			return p, nil
		}
		if kind == "application/pdf" || kind == "text/plain" {
			p.Kind, p.Title, p.Description = "file", clean(path.Base(response.Request.URL.Path), 240), kind
			return p, nil
		}
		return p, ErrUnavailable
	}
	meta, title, err := readMetadata(response.Body)
	if err != nil {
		if p.YouTubeID != "" || p.Player != "" {
			return p, nil
		}
		return p, ErrUnavailable
	}
	p.Title = first(meta["og:title"], meta["twitter:title"], title, p.Title)
	p.Description = first(meta["og:description"], meta["twitter:description"], meta["description"])
	p.SiteName = first(meta["og:site_name"], p.SiteName)
	p.ImageAlt = clean(meta["og:image:alt"], 160)
	p.Author = clean(first(meta["article:author"], meta["author"]), 120)
	p.PublishedAt = published(first(meta["article:published_time"], meta["date"], meta["datepublished"]))
	p.ThemeColor = themeColor(meta["theme-color"])
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
	values, title, _ := readMetadata(bytes.NewReader(data))
	return values, title
}
func readMetadata(reader io.Reader) (map[string]string, string, error) {
	values := map[string]string{}
	title := ""
	inTitle := false
	limited := &io.LimitedReader{R: reader, N: maxHTML + 1}
	tokens := html.NewTokenizer(limited)
	tokens.SetMaxBuf(maxHTML)
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			if limited.N == 0 || tokens.Err() != io.EOF {
				return values, title, ErrUnavailable
			}
			break
		}
		token := tokens.Token()
		switch kind {
		case html.StartTagToken, html.SelfClosingTagToken:
			if token.Data == "body" {
				return values, clean(title, 240), nil
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
				case "og:title", "twitter:title", "og:description", "twitter:description", "description", "og:site_name", "og:image:alt", "og:image:secure_url", "og:image", "og:image:url", "twitter:image", "twitter:image:src", "article:author", "author", "article:published_time", "date", "datepublished", "theme-color", "og:video", "og:video:url", "og:video:secure_url", "twitter:player:stream":
					if values[key] == "" {
						if key == "og:description" || key == "twitter:description" {
							values[key] = postText(value)
						} else {
							values[key] = clean(value, 2048)
						}
					}
				}
			}
		case html.EndTagToken:
			if token.Data == "title" {
				inTitle = false
			}
			if token.Data == "head" {
				return values, clean(title, 240), nil
			}
		case html.TextToken:
			if inTitle {
				title += token.Data
			}
		}
	}
	return values, clean(title, 240), nil
}
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func published(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, time.RubyDate, time.RFC1123Z, "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return ""
}
func themeColor(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 4 && len(value) != 7 {
		return ""
	}
	if value[0] != '#' {
		return ""
	}
	for _, r := range value[1:] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return ""
		}
	}
	return value
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
