package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/ai/clientidentity"
	"github.com/example-git/haunt-server/internal/ai/provider"
)

type imageTransport func(*http.Request) (*http.Response, error)

func (fn imageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestImageAccountAndEditWire(t *testing.T) {
	ids, err := clientidentity.New("")
	if err != nil {
		t.Fatal(err)
	}
	account := provider.Credentials{AccountID: "channel-account", AccessToken: "channel-token"}
	for _, edit := range []bool{false, true} {
		calls := 0
		c := New(ids, &http.Client{Transport: imageTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			want := imageBaseURL + "generations"
			if edit {
				want = imageBaseURL + "edits"
			}
			if r.URL.String() != want || r.Header.Get("Authorization") != "Bearer channel-token" || r.Header.Get("ChatGPT-Account-ID") != "channel-account" || r.Header.Get("originator") == "" {
				t.Fatal("image request identity or route changed")
			}
			var body struct {
				Model, Prompt string
				Images        []struct {
					URL string `json:"image_url"`
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Model != "gpt-image-2" || body.Prompt != "fixture" {
				t.Fatal("missing image request")
			}
			if edit && (len(body.Images) != 1 || body.Images[0].URL != "data:image/png;base64,c291cmNl") || !edit && len(body.Images) != 0 {
				t.Fatal("incorrect edit input")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"b64_json":"b3V0cHV0"}]}`)), Header: make(http.Header)}, nil
		})})
		var parts []provider.Part
		if edit {
			parts = []provider.Part{{Type: "image", MIMEType: "image/png", Data: []byte("source")}}
		}
		img, err := c.Image(context.Background(), account, "fixture", parts, 1024)
		if err != nil || string(img) != "output" || calls != 1 {
			t.Fatalf("image round trip failed: %v", err)
		}
		if _, err := c.Image(context.Background(), provider.Credentials{}, "fixture", parts, 1024); err == nil || calls != 1 {
			t.Fatal("missing credentials did not stop before submission")
		}
	}
}
