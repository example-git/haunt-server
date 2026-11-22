package ai

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"

	"github.com/example-git/haunt-server/internal/ai/provider"
)

func TestProfileGIFUsesOnlyFirstFrameOnOriginalCanvas(t *testing.T) {
	palette := color.Palette{color.Transparent, color.White, color.Black}
	patch := image.Rect(2, 1, 4, 2)
	first, second := image.NewPaletted(patch, palette), image.NewPaletted(patch, palette)
	first.SetColorIndex(2, 1, 1)
	second.SetColorIndex(2, 1, 2)
	var data bytes.Buffer
	if err := gif.EncodeAll(&data, &gif.GIF{
		Config: image.Config{Width: 6, Height: 3, ColorModel: palette},
		Image:  []*image.Paletted{first, second}, Delay: []int{1, 1},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := profilePNG(data.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	still, err := png.Decode(bytes.NewReader(raw))
	if err != nil || still.Bounds() != image.Rect(0, 0, 6, 3) {
		t.Fatalf("first frame dimensions: %v", err)
	}
	if r, _, _, a := still.At(2, 1).RGBA(); r != 65535 || a != 65535 {
		t.Fatal("second frame replaced the first")
	}
	if _, _, _, a := still.At(0, 0).RGBA(); a != 0 {
		t.Fatal("cropped transparent padding")
	}
}

func TestProfilePicturesShareProviderImageBudget(t *testing.T) {
	part := provider.Part{Type: "image", MIMEType: "image/png", Data: []byte("input")}
	pictures := profilePictures{{id: "id", username: "alice", image: part}}
	history := provider.Message{Role: "user"}
	for range maxContextImages {
		history.Parts = append(history.Parts, part)
	}
	req := provider.Request{SupportsVision: true, Messages: []provider.Message{history, {Role: "user", Parts: []provider.Part{part}}}}
	out := pictures.visualContext(req)
	if len(out.ToolMedia) != 2 || len(out.Messages[0].Parts) != maxContextImages-2 || len(out.Messages[1].Parts) != 1 {
		t.Fatal("history, current images and profile pictures did not share their image budget")
	}
	if len(req.ToolMedia) != 0 || len(req.Messages[0].Parts) != maxContextImages || req.Messages[0].Parts[maxContextImages-1].Type != "image" {
		t.Fatal("original context was mutated")
	}
	req.SupportsVision = false
	if len(pictures.visualContext(req).ToolMedia) != 0 {
		t.Fatal("text-only model received a picture")
	}
	req.SupportsVision = true
	req.Messages = []provider.Message{history}
	if len(pictures.visualContext(req).ToolMedia) != 0 {
		t.Fatal("a profile lookup displaced an image in the current request")
	}
	if _, err := pictures.references([]string{"unavailable"}); err == nil {
		t.Fatal("unresolved profile picture reference was accepted")
	}
}
