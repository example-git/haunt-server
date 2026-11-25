package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--decode-url-image" {
		if err := DecodeURLImage(os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPageDiscoversVisibleImageAndScopesImmutableCache(t *testing.T) {
	var b bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, 2, 1))
	im.Set(1, 0, color.White)
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	r, _ := fixture(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<article>Test source <img alt="preview" src="/thumb"></article>`)
		case "/thumb":
			hits.Add(1)
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, immutable, max-age=1800")
			w.Write(b.Bytes())
		case "/redirect":
			http.Redirect(w, req, "/thumb", 302)
		case "/mismatch":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(b.Bytes())
		}
	})
	read := func(channel, path string) provider.ToolResult {
		call := provider.ToolCall{ID: "test", Name: "read_url", Arguments: json.RawMessage(fmt.Sprintf(`{"url":"https://public.example.com/%s","source_url":"https://public.example.com/page","media_role":"thumbnail"}`, path))}
		res, err := r.Execute(WithChannel(t.Context(), channel), call)
		if err != nil && path != "mismatch" {
			t.Fatal(err)
		}
		return res
	}
	if !strings.Contains(read("a", "page").Content, "https://public.example.com/thumb") {
		t.Fatal("missing discoverable image URL")
	}
	result := read("a", "redirect")
	var v visualResource
	if err := json.Unmarshal([]byte(result.Content), &v); err != nil {
		t.Fatal(err)
	}
	if v.FinalURL == v.RequestedURL || !strings.HasSuffix(v.FinalURL, "/thumb") || v.Role != "thumbnail" || v.SourceURL == "" || v.Downloaded != b.Len() || len(result.Media) != 2 || strings.Contains(result.Content, "pixels") {
		t.Fatal("lost source metadata or visual part")
	}
	img, err := png.Decode(bytes.NewReader(result.Media[1].Data))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := img.At(1, 0).RGBA(); a != 65535 {
		t.Fatal("pixels lost")
	}
	read("a", "thumb")
	read("a", "thumb")
	read("b", "thumb")
	if hits.Load() != 3 {
		t.Fatalf("unexpected image requests: %d", hits.Load())
	}
	if fail := read("a", "mismatch"); !fail.IsError || len(fail.Media) != 0 {
		t.Fatal("mismatched image was accepted")
	}
}

func TestGIFSamplesCompositedTimeline(t *testing.T) {
	pal := color.Palette{color.Transparent, color.White, color.Black, color.RGBA{255, 0, 0, 255}}
	frame := func(x, index int) *image.Paletted {
		f := image.NewPaletted(image.Rect(x, 0, x+1, 1), pal)
		f.Pix[0] = uint8(index)
		return f
	}
	g := &gif.GIF{Config: image.Config{Width: 3, Height: 1, ColorModel: pal}, Image: []*image.Paletted{frame(0, 1), frame(1, 2), frame(2, 3), frame(2, 3)}, Delay: []int{1, 40, 1, 1}, Disposal: []byte{gif.DisposalNone, gif.DisposalPrevious, gif.DisposalNone, gif.DisposalNone}}
	var in, out bytes.Buffer
	if err := gif.EncodeAll(&in, g); err != nil {
		t.Fatal(err)
	}
	if err := DecodeURLImage("{}", &in, &out); err != nil {
		t.Fatal(err)
	}
	var v visualResource
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Kind != "animated_image" || v.Complete || v.FrameCount != 4 || v.Duration != 430 || len(v.Frames) != 3 || v.Frames[1].Timestamp != 10 {
		t.Fatalf("wrong timeline: %v, %d frames, %d ms", v.Kind, len(v.Frames), v.Duration)
	}
	for i, f := range v.Frames {
		im, err := png.Decode(bytes.NewReader(f.Pixels))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, a := im.At(0, 0).RGBA(); a == 0 {
			t.Fatal("lost initial delta")
		}
		if _, _, _, a := im.At(1, 0).RGBA(); (a != 0) != (i == 1) {
			t.Fatal("restore-previous disposal failed")
		}
	}
}
