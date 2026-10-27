package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/embeds"
	"github.com/example-git/haunt-server/internal/identity"
)

type previewFixture struct {
	calls atomic.Int32
	page  func(context.Context, string) (embeds.Preview, error)
}

func (p *previewFixture) Page(ctx context.Context, url string) (embeds.Preview, error) {
	p.calls.Add(1)
	if p.page != nil {
		return p.page(ctx, url)
	}
	return embeds.Preview{URL: url, Title: "A <script>plain title</script>", Kind: "article", Image: true, ImageURL: "https://preview.example.com/image"}, nil
}
func (*previewFixture) Image(context.Context, string) (embeds.Asset, error) {
	return embeds.Asset{Type: "image/png", Data: []byte("fixture-image")}, nil
}
func (*previewFixture) Close() {}

func TestEmbedsClientPostsKeepOtherLinkIndexes(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	preview := &previewFixture{}
	features.Previews.Close()
	features.Previews = preview
	channel := textChannel(t, f)
	m := sendMessage(t, f, f.owner.Token, channel.ID, "https://bsky.app/profile/author.bsky.social/post/3video https://preview.example.com/article https://www.tiktok.com/@author/video/123")
	path := "/api/v1/conversations/" + channel.ID + "/messages/" + m.ID + "/embeds"
	var response embedsResponse
	f.call(t, "GET", path+"?client_posts=true", f.owner.Token, nil, 200, &response)
	if preview.calls.Load() != 1 || len(response.Embeds) != 1 || response.Embeds[0].Index != 1 {
		t.Fatal("client posts fetched on server or other link index changed", preview.calls.Load(), response)
	}
	f.call(t, "GET", path, f.owner.Token, nil, 200, &response)
	if preview.calls.Load() != 4 || len(response.Embeds) != 3 {
		t.Fatal("legacy preview behavior changed", preview.calls.Load(), response)
	}
}

func TestEmbedsAuthorizationAndCurrentMessage(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	preview := &previewFixture{}
	features.Previews.Close()
	features.Previews = preview
	alice, bob := f.register(t, "alice"), f.register(t, "bob")
	var dm chat.Conversation
	f.call(t, "POST", "/api/v1/dms", alice.Token, map[string]any{"members": []string{bob.User.ID}}, 201, &dm)
	m := sendMessage(t, f, alice.Token, dm.ID, "https://preview.example.com/article")
	path := "/api/v1/conversations/" + dm.ID + "/messages/" + m.ID
	f.call(t, "GET", path+"/embeds", "", nil, 401, nil)
	f.call(t, "GET", path+"/embeds", f.owner.Token, nil, 404, nil)
	f.call(t, "GET", path+"/embeds/image?index=0", f.owner.Token, nil, 404, nil)
	if preview.calls.Load() != 0 {
		t.Fatal("unauthorized network fetch")
	}
	var response embedsResponse
	f.call(t, "GET", path+"/embeds", bob.Token, nil, 200, &response)
	if len(response.Embeds) != 1 || response.Embeds[0].Index != 0 || !response.Embeds[0].Image || response.Embeds[0].ImageURL != "" || response.Embeds[0].Asset != nil {
		t.Fatal(response)
	}
	f.call(t, "GET", path+"/embeds/image?index=3", bob.Token, nil, 400, nil)
	f.call(t, "GET", path+"/embeds/image?index=1", bob.Token, nil, 404, nil)
	req, _ := http.NewRequest("GET", f.server.URL+path+"/embeds/image?index=0", nil)
	req.Header.Set("Authorization", "Bearer "+bob.Token)
	r, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || string(data) != "fixture-image" || r.Header.Get("Content-Type") != "image/png" || r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("unsafe image response", r.StatusCode, r.Header)
	}
	f.call(t, "PUT", path, alice.Token, map[string]any{"body": "<https://preview.example.com/article> `https://preview.example.com/code`"}, 200, nil)
	before := preview.calls.Load()
	f.call(t, "GET", path+"/embeds", bob.Token, nil, 200, &response)
	if len(response.Embeds) != 0 || preview.calls.Load() != before {
		t.Fatal("suppressed links fetched")
	}
	f.call(t, "DELETE", path, alice.Token, nil, 200, nil)
	f.call(t, "GET", path+"/embeds", bob.Token, nil, 404, nil)
	accounting(t, f)
}

