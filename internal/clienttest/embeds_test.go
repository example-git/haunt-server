package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/example-git/haunt-server/internal/embeds"
)

// Deterministic provider data for browser integration; the real network policy,
// metadata parser, redirects and image validation are tested in internal/embeds.
// This resolver exists only in the test binary, with no production bypass flag.
type fixturePreviews struct{}

func (fixturePreviews) Close() {}
func (fixturePreviews) Page(_ context.Context, url string) (embeds.Preview, error) {
	if id := embeds.YouTube(url); id != "" {
		return embeds.Preview{URL: url, Title: "Fixture video", SiteName: "YouTube", Kind: "video", YouTubeID: id}, nil
	}
	for _, provider := range []struct{ url, name, player, id string }{
		{"https://vimeo.com/76979871", "Vimeo", "vimeo", "76979871"},
		{"https://streamable.com/abc123", "Streamable", "streamable", "abc123"},
		{"https://twitch.tv/videos/123456", "Twitch", "twitch-video", "123456"},
		{"https://twitch.tv/hauntfixture", "Twitch", "twitch-channel", "hauntfixture"},
		{"https://clips.twitch.tv/FixtureClip", "Twitch", "twitch-clip", "FixtureClip"},
	} {
		if url == provider.url {
			return embeds.Preview{URL: url, Title: provider.name + " fixture", SiteName: provider.name, Kind: "video", Player: provider.player, PlayerID: provider.id}, nil
		}
	}
	if url == "https://preview.example.com/direct.mp4" || url == "https://preview.example.com/direct.wav" {
		kind := "video"
		if strings.HasSuffix(url, ".wav") {
			kind = "audio"
		}
		return embeds.Preview{URL: url, Title: "Direct " + kind, Kind: kind, Media: []embeds.Media{{Kind: kind, URL: url}}}, nil
	}
	if strings.HasPrefix(url, "https://x.com/") || strings.HasPrefix(url, "https://bsky.app/") {
		provider, name := "x", "X"
		if strings.HasPrefix(url, "https://bsky.app/") {
			provider, name = "bluesky", "Bluesky"
		}
		media := []embeds.Media{{Kind: "image", URL: "https://preview.example.com/one", Alt: "First post image"}, {Kind: "image", URL: "https://preview.example.com/two", Alt: "Second post image"}}
		if provider == "x" {
			media = append(media, embeds.Media{Kind: "video", URL: "https://preview.example.com/video", Poster: true, PosterURL: "https://preview.example.com/poster"})
		}
		return embeds.Preview{URL: url, Kind: "post", Provider: provider, SiteName: name, Author: "Post author", AuthorHandle: "@author", Avatar: true, AvatarURL: "https://preview.example.com/avatar", Description: "First paragraph.\n\nSecond paragraph.", Media: media, Status: "ready"}, nil
	}
	if !strings.HasPrefix(url, "https://preview.example.com/") {
		return embeds.Preview{}, embeds.ErrUnavailable
	}
	return embeds.Preview{URL: url, Title: "A rich preview <script>plain text</script>", Description: "A useful description from the linked page.", SiteName: "Example journal", Kind: "article", Image: true, ImageURL: url + "/image", ImageAlt: "Blue preview"}, nil
}
func (fixturePreviews) Image(_ context.Context, url string) (embeds.Asset, error) {
	if strings.Contains(url, "missing-image") {
		return embeds.Asset{}, embeds.ErrUnavailable
	}
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==")
	return embeds.Asset{Data: data, Type: "image/png"}, nil
}

var videoFixture []byte
var audioFixture []byte
var videoFixtureErr error
var videoFixtureOnce sync.Once

func (fixturePreviews) Video(ctx context.Context, raw, byteRange string) (*http.Response, error) {
	videoFixtureOnce.Do(func() {
		videoFixture, videoFixtureErr = exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=12", "-t", "4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "frag_keyframe+empty_moov", "-f", "mp4", "pipe:1").Output()
		if videoFixtureErr == nil {
			audioFixture, videoFixtureErr = exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000", "-t", "2", "-f", "wav", "pipe:1").Output()
		}
	})
	if videoFixtureErr != nil {
		return nil, videoFixtureErr
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", raw, nil).WithContext(ctx)
	request.Header.Set("Range", byteRange)
	recorder.Header().Set("Content-Type", "video/mp4")
	if strings.HasSuffix(raw, ".wav") {
		recorder.Header().Set("Content-Type", "audio/wav")
		http.ServeContent(recorder, request, "audio.wav", time.Time{}, bytes.NewReader(audioFixture))
		return recorder.Result(), nil
	}
	http.ServeContent(recorder, request, "post.mp4", time.Time{}, bytes.NewReader(videoFixture))
	return recorder.Result(), nil
}
