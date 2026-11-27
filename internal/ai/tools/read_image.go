package tools

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/example-git/haunt-server/internal/config"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/example-git/haunt-server/internal/ai/provider"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const maxSamples = 6

type visualFrame struct {
	Timestamp int    `json:"timestamp_ms"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Format    string `json:"content_type"`
	// Pixels are transported only between the decoder and the tool process.
	Pixels []byte `json:"pixels,omitempty"`
}
type visualResource struct {
	Kind         string        `json:"kind"`
	RequestedURL string        `json:"requested_url"`
	FinalURL     string        `json:"final_url"`
	SourceURL    string        `json:"source_url,omitempty"`
	Role         string        `json:"media_role"`
	ContentType  string        `json:"content_type"`
	Width        int           `json:"width"`
	Height       int           `json:"height"`
	Downloaded   int           `json:"downloaded_bytes"`
	Resized      bool          `json:"resized_for_vision"`
	Duration     int           `json:"duration_ms"`
	FrameCount   int           `json:"original_frame_count"`
	Delays       []int         `json:"original_frame_delays_ms,omitempty"`
	Sampling     string        `json:"sampling_method,omitempty"`
	AllFrames    bool          `json:"all_frames_represented"`
	Complete     bool          `json:"complete_animation_provided"`
	Frames       []visualFrame `json:"frames"`
}

func (r *Registry) readImage(ctx context.Context, p readParams, source fetchedResource) (any, []provider.Part, error) {
	final := source.final.String()
	limits := r.limits
	n := limits.Samples
	if p.MaxFrames != nil {
		n = min(n, *p.MaxFrames)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(limits.DecodeSeconds)*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	limits.Samples = n
	options, err := json.Marshal(limits)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, executable, "--decode-url-image", string(options))
	cmd.Env = []string{"GOMEMLIMIT=160MiB", "GOMAXPROCS=1"}
	cmd.Stdin = bytes.NewReader(source.data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, nil, fmt.Errorf("image processing interrupted: %w", ctx.Err())
	}
	if err != nil {
		if stderr.Len() > 0 {
			return nil, nil, fmt.Errorf("image decoding failed: %s", bounded(stderr.String(), 256))
		}
		return nil, nil, errors.New("image decoding failed")
	}
	var v visualResource
	if json.Unmarshal(out, &v) != nil || len(v.Frames) == 0 || len(v.Frames) > n {
		return nil, nil, errors.New("invalid image decoder output")
	}
	v.RequestedURL, v.FinalURL, v.SourceURL, v.Role, v.ContentType, v.Downloaded = p.URL, final, p.SourceURL, p.MediaRole, source.kind, source.downloaded
	if v.Role == "" {
		v.Role = "unknown"
	}
	var media []provider.Part
	for i, f := range v.Frames {
		label := "Image from " + final
		if v.Kind == "animated_image" {
			label += fmt.Sprintf(", frame at %d ms", f.Timestamp)
		}
		media = append(media, provider.Part{Type: "text", Text: label}, provider.Part{Type: "image", MIMEType: f.Format, Data: f.Pixels})
		v.Frames[i].Pixels = nil
	}
	return v, media, nil
}

// DecodeURLImage runs in a short-lived child process, before server setup.
// The parent owns its deadline. Decoders cannot hold server memory after exit.
func DecodeURLImage(options string, in io.Reader, out io.Writer) error {
	limits, err := config.ParseURLReaderLimits(options)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(in, int64(limits.DownloadBytes+1)))
	if err != nil || len(data) > limits.DownloadBytes {
		return errors.New("image download exceeds limit")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return errors.New("malformed or unsupported image")
	}
	w, h := cfg.Width, cfg.Height
	if w <= 0 || h <= 0 || w > limits.MaxSide || h > limits.MaxSide || int64(w)*int64(h) > int64(limits.MaxPixels) {
		return errors.New("decoded image dimensions exceed limit")
	}
	v := visualResource{Kind: "image", Width: w, Height: h, FrameCount: 1}
	switch format {
	case "gif":
		if err = checkGIF(data, w, h, limits); err != nil {
			return err
		}
		anim, e := gif.DecodeAll(bytes.NewReader(data))
		if e != nil {
			return errors.New("malformed GIF")
		}
		if err = sampleGIF(anim, limits, &v); err != nil {
			return err
		}
	case "png", "jpeg", "webp":
		if animatedContainer(data, format) {
			return errors.New("unsupported animated format; animated GIF is supported")
		}
		im, _, e := image.Decode(bytes.NewReader(data))
		if e != nil {
			return errors.New("malformed image")
		}
		if im.Bounds().Dx() != w || im.Bounds().Dy() != h {
			return errors.New("inconsistent decoded image dimensions")
		}
		f, e := encodeFrame(im, limits.StillSide, 0)
		if e != nil {
			return e
		}
		v.Frames = []visualFrame{f}
	default:
		return errors.New("unsupported image format")
	}
	if v.Kind == "image" {
		v.AllFrames = true
	}
	v.Resized = v.Frames[0].Width != w || v.Frames[0].Height != h
	return json.NewEncoder(out).Encode(v)
}

// Check container chunks so an APNG or animated WebP cannot masquerade as a still.
func animatedContainer(data []byte, format string) bool {
	if format == "png" {
		for pos := 8; pos+12 <= len(data); {
			n := int64(binary.BigEndian.Uint32(data[pos:]))
			if string(data[pos+4:pos+8]) == "acTL" {
				return true
			}
			if n > int64(len(data)-pos-12) {
				break
			}
			pos += 12 + int(n)
		}
	}
	return format == "webp" && len(data) >= 21 && string(data[12:16]) == "VP8X" && data[20]&2 != 0
}

func encodeFrame(src image.Image, side, ms int) (visualFrame, error) {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if longest := max(w, h); longest > side {
		w, h = max(1, w*side/longest), max(1, h*side/longest)
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, bounds, draw.Src, nil)
		src = dst
	}
	var buf bytes.Buffer
	mime := "image/png"
	// Preserve text and transparent edges. Use JPEG for noisy images that
	// would consume a disproportionate share of the provider's byte budget.
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&buf, src); err != nil {
		return visualFrame{}, err
	}
	if buf.Len() > 640<<10 {
		mime = "image/jpeg"
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Over)
		buf.Reset()
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 88}); err != nil {
			return visualFrame{}, err
		}
	}
	return visualFrame{Timestamp: ms, Width: w, Height: h, Format: mime, Pixels: buf.Bytes()}, nil
}

// GeneratedPreview makes a bounded visual part from the exact private Codex output.
func GeneratedPreview(data []byte) (provider.Part, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 24<<20 {
		return provider.Part{}, errors.New("generated image exceeds preview limits or is malformed")
	}
	if format != "png" && format != "jpeg" && format != "webp" || animatedContainer(data, format) {
		return provider.Part{}, errors.New("generated image preview requires a supported still image")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return provider.Part{}, err
	}
	frame, err := encodeFrame(src, 1536, 0)
	return provider.Part{Type: "image", MIMEType: frame.Format, Data: frame.Pixels}, err
}
