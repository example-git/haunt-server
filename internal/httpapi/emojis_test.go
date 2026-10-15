package httpapi

import (
	"bytes"
	"context"
	"errors"
	"github.com/example-git/haunt-server/internal/identity"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"
)

func TestEmojisAndAnimatedArt(t *testing.T) {
	f := newFixture(t)
	alice := f.register(t, "emoji_user")
	palette := color.Palette{color.NRGBA{}, color.White}
	a := image.NewPaletted(image.Rect(0, 0, 16, 16), palette)
	b := image.NewPaletted(a.Rect, palette)
	b.SetColorIndex(0, 0, 1)
	var animated bytes.Buffer
	if err := gif.EncodeAll(&animated, &gif.GIF{Image: []*image.Paletted{a, b}, Delay: []int{5, 10}, LoopCount: 0}); err != nil {
		t.Fatal(err)
	}
	raw := animated.Bytes()
	for _, kind := range []string{"avatar", "icon", "banner"} {
		path := "/api/v1/instance/" + kind
		get := path
		if kind == "avatar" {
			path = "/api/v1/me/avatar"
			get = "/api/v1/users/" + f.owner.User.ID + "/avatar"
		}
		artRequest(t, f, "PUT", path, f.owner.Token, raw, 200)
		got := artRequest(t, f, "GET", get, f.owner.Token, nil, 200)
		if !bytes.Equal(raw, got) {
			t.Fatalf("%s GIF was flattened", kind)
		}
	}
	artRequest(t, f, "PUT", "/api/v1/me/avatar", f.owner.Token, make([]byte, identity.MaxArtBytes+1), 413)
	padded := append([]byte{}, raw[:len(raw)-1]...)
	for len(padded)+1 < identity.MaxArtBytes {
		left := identity.MaxArtBytes - len(padded) - 1
		n := min(255, left-4)
		if remain := left - (n + 4); remain > 0 && remain < 4 {
			n -= 4 - remain
		}
		padded = append(padded, 0x21, 0xfe, byte(n))
		padded = append(padded, make([]byte, n)...)
		padded = append(padded, 0)
	}
	padded = append(padded, 0x3b)
	artRequest(t, f, "PUT", "/api/v1/me/avatar", f.owner.Token, padded, 200)
	if got := artRequest(t, f, "GET", "/api/v1/users/"+f.owner.User.ID+"/avatar", f.owner.Token, nil, 200); !bytes.Equal(got, padded) {
		t.Fatal("20 MiB animation did not survive database storage")
	}
	path := "/api/v1/emojis/wave"
	artRequest(t, f, "PUT", path, "", raw, 401)
	artRequest(t, f, "PUT", path, alice.Token, raw, 403)
	artRequest(t, f, "PUT", "/api/v1/emojis/Bad-Name", f.owner.Token, raw, 400)
	artRequest(t, f, "PUT", path, f.owner.Token, raw, 200)
	var list emojisResponse
	f.call(t, "GET", "/api/v1/emojis", alice.Token, nil, 200, &list)
	if len(list.Emojis) != 1 || list.Emojis[0].Name != "wave" || !list.Emojis[0].Animated {
		t.Fatalf("bad catalog: %#v", list)
	}
	first := list.Emojis[0]
	other := newFixture(t)
	var elsewhere emojisResponse
	other.call(t, "GET", "/api/v1/emojis", other.owner.Token, nil, 200, &elsewhere)
	if len(elsewhere.Emojis) != 0 {
		t.Fatal("cross-instance emoji catalog")
	}
	artRequest(t, other, "GET", "/api/v1/emojis/"+first.ID+"/image", other.owner.Token, nil, 404)
	artRequest(t, f, "GET", "/api/v1/emojis/"+first.ID+"/image", "", nil, 401)
	if got := artRequest(t, f, "GET", "/api/v1/emojis/"+first.ID+"/image", alice.Token, nil, 200); !bytes.Equal(got, raw) {
		t.Fatal("emoji animation changed")
	}
	var static bytes.Buffer
	_ = png.Encode(&static, image.NewRGBA(image.Rect(0, 0, 16, 16)))
	artRequest(t, f, "PUT", path, f.owner.Token, static.Bytes(), 200)
	f.call(t, "GET", "/api/v1/emojis", alice.Token, nil, 200, &list)
	if len(list.Emojis) != 1 || list.Emojis[0].ID != first.ID || list.Emojis[0].Version == first.Version || list.Emojis[0].Animated {
		t.Fatal("replacement did not retain identity/change version")
	}
	artRequest(t, f, "PUT", path, f.owner.Token, make([]byte, identity.MaxEmojiBytes+1), 413)
	var wide bytes.Buffer
	_ = png.Encode(&wide, image.NewRGBA(image.Rect(0, 0, 257, 1)))
	artRequest(t, f, "PUT", path, f.owner.Token, wide.Bytes(), 400)
	f.call(t, "DELETE", "/api/v1/emojis/"+first.ID, alice.Token, nil, 403, nil)
	f.call(t, "DELETE", "/api/v1/emojis/"+first.ID, f.owner.Token, nil, 204, nil)
	artRequest(t, f, "GET", "/api/v1/emojis/"+first.ID+"/image", alice.Token, nil, 404)
	f.call(t, "GET", "/api/v1/emojis", alice.Token, nil, 200, &list)
	if len(list.Emojis) != 0 {
		t.Fatal("deleted emoji still listed")
	}
}

func TestEmojiQuotaConcurrentMutations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	raw := pngData.Bytes()
	_, err := f.pool.Exec(ctx, `INSERT INTO chatxo.emojis(id,instance_id,name,version,animated,image)
 SELECT gen_random_uuid(),$1,'seed_'||i,gen_random_uuid(),false,$2 FROM generate_series(1,99) i`, f.cfg.InstanceID, raw)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"final_one", "final_two"} {
		go func(name string) {
			<-start
			_, err := f.identities.SaveEmoji(ctx, identity.Principal{User: f.owner.User}, name, raw)
			results <- err
		}(name)
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, identity.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("quota race: success=%d conflict=%d", success, conflict)
	}
	catalog, err := f.identities.Emojis(ctx)
	if err != nil || len(catalog) != 100 {
		t.Fatalf("quota exceeded: %d %v", len(catalog), err)
	}
	if _, err = f.identities.SaveEmoji(ctx, identity.Principal{User: f.owner.User}, "seed_1", raw); err != nil {
		t.Fatalf("replacement at quota: %v", err)
	}
}