func TestEmbedsRecheckEditsDeletionExpiryAndRevocation(t *testing.T) {
	for _, action := range []string{"edit", "delete", "expire", "revoke", "hide"} {
		t.Run(action, func(t *testing.T) {
			f, features := featureFixture(t, 256_000_000)
			channel := textChannel(t, f)
			alice := f.register(t, "alice")
			message := sendMessage(t, f, f.owner.Token, channel.ID, "https://preview.example.com/article")
			path := "/api/v1/conversations/" + channel.ID + "/messages/" + message.ID
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			preview := &previewFixture{page: func(ctx context.Context, url string) (embeds.Preview, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return embeds.Preview{}, ctx.Err()
				}
				return embeds.Preview{URL: url, Title: "old metadata", Kind: "article"}, nil
			}}
			features.Previews.Close()
			features.Previews = preview
			type result struct {
				status int
				err    error
			}
			done := make(chan result, 1)
			go func() {
				status, _, err := f.request("GET", path+"/embeds", alice.Token, nil)
				done <- result{status, err}
			}()
			<-entered
			want := 404
			switch action {
			case "edit":
				f.call(t, "PUT", path, f.owner.Token, map[string]any{"body": "https://preview.example.com/replacement"}, 200, nil)
				want = 409
			case "delete":
				f.call(t, "DELETE", path, f.owner.Token, nil, 200, nil)
			case "expire":
				if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.messages SET created_at=now()-interval '31 days' WHERE id=$1`, message.ID); err != nil {
					t.Fatal(err)
				}
			case "revoke":
				f.call(t, "DELETE", "/api/v1/sessions/"+alice.Session.ID, alice.Token, nil, 204, nil)
				want = 401
			case "hide":
				var roles rolesResponse
				f.call(t, "GET", "/api/v1/roles", f.owner.Token, nil, 200, &roles)
				everyone := roles.Roles[0].ID
				f.call(t, "PUT", "/api/v1/channels/"+channel.ID+"/permissions/"+everyone, f.owner.Token, map[string]any{"allow": 0, "deny": 1}, 204, nil)
				want = 404
			}
			release <- struct{}{}
			r := <-done
			if r.err != nil || r.status != want {
				t.Fatal(action, r, want)
			}
		})
	}
}

func TestEmbedsBusyAndUnavailable(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	c := textChannel(t, f)
	m := sendMessage(t, f, f.owner.Token, c.ID, "https://preview.example.com/article")
	features.Previews.Close()
	features.Previews = &previewFixture{page: func(context.Context, string) (embeds.Preview, error) { return embeds.Preview{}, embeds.ErrBusy }}
	path := "/api/v1/conversations/" + c.ID + "/messages/" + m.ID + "/embeds"
	f.call(t, "GET", path, f.owner.Token, nil, 503, nil)
	f.call(t, "GET", path+"/image?index=0", f.owner.Token, nil, 503, nil)
	features.Previews = &previewFixture{page: func(context.Context, string) (embeds.Preview, error) { return embeds.Preview{}, embeds.ErrUnavailable }}
	var response embedsResponse
	f.call(t, "GET", path, f.owner.Token, nil, 200, &response)
	if len(response.Embeds) != 1 || response.Embeds[0].Status != "unavailable" {
		t.Fatal(response)
	}
	f.call(t, "GET", "/api/v1/attachments/"+identity.NewID()+"/metadata", f.owner.Token, nil, 404, nil)
}

func (p *previewFixture) Video(ctx context.Context, raw, byteRange string) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", raw, nil).WithContext(ctx)
	request.Header.Set("Range", byteRange)
	recorder.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(recorder, request, "video.mp4", time.Time{}, bytes.NewReader([]byte("0123456789")))
	return recorder.Result(), nil
}
func TestEmbedVideoTicketRangesAndRevocation(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	c := textChannel(t, f)
	alice := f.register(t, "video_viewer")
	m := sendMessage(t, f, f.owner.Token, c.ID, "https://x.com/person/status/123")
	features.Previews.Close()
	features.Previews = &previewFixture{page: func(_ context.Context, raw string) (embeds.Preview, error) {
		return embeds.Preview{URL: raw, Kind: "post", Media: []embeds.Media{{Kind: "video", URL: "https://video.twimg.com/movie.mp4"}}}, nil
	}}
	path := "/api/v1/conversations/" + c.ID + "/messages/" + m.ID + "/embeds"
	var ticket embedTicketResponse
	f.call(t, "GET", path+"/video-ticket?index=0&asset=0", alice.Token, nil, 200, &ticket)
	if ticket.Ticket == "" || ticket.Ticket == alice.Token {
		t.Fatal("invalid media ticket")
	}
	req, _ := http.NewRequest("GET", f.server.URL+path+"/video?ticket="+ticket.Ticket, nil)
	req.Header.Set("Range", "bytes=2-5")
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 206 || string(body) != "2345" || response.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatal(response.StatusCode, string(body), response.Header)
	}
	f.call(t, "DELETE", "/api/v1/sessions/"+alice.Session.ID, alice.Token, nil, 204, nil)
	f.call(t, "GET", path+"/video?ticket="+ticket.Ticket, "", nil, 401, nil)
	f.call(t, "GET", path+"/video?ticket=unknown", "", nil, 401, nil)
	f.call(t, "GET", path+"/video-ticket?index=0&asset=1", f.owner.Token, nil, 404, nil)
	f.call(t, "GET", path+"/video-ticket?index=0&asset=0", f.owner.Token, nil, 200, &ticket)
	f.call(t, "PUT", "/api/v1/conversations/"+c.ID+"/messages/"+m.ID, f.owner.Token, map[string]any{"body": "changed"}, 200, nil)
	f.call(t, "GET", path+"/video?ticket="+ticket.Ticket, "", nil, 409, nil)
}
