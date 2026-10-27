package embeds

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOfficialPlayerSelection(t *testing.T) {
	for _, tt := range []struct{ raw, player, id, token string }{
		{"https://vimeo.com/123456789/abc123def0", "vimeo", "123456789", "abc123def0"},
		{"https://player.vimeo.com/video/123456789?h=abc123def0", "vimeo", "123456789", "abc123def0"},
		{"https://streamable.com/a1b2c3", "streamable", "a1b2c3", ""},
		{"https://www.twitch.tv/videos/123456", "twitch-video", "123456", ""},
		{"https://twitch.tv/channel_name", "twitch-channel", "channel_name", ""},
		{"https://clips.twitch.tv/Some-Clip_123", "twitch-clip", "Some-Clip_123", ""},
		{"https://twitch.tv/name/clip/Some-Clip_123", "twitch-clip", "Some-Clip_123", ""},
		{"https://vimeo.com.attacker.com/12345", "", "", ""},
		{"https://attacker.com/?player=https://player.vimeo.com/video/12345", "", "", ""},
		{"https://streamable.com/%2f%2fattacker.com", "", "", ""},
		{"https://vimeo.com/12345?h=https://attacker.com", "", "", ""},
		{"https://twitch.tv/directory", "", "", ""},
	} {
		u, err := ParseURL(tt.raw)
		if err != nil {
			t.Fatal(err)
		}
		p := playerPreview(u)
		if p.Player != tt.player || p.PlayerID != tt.id || p.PlayerToken != tt.token {
			t.Fatalf("%s: %+v", tt.raw, p)
		}
	}
}

type headOnlyReader struct{ read bool }

func (r *headOnlyReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, fmt.Errorf("body must not be read")
	}
	r.read = true
	return copy(p, `<html><head><title>Head only</title><meta name="description" content="Safe text"><meta property="og:video" content="https://attacker.com/player"><meta http-equiv="refresh" content="0;url=https://attacker.com"></head>`), nil
}
func TestMetadataStopsAtHeadAndNeverSelectsUserPlayerHTML(t *testing.T) {
	meta, title, err := readMetadata(&headOnlyReader{})
	if err != nil || title != "Head only" || meta["description"] != "Safe text" {
		t.Fatal(meta, title, err)
	}
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Authorization", "Cookie", "X-Forwarded-For", "Forwarded", "X-Real-IP", "Origin", "Referer"} {
			if r.Header.Get(name) != "" {
				t.Errorf("forwarded %s", name)
			}
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<head><title>Article</title><meta property="og:image" content="https://attacker.com/tracker.png"><meta property="og:video" content="https://attacker.com/player"><meta name="twitter:player" content="https://attacker.com/player"></head><body><iframe src="https://attacker.com/player"></iframe>`)
	}))
	p, err := s.Page(context.Background(), "http://preview.example.com/article")
	if err != nil || p.Player != "" || p.YouTubeID != "" || len(p.Media) != 0 || p.Title != "Article" || p.ImageURL != "https://attacker.com/tracker.png" || len(p.ImageKey) != 64 {
		t.Fatal(p, err)
	}
}

func TestDirectMediaSignaturesAndRanges(t *testing.T) {
	mp4 := append([]byte{0, 0, 0, 24}, []byte("ftypisom\x00\x00\x00\x00isommp42")...)
	mp4 = append(mp4, bytes.Repeat([]byte{0}, 1000)...)
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			w.Write(mp4)
		case "/download":
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeContent(w, r, "video", time.Time{}, bytes.NewReader(mp4))
		case "/audio.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(append([]byte("ID3"), bytes.Repeat([]byte{0}, 600)...))
		case "/picture":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(imageBytes())
		case "/fake.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "<html><script>alert(1)</script></html>")
		case "/playlist.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\nhttps://attacker.com/video")
		case "/document.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			io.WriteString(w, "%PDF-1.7")
		default:
			w.WriteHeader(404)
		}
	}))
	for _, tt := range []struct{ path, kind string }{{"video.mp4", "video"}, {"download", "video"}, {"audio.mp3", "audio"}, {"picture", "image"}, {"document.pdf", "file"}} {
		p, err := s.Page(context.Background(), "http://preview.example.com/"+tt.path)
		if err != nil || p.Kind != tt.kind {
			t.Fatal(tt.path, p, err)
		}
	}
	for _, path := range []string{"fake.mp4", "playlist.m3u8"} {
		if _, err := s.Page(context.Background(), "http://preview.example.com/"+path); err == nil {
			t.Fatal("accepted", path)
		}
	}
	response, err := s.Video(context.Background(), "http://preview.example.com/download", "bytes=100-199")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 206 || response.Header.Get("Content-Type") != "video/mp4" || len(data) != 100 {
		t.Fatal(response, len(data), err)
	}
	if strings.Contains(response.Header.Get("Content-Type"), "html") {
		t.Fatal("executable media")
	}
}
