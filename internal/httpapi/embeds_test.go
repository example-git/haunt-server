package httpapi

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

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
	if len(response.Embeds) != 0 {
		t.Fatal(response)
	}
	f.call(t, "GET", "/api/v1/attachments/"+identity.NewID()+"/metadata", f.owner.Token, nil, 404, nil)
}
