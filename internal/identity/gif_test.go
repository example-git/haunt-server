package identity

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"testing"
)

func animatedTestGIF(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	a := image.NewPaletted(image.Rect(0, 0, 4, 4), palette)
	b := image.NewPaletted(a.Rect, palette)
	b.SetColorIndex(1, 1, 1)
	var out bytes.Buffer
	if err := gif.EncodeAll(&out, &gif.GIF{Image: []*image.Paletted{a, b}, Delay: []int{5, 9}, LoopCount: 0, Disposal: []byte{gif.DisposalNone, gif.DisposalBackground}}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func TestAnimatedArtValidation(t *testing.T) {
	raw := animatedTestGIF(t)
	got, err := normalizeArt(raw, false)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("animation not preserved: %v", err)
	}
	for name, bad := range map[string][]byte{"truncated": raw[:len(raw)-1], "trailing": append(append([]byte{}, raw...), 1), "oversized": make([]byte, MaxArtBytes+1), "bad_frame": append([]byte{}, raw...)} {
		if name == "bad_frame" {
			bad[6] = 0xff
			bad[7] = 0xff
		}
		if _, err := normalizeArt(bad, false); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	// The inclusive 20 MiB boundary: valid GIF comments add bytes, not decoded pixels.
	padded := append([]byte{}, raw[:len(raw)-1]...)
	for len(padded)+1 < MaxArtBytes {
		left := MaxArtBytes - len(padded) - 1
		if left < 4 {
			t.Fatal("padding alignment")
		}
		n := min(255, left-4)
		if remain := left - (n + 4); remain > 0 && remain < 4 {
			n -= 4 - remain
		}
		padded = append(padded, 0x21, 0xfe, byte(n))
		padded = append(padded, make([]byte, n)...)
		padded = append(padded, 0)
	}
	padded = append(padded, 0x3b)
	if _, err := normalizeArt(padded, false); err != nil {
		t.Fatalf("20 MiB GIF rejected: %v", err)
	}
	// Tiny repeated frames cannot exceed the explicit CPU/frame budget.
	header := raw[:19]
	start := bytes.IndexByte(raw[19:], 0x2c) + 19
	end := len(raw) - 1
	frame := raw[start:end] // contains the two original frames; >1000 is still bounded.
	bomb := append([]byte{}, header...)
	for i := 0; i < 1001; i++ {
		bomb = append(bomb, frame...)
	}
	bomb = append(bomb, 0x3b)
	if err := validateGIF(bomb, 1024, 1024); err == nil {
		t.Fatal("unbounded frame count accepted")
	}
}
