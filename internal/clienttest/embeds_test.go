package integration

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/example-git/haunt-server/internal/embeds"
)

// Deterministic provider data for browser integration; the real network policy,
// metadata parser, redirects and image validation are tested in internal/embeds.
// This resolver exists only in the test binary, with no production bypass flag.
type fixturePreviews struct{}

func (fixturePreviews) Close() {}
func (fixturePreviews) Page(_ context.Context, url string) (embeds.Preview, error) {
	if id := embeds.YouTube(url); id != "" {
		return embeds.Preview{URL: url, Title: "Fixture video", Kind: "video", YouTubeID: id}, nil
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
