package tools

import (
	"encoding/binary"
	"errors"
	"github.com/example-git/haunt-server/internal/config"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"slices"

	xdraw "golang.org/x/image/draw"
)

// Walk block lengths before DecodeAll allocates the frame buffers.
func checkGIF(b []byte, w, h int, limits config.URLReaderLimits) error {
	bad := errors.New("malformed GIF blocks")
	if len(b) < 13 {
		return bad
	}
	pos := 13
	paletteBytes := func(flags byte) int {
		if flags&0x80 != 0 {
			return 3 << (1 + (flags & 7))
		}
		return 0
	}
	pos += paletteBytes(b[10])
	count := 0
	for pos < len(b) {
		block := b[pos]
		pos++
		switch block {
		case 0x3b:
			if count == 0 {
				return bad
			}
			return nil
		case 0x21:
			pos++ // Extension label, followed by length-prefixed sub-blocks.
		case 0x2c:
			if len(b)-pos < 9 {
				return bad
			}
			num := func(at int) int { return int(binary.LittleEndian.Uint16(b[at:])) }
			x, y, fw, fh := num(pos), num(pos+2), num(pos+4), num(pos+6)
			if fw == 0 || fh == 0 || x+fw > w || y+fh > h {
				return bad
			}
			count++
			if count > limits.GIFFrames || int64(count)*int64(w)*int64(h) > int64(limits.GIFCanvasPixels) {
				return errors.New("animation exceeds frame or aggregate pixel limit")
			}
			pos += 9 + paletteBytes(b[pos+8]) + 1 // Local palette and LZW code size.
		default:
			return bad
		}
		for {
			if pos >= len(b) {
				return bad
			}
			n := int(b[pos])
			pos++
			if n == 0 {
				break
			}
			if n > len(b)-pos {
				return bad
			}
			pos += n
		}
	}
	return bad
}

func gifBackground(g *gif.GIF, frame *image.Paletted) color.Color {
	for _, c := range frame.Palette {
		if _, _, _, a := c.RGBA(); a == 0 {
			return color.Transparent
		}
	}
	if p, ok := g.Config.ColorModel.(color.Palette); ok && int(g.BackgroundIndex) < len(p) {
		return p[g.BackgroundIndex]
	}
	return color.Transparent
}
func walkGIF(g *gif.GIF, visit func(int, *image.RGBA) error) error {
	canvas := image.NewRGBA(image.Rect(0, 0, g.Config.Width, g.Config.Height))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(gifBackground(g, g.Image[0])), image.Point{}, draw.Src)
	for i, frame := range g.Image {
		disposal := g.Disposal[i]
		rect := frame.Bounds()
		var prior *image.RGBA
		if disposal == gif.DisposalPrevious {
			prior = image.NewRGBA(rect)
			draw.Draw(prior, rect, canvas, rect.Min, draw.Src)
		}
		draw.Draw(canvas, rect, frame, rect.Min, draw.Over)
		if err := visit(i, canvas); err != nil {
			return err
		}
		switch disposal {
		case gif.DisposalPrevious:
			draw.Draw(canvas, rect, prior, rect.Min, draw.Src)
		case gif.DisposalBackground:
			draw.Draw(canvas, rect, image.NewUniform(gifBackground(g, frame)), image.Point{}, draw.Src)
		}
	}
	return nil
}

type transition struct{ index, change int }

func distance(a, b []byte) int {
	sum := 0
	for i, v := range a {
		d := int(v) - int(b[i])
		sum += max(d, -d)
	}
	return sum / len(a)
}

func sampleGIF(g *gif.GIF, limits config.URLReaderLimits, v *visualResource) error {
	limit := limits.Samples
	n := len(g.Image)
	v.FrameCount = n
	times := make([]int, n)
	for i, delay := range g.Delay {
		times[i] = v.Duration
		v.Delays = append(v.Delays, delay*10)
		v.Duration += delay * 10
	}
	if v.Duration > limits.GIFDurationMS {
		return errors.New("animation exceeds duration limit")
	}
	if n == 1 {
		return walkGIF(g, func(_ int, canvas *image.RGBA) error {
			f, err := encodeFrame(canvas, limits.StillSide, 0)
			if err == nil {
				v.Frames = []visualFrame{f}
			}
			return err
		})
	}
	v.Kind = "animated_image"
	v.Sampling = "representative_frames"
	thumbs := make([][]byte, n)
	var cuts []transition
	err := walkGIF(g, func(i int, canvas *image.RGBA) error {
		tiny := image.NewRGBA(image.Rect(0, 0, 16, 16))
		xdraw.ApproxBiLinear.Scale(tiny, tiny.Bounds(), canvas, canvas.Bounds(), draw.Src, nil)
		thumbs[i] = tiny.Pix
		if i > 0 {
			cuts = append(cuts, transition{i, distance(thumbs[i-1], thumbs[i])})
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortStableFunc(cuts, func(a, b transition) int { return b.change - a.change })
	selected := []int{}
	add := func(i int) {
		if len(selected) >= limit {
			return
		}
		for _, j := range selected {
			if distance(thumbs[i], thumbs[j]) <= 2 {
				return
			}
		}
		selected = append(selected, i)
	}
	at := func(numerator, denominator int) int {
		if v.Duration == 0 {
			return (n - 1) * numerator / denominator
		}
		instant := v.Duration * numerator / denominator
		for i := range times {
			if times[i]+v.Delays[i] > instant {
				return i
			}
		}
		return n - 1
	}
	if limit == 1 {
		add(at(1, 2))
	} else {
		add(0)
		add(n - 1)
		add(at(1, 2))
	}
	for _, cut := range cuts {
		if cut.change < 10 {
			break
		}
		add(cut.index)
		add(cut.index - 1)
	}
	for i := 1; i < limit*2; i++ {
		add(at(i, limit*2))
	}
	slices.Sort(selected)
	v.AllFrames = len(selected) == n
	return walkGIF(g, func(i int, canvas *image.RGBA) error {
		if !slices.Contains(selected, i) {
			return nil
		}
		f, e := encodeFrame(canvas, limits.SampledSide, times[i])
		if e != nil {
			return e
		}
		v.Frames = append(v.Frames, f)
		return nil
	})
}
