package embeds

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/example-git/haunt-server/internal/testutil"
)

type postTransport func(*http.Request) (*http.Response, error)

func (f postTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func postResponse(r *http.Request, status int, kind, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r, ContentLength: int64(len(body))}
}
func TestSocialPostsAndCanonicalCoalescing(t *testing.T) {
	s := New()
	defer s.Close()
	var calls atomic.Int32
	s.client.Transport = postTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "api.fxtwitter.com" || r.URL.Path != "/status/12345" {
			t.Errorf("unexpected request %s", r.URL)
		}
		return postResponse(r, 200, "application/json", `{"tweet":{"text":"First line\n\nSecond line <script>literal</script>","created_timestamp":1720000000,"author":{"name":"Author","screen_name":"person","avatar_url":"https://pbs.twimg.com/avatar.jpg"},"media":{"all":[{"type":"photo","url":"https://pbs.twimg.com/one.jpg","alt_text":"First image"},{"type":"photo","url":"https://pbs.twimg.com/two.jpg"}],"videos":[{"url":"https://video.twimg.com/movie.mp4","thumbnail_url":"https://pbs.twimg.com/poster.jpg"}]}}}`), nil
	})
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			raw := "https://x.com/person/status/12345?s=20"
			if i%2 == 0 {
				raw = "https://fixupx.com/person/status/12345/photo/1"
			}
			p, err := s.Page(context.Background(), raw)
			if err != nil || p.URL != raw || p.Provider != "x" || len(p.Media) != 3 || p.Media[2].Kind != "video" || p.Description != "First line\n\nSecond line <script>literal</script>" || !p.Avatar {
				t.Errorf("post: %+v %v", p, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("shared post made %d calls", calls.Load())
	}
	for _, raw := range []string{"https://x.com.evil.com/person/status/12345", "https://x.com/person/status/nope", "https://bsky.app/profile/author"} {
		u, _ := ParseURL(raw)
		_, parts := postIdentity(u)
		if parts != nil {
			t.Fatalf("misclassified %s", raw)
		}
	}
}
func TestXHTMLFallbackAndBlueskyImages(t *testing.T) {
	s := New()
	defer s.Close()
	s.client.Transport = postTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host + r.URL.Path {
		case "api.fxtwitter.com/status/54321":
			return postResponse(r, 503, "text/plain", "unavailable"), nil
		case "fxtwitter.com/i/status/54321":
			response := postResponse(r, 302, "text/html", `<head><meta property="og:title" content="Display (@handle)"><meta property="og:description" content="Post body"><meta property="og:video" content="https://video.twimg.com/fallback.mp4"><meta property="og:image" content="https://pbs.twimg.com/poster.jpg"></head>`)
			response.Header.Set("Location", "https://x.com/handle/status/54321")
			return response, nil
		case "public.api.bsky.app/xrpc/com.atproto.identity.resolveHandle":
			return postResponse(r, 200, "application/json", `{"did":"did:plc:abc"}`), nil
		case "public.api.bsky.app/xrpc/app.bsky.feed.getPosts":
			if r.URL.Query().Get("uris") != "at://did:plc:abc/app.bsky.feed.post/3key" {
				t.Error(r.URL)
			}
			return postResponse(r, 200, "application/json", `{"posts":[{"record":{"text":"Sky\npost","createdAt":"2026-10-26T12:00:00Z"},"author":{"displayName":"Sky Author","handle":"author.bsky.social","avatar":"https://cdn.bsky.app/avatar.jpg"},"embed":{"media":{"images":[{"fullsize":"https://cdn.bsky.app/full.jpg","alt":"Image description","aspectRatio":{"width":800,"height":600}},{"thumb":"https://cdn.bsky.app/thumb.jpg"}]}}}]}`), nil
		}
		t.Errorf("unexpected %s", r.URL)
		return postResponse(r, 404, "text/plain", ""), nil
	})
	p, err := s.Page(context.Background(), "https://twitter.com/handle/status/54321")
	if err != nil || p.Author != "Display" || p.AuthorHandle != "@handle" || len(p.Media) != 1 || p.Media[0].Kind != "video" || !p.Media[0].Poster {
		t.Fatal(p, err)
	}
	for _, author := range []string{"author.bsky.social", "did:plc:abc"} {
		p, err = s.Page(context.Background(), "https://bsky.app/profile/"+author+"/post/3key")
		if err != nil || p.Description != "Sky\npost" || len(p.Media) != 2 || p.Media[0].Alt != "Image description" || p.Media[0].Width != 800 || p.Provider != "bluesky" {
			t.Fatal(p, err)
		}
	}
	data, _ := json.Marshal(p)
	if bytes.Contains(data, []byte("cdn.bsky.app")) {
		t.Fatal("upstream media URL exposed", string(data))
	}
}
func TestPreviewCacheSurvivesResolverRestart(t *testing.T) {
	pool, _ := testutil.Database(t)
	if err := postgres.Migrate(context.Background(), pool, "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	s := NewPersistent(pool)
	s.client.Transport = postTransport(func(r *http.Request) (*http.Response, error) {
		return postResponse(r, 200, "application/json", `{"tweet":{"text":"cached post","author":{"name":"Author","screen_name":"person","avatar_url":"https://pbs.twimg.com/avatar.jpg"},"media":{"all":[{"type":"photo","url":"https://pbs.twimg.com/image.jpg"}]}}}`), nil
	})
	if _, err := s.Page(context.Background(), "https://x.com/person/status/123"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = NewPersistent(pool)
	defer s.Close()
	s.client.Transport = postTransport(func(*http.Request) (*http.Response, error) {
		t.Error("refetched persisted post")
		return nil, ErrUnavailable
	})
	p, err := s.Page(context.Background(), "https://fxtwitter.com/person/status/123")
	if err != nil || p.Description != "cached post" || p.AvatarURL == "" || len(p.Media) != 1 || p.Media[0].URL == "" {
		t.Fatal(p, err)
	}
}
func TestVideoRangeValidation(t *testing.T) {
	s := New()
	defer s.Close()
	s.mediaClient.Transport = postTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Range") != "bytes=0-8388607" {
			t.Error(r.Header)
		}
		response := postResponse(r, 206, "video/mp4", "test")
		response.Header.Set("Content-Range", "bytes 0-3/4")
		response.Header.Set("Content-Length", "4")
		return response, nil
	})
	response, err := s.Video(context.Background(), "https://video.twimg.com/video.mp4", "bytes=0-")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	for _, value := range []string{"bytes=0-1,3-4", "bytes=5-1", "bytes=-0", "invalid"} {
		if _, err := s.Video(context.Background(), "https://video.twimg.com/video.mp4", value); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}

