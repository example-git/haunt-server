package embeds

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Media struct {
	CacheKey  string `json:"cache_key,omitempty"`
	Kind      string `json:"kind"`
	Alt       string `json:"alt,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Poster    bool   `json:"poster,omitempty"`
	URL       string `json:"-"`
	PosterURL string `json:"-"`
}

var xPath = regexp.MustCompile(`^/(?:[A-Za-z0-9_]+/status|i/status|i/web/status)/([0-9]{1,25})(?:/(?:photo|video)/[0-9]+)?/?$`)
var blueskyPath = regexp.MustCompile(`^/profile/([^/]+)/post/([A-Za-z0-9]+)/*$`)
var authorTitle = regexp.MustCompile(`^(.+?)\s*\(@(\w+)\)$`)

func postIdentity(u *url.URL) (string, []string) {
	switch strings.TrimPrefix(u.Hostname(), "www.") {
	case "x.com", "twitter.com", "mobile.twitter.com", "mobile.x.com", "fxtwitter.com", "fixupx.com", "vxtwitter.com":
		if match := xPath.FindStringSubmatch(u.Path); match != nil {
			return "x:" + match[1], match[1:]
		}
	case "bsky.app":
		if match := blueskyPath.FindStringSubmatch(u.Path); match != nil {
			return "bluesky:" + match[1] + ":" + match[2], match[1:]
		}
	}
	return u.String(), nil
}
func mediaURL(raw string) string {
	if u, err := ParseURL(raw); err == nil {
		return u.String()
	}
	return ""
}
func (p *Preview) addMedia(kind, raw, poster, alt string, width, height int) {
	raw = mediaURL(raw)
	if raw == "" || len(p.Media) >= 4 {
		return
	}
	for _, item := range p.Media {
		if item.URL == raw {
			return
		}
	}
	poster = mediaURL(poster)
	p.Media = append(p.Media, Media{Kind: kind, URL: raw, PosterURL: poster, Poster: poster != "", Alt: clean(alt, 1000), Width: max(0, min(width, 8192)), Height: max(0, min(height, 8192))})
	if p.ImageURL == "" {
		if kind == "image" {
			p.ImageURL = raw
			p.Image = true
		} else if poster != "" {
			p.ImageURL = poster
			p.Image = true
		}
	}
}
func (s *Service) json(ctx context.Context, raw string, target any) error {
	u, err := ParseURL(raw)
	if err != nil {
		return err
	}
	response, err := s.get(ctx, u)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxHTML+1))
	if err != nil || len(data) > maxHTML {
		return ErrUnavailable
	}
	if json.Unmarshal(data, target) != nil {
		return ErrUnavailable
	}
	return nil
}
func (s *Service) fetchX(ctx context.Context, u *url.URL, id string) (Preview, error) {
	p := Preview{URL: u.String(), Kind: "post", SiteName: "X", Provider: "x"}
	var data struct {
		Tweet *struct {
			Text      string `json:"text"`
			Created   string `json:"created_at"`
			Timestamp int64  `json:"created_timestamp"`
			Author    struct {
				Name   string `json:"name"`
				Handle string `json:"screen_name"`
				Avatar string `json:"avatar_url"`
			} `json:"author"`
			Media struct {
				All []struct {
					Type      string `json:"type"`
					URL       string `json:"url"`
					Alt       string `json:"alt_text"`
					Width     int    `json:"width"`
					Height    int    `json:"height"`
					Thumbnail string `json:"thumbnail_url"`
				} `json:"all"`
				Videos []struct {
					URL       string `json:"url"`
					Thumbnail string `json:"thumbnail_url"`
					Width     int    `json:"width"`
					Height    int    `json:"height"`
				} `json:"videos"`
				External struct {
					Type string `json:"type"`
					URL  string `json:"url"`
				} `json:"external"`
			} `json:"media"`
		} `json:"tweet"`
	}
	apiCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := s.json(apiCtx, "https://api.fxtwitter.com/status/"+id, &data)
	cancel()
	if err == nil && data.Tweet != nil {
		tweet := data.Tweet
		p.Description = postText(tweet.Text)
		p.Author = clean(tweet.Author.Name, 120)
		p.AuthorHandle = "@" + clean(tweet.Author.Handle, 100)
		p.AvatarURL = mediaURL(tweet.Author.Avatar)
		p.Avatar = p.AvatarURL != ""
		p.PublishedAt = published(tweet.Created)
		if p.PublishedAt == "" && tweet.Timestamp > 0 {
			p.PublishedAt = time.Unix(tweet.Timestamp, 0).UTC().Format(time.RFC3339)
		}
		for _, item := range tweet.Media.All {
			if item.Type == "photo" {
				p.addMedia("image", item.URL, "", item.Alt, item.Width, item.Height)
			} else if item.Type == "video" || item.Type == "gif" {
				p.addMedia("video", item.URL, item.Thumbnail, item.Alt, item.Width, item.Height)
			}
		}
		for _, item := range tweet.Media.Videos {
			p.addMedia("video", item.URL, item.Thumbnail, "", item.Width, item.Height)
		}
		if len(tweet.Media.Videos) == 0 && tweet.Media.External.Type == "video" {
			p.addMedia("video", tweet.Media.External.URL, "", "", 0, 0)
		}
		return p, nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://fxtwitter.com/i/status/"+id, nil)
	if err != nil {
		return p, ErrUnavailable
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)")
	req.Header.Set("Accept", "text/html")
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return p, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != 200 && response.StatusCode != 302 {
		return p, ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHTML+1))
	if err != nil || len(body) > maxHTML {
		return p, ErrUnavailable
	}
	meta, _ := metadata(body)
	p.Author = clean(meta["og:title"], 120)
	p.Description = postText(meta["og:description"])
	if p.Author == "" && p.Description == "" {
		return p, ErrUnavailable
	}
	if match := authorTitle.FindStringSubmatch(p.Author); match != nil {
		p.Author = match[1]
		p.AuthorHandle = "@" + match[2]
	}
	video := first(meta["og:video:secure_url"], meta["og:video:url"], meta["og:video"], meta["twitter:player:stream"])
	if video != "" {
		p.addMedia("video", video, meta["og:image"], "", 0, 0)
	} else {
		p.addMedia("image", meta["og:image"], "", "", 0, 0)
	}
	return p, nil
}
func (s *Service) fetchBluesky(ctx context.Context, u *url.URL, handle, key string) (Preview, error) {
	p := Preview{URL: u.String(), Kind: "post", SiteName: "Bluesky", Provider: "bluesky"}
	did := handle
	if !strings.HasPrefix(did, "did:") {
		var resolved struct {
			DID string `json:"did"`
		}
		if err := s.json(ctx, "https://public.api.bsky.app/xrpc/com.atproto.identity.resolveHandle?handle="+url.QueryEscape(handle), &resolved); err != nil {
			return p, err
		}
		did = resolved.DID
	}
	if !strings.HasPrefix(did, "did:") || strings.ContainsAny(did, "/?#") {
		return p, ErrUnavailable
	}
	type picture struct {
		Full   string `json:"fullsize"`
		Thumb  string `json:"thumb"`
		Alt    string `json:"alt"`
		Aspect struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"aspectRatio"`
	}
	var data struct {
		Posts []struct {
			Record struct {
				Text    string `json:"text"`
				Created string `json:"createdAt"`
			} `json:"record"`
			Author struct {
				Name   string `json:"displayName"`
				Handle string `json:"handle"`
				Avatar string `json:"avatar"`
			} `json:"author"`
			Embed struct {
				Images []picture `json:"images"`
				Media  struct {
					Images []picture `json:"images"`
				} `json:"media"`
			} `json:"embed"`
		} `json:"posts"`
	}
	if err := s.json(ctx, "https://public.api.bsky.app/xrpc/app.bsky.feed.getPosts?uris="+url.QueryEscape("at://"+did+"/app.bsky.feed.post/"+key), &data); err != nil {
		return p, err
	}
	if len(data.Posts) == 0 {
		return p, ErrUnavailable
	}
	post := data.Posts[0]
	p.Description = postText(post.Record.Text)
	p.Author = clean(first(post.Author.Name, post.Author.Handle), 120)
	p.AuthorHandle = "@" + clean(post.Author.Handle, 253)
	p.AvatarURL = mediaURL(post.Author.Avatar)
	p.Avatar = p.AvatarURL != ""
	p.PublishedAt = published(post.Record.Created)
	pictures := post.Embed.Images
	if len(pictures) == 0 {
		pictures = post.Embed.Media.Images
	}
	for _, item := range pictures {
		p.addMedia("image", first(item.Full, item.Thumb), "", item.Alt, item.Aspect.Width, item.Aspect.Height)
	}
	return p, nil
}

func postText(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = clean(line, 32000)
	}
	chars := []rune(strings.TrimSpace(strings.Join(lines, "\n")))
	if len(chars) > 32000 {
		chars = chars[:32000]
	}
	return string(chars)
}
