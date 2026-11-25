package config

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Operators can tighten each ceiling. All values are positive except redirects.
type URLReaderLimits struct {
	DownloadBytes   int `json:"download_bytes"`
	MaxSide         int `json:"max_side"`
	MaxPixels       int `json:"max_pixels"`
	GIFDurationMS   int `json:"gif_duration_ms"`
	GIFFrames       int `json:"gif_frames"`
	GIFCanvasPixels int `json:"gif_canvas_pixels"`
	Samples         int `json:"samples"`
	SampledSide     int `json:"sampled_side"`
	StillSide       int `json:"still_side"`
	Redirects       int `json:"redirects"`
	FetchSeconds    int `json:"fetch_seconds"`
	DecodeSeconds   int `json:"decode_seconds"`
	HostIntervalMS  int `json:"host_interval_ms"`
	CacheSeconds    int `json:"cache_seconds"`
}

func DefaultURLReaderLimits() URLReaderLimits {
	return URLReaderLimits{DownloadBytes: 8 << 20, MaxSide: 8192, MaxPixels: 12_000_000, GIFDurationMS: 300_000, GIFFrames: 300, GIFCanvasPixels: 60_000_000, Samples: 6, SampledSide: 768, StillSide: 1536, Redirects: 3, FetchSeconds: 60, DecodeSeconds: 12, HostIntervalMS: 250, CacheSeconds: 300}
}
func ParseURLReaderLimits(value string) (URLReaderLimits, error) {
	l := DefaultURLReaderLimits()
	if value == "" {
		return l, nil
	}
	d := json.NewDecoder(strings.NewReader(value))
	d.DisallowUnknownFields()
	if !strings.HasPrefix(strings.TrimSpace(value), "{") || d.Decode(&l) != nil || d.Decode(new(any)) != io.EOF {
		return l, fmt.Errorf("invalid HAUNT_AI_READER_LIMITS object")
	}
	top := DefaultURLReaderLimits()
	for _, n := range [][2]int{{l.DownloadBytes, top.DownloadBytes}, {l.MaxSide, top.MaxSide}, {l.MaxPixels, top.MaxPixels}, {l.GIFDurationMS, top.GIFDurationMS}, {l.GIFFrames, top.GIFFrames}, {l.GIFCanvasPixels, top.GIFCanvasPixels}, {l.Samples, top.Samples}, {l.SampledSide, top.SampledSide}, {l.StillSide, top.StillSide}, {l.FetchSeconds, top.FetchSeconds}, {l.DecodeSeconds, top.DecodeSeconds}} {
		if n[0] < 1 || n[0] > n[1] {
			return l, fmt.Errorf("HAUNT_AI_READER_LIMITS value outside supported range")
		}
	}
	if l.Redirects < 0 || l.Redirects > top.Redirects || l.CacheSeconds < 0 || l.CacheSeconds > top.CacheSeconds || l.HostIntervalMS < top.HostIntervalMS || l.HostIntervalMS > 5000 {
		return l, fmt.Errorf("HAUNT_AI_READER_LIMITS invalid redirect, cache or throttle setting")
	}
	return l, nil
}