func TestLiveSocialProviders(t *testing.T) {
	if os.Getenv("HAUNT_LIVE_EMBEDS") != "1" {
		t.Skip("set HAUNT_LIVE_EMBEDS=1 for bounded public provider checks")
	}
	s := New()
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	urls := []string{"https://x.com/i/status/1570176456059629570"}
	if raw := os.Getenv("HAUNT_LIVE_X_POST"); raw != "" {
		urls[0] = raw
	}
	var feed struct {
		Feed []struct {
			Post struct {
				URI   string `json:"uri"`
				Embed struct {
					Images []json.RawMessage `json:"images"`
					Media  struct {
						Images []json.RawMessage `json:"images"`
					} `json:"media"`
				} `json:"embed"`
				Author struct {
					Handle string `json:"handle"`
				} `json:"author"`
			} `json:"post"`
		} `json:"feed"`
	}
	if err := s.json(ctx, "https://public.api.bsky.app/xrpc/app.bsky.feed.getAuthorFeed?actor=bsky.app&filter=posts_with_media&limit=10", &feed); err != nil {
		t.Errorf("Bluesky feed: %v", err)
	} else {
		found := false
		for _, item := range feed.Feed {
			post := item.Post
			if len(post.Embed.Images) == 0 && len(post.Embed.Media.Images) == 0 {
				continue
			}
			parts := strings.Split(post.URI, "/")
			urls = append(urls, "https://bsky.app/profile/"+post.Author.Handle+"/post/"+parts[len(parts)-1])
			found = true
			break
		}
		if !found {
			t.Error("No Bluesky image sample in the bounded feed")
		}
	}
	var mediaFeed struct {
		Results []struct {
			URL   string `json:"url"`
			Media struct {
				All []struct {
					Type string `json:"type"`
				} `json:"all"`
			} `json:"media"`
		} `json:"results"`
	}
	if err := s.json(ctx, "https://api.fxtwitter.com/2/profile/NASA/media?count=5", &mediaFeed); err != nil {
		t.Logf("X sample discovery unavailable: %v", err)
	} else {
		images, videos := false, false
		for _, item := range mediaFeed.Results {
			for _, m := range item.Media.All {
				if m.Type == "photo" && !images {
					urls = append(urls, item.URL)
					images = true
					break
				}
				if m.Type == "video" && !videos {
					urls = append(urls, item.URL)
					videos = true
					break
				}
			}
			if images && videos {
				break
			}
		}
	}

	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			p, err := s.Page(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("provider=%s author=%s media=%d text_chars=%d url=%s", p.Provider, p.Author, len(p.Media), len(p.Description), p.URL)
			if p.Description == "" && len(p.Media) == 0 {
				t.Fatal("empty post")
			}
			for _, m := range p.Media {
				if m.Kind == "image" {
					asset, err := s.Image(ctx, m.URL)
					if err != nil {
						t.Fatal("image", err)
					}
					t.Logf("image type=%s bytes=%d", asset.Type, len(asset.Data))
				} else {
					response, err := s.Video(ctx, m.URL, "bytes=0-65535")
					if err != nil {
						t.Fatal("video", err)
					}
					data, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil || len(data) == 0 {
						t.Fatal(err)
					}
					t.Logf("video status=%d type=%s range=%s bytes=%d", response.StatusCode, response.Header.Get("Content-Type"), response.Header.Get("Content-Range"), len(data))
				}
			}
		})
	}
}
