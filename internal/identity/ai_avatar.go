package identity

import (
	"bytes"
	"image"
	"image/gif"
	"image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const MaxAIAvatarInput = 64 << 20

func fitAIAvatar(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxAIAvatarInput {
		return nil, ErrInvalid
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 64<<20 {
		return nil, ErrInvalid
	}
	if format == "gif" {
		if err := validateGIF(raw, cfg.Width, cfg.Height); err != nil {
			return nil, err
		}
		if max(cfg.Width, cfg.Height) <= 1024 && len(raw) <= MaxArtBytes {
			return raw, nil
		}
		src, err := gif.DecodeAll(bytes.NewReader(raw))
		if err != nil {
			return nil, ErrInvalid
		}
		for edge := min(512, max(cfg.Width, cfg.Height)); edge >= 1; edge /= 2 {
			out, err := resizeAnimatedAvatar(src, edge)
			if err != nil {
				return nil, err
			}
			if len(out) <= MaxArtBytes {
				return out, nil
			}
		}
		return nil, ErrInvalid
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrInvalid
	}
	w, h := avatarSize(cfg.Width, cfg.Height, 512)
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(img, img.Bounds(), src, src.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, ErrInvalid
	}
	return out.Bytes(), nil
}

func avatarSize(width, height, edge int) (int, int) {
	largest := max(width, height)
	if largest <= edge {
		return width, height
	}
	return max(1, width*edge/largest), max(1, height*edge/largest)
}

func resizeAnimatedAvatar(src *gif.GIF, edge int) ([]byte, error) {
	w, h := avatarSize(src.Config.Width, src.Config.Height, edge)
	out := *src
	out.Config.Width, out.Config.Height = w, h
	out.Image = make([]*image.Paletted, len(src.Image))
	for i, frame := range src.Image {
		b := frame.Bounds()
		x0, y0 := b.Min.X*w/src.Config.Width, b.Min.Y*h/src.Config.Height
		x1 := min(w, (b.Max.X*w+src.Config.Width-1)/src.Config.Width)
		y1 := min(h, (b.Max.Y*h+src.Config.Height-1)/src.Config.Height)
		rect := image.Rect(x0, y0, x1, y1)
		if rect.Empty() {
			return nil, ErrInvalid
		}
		dst := image.NewPaletted(rect, frame.Palette)
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			sy := b.Min.Y + (y-y0)*b.Dy()/rect.Dy()
			for x := rect.Min.X; x < rect.Max.X; x++ {
				sx := b.Min.X + (x-x0)*b.Dx()/rect.Dx()
				dst.SetColorIndex(x, y, frame.ColorIndexAt(sx, sy))
			}
		}
		out.Image[i] = dst
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &out); err != nil {
		return nil, ErrInvalid
	}
	return buf.Bytes(), nil
}
