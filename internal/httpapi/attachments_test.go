package httpapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
)

func TestAttachmentMP4ContainerDetection(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "video.mp4")
	command := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("generate media: %v: %s", err, output)
		}
	}
	command("-v", "error", "-threads", "1", "-filter_threads", "1",
		"-f", "lavfi", "-i", "testsrc2=size=720x1280:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100",
		"-t", "0.2", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p",
		"-profile:v", "high", "-video_track_timescale", "9000000", "-c:a", "aac", "-ac", "2", video)
	payload, err := os.ReadFile(video)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) < 32 || binary.BigEndian.Uint32(payload[:4]) != 32 || string(payload[4:8]) != "ftyp" || http.DetectContentType(payload) != "video/mp4" {
		t.Fatal("expected an MP4 fixture with a 32-byte file type box")
	}
	iso := bytes.Clone(payload)
	binary.BigEndian.PutUint32(iso[:4], 24)
	copy(iso[8:12], "isom")
	binary.BigEndian.PutUint32(iso[12:16], 1)
	copy(iso[16:24], "isomiso4")
	binary.BigEndian.PutUint32(iso[24:28], 8)
	copy(iso[28:32], "free")
	if http.DetectContentType(iso) != "application/octet-stream" {
		t.Fatal("fixture must exercise MP4 identification beyond the HTTP MIME sniffer")
	}
	fragmentedPath := filepath.Join(dir, "fragmented.mp4")
	command("-v", "error", "-i", video, "-c", "copy", "-movflags", "frag_keyframe+empty_moov+default_base_moof", fragmentedPath)
	fragmented, err := os.ReadFile(fragmentedPath)
	if err != nil {
		t.Fatal(err)
	}
	padded := append([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}, fragmented...)
	if http.DetectContentType(padded) != "application/octet-stream" {
		t.Fatal("expected an unrecognized MP4 with a leading padding box")
	}
	audioPath := filepath.Join(dir, "audio.mp4")
	command("-v", "error", "-i", video, "-map", "0:a:0", "-c:a", "copy", audioPath)
	audio, err := os.ReadFile(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	transportPath := filepath.Join(dir, "video.ts")
	command("-v", "error", "-i", video, "-c", "copy", "-f", "mpegts", transportPath)
	transport, err := os.ReadFile(transportPath)
	if err != nil {
		t.Fatal(err)
	}
	if http.DetectContentType(transport) != "application/octet-stream" {
		t.Fatal("expected an unrecognized video container")
	}
	for _, tc := range []struct {
		name string
		data []byte
		want int
	}{
		{"mp4-brands", payload, http.StatusCreated},
		{"iso-brands", iso, http.StatusCreated},
		{"fragmented", fragmented, http.StatusCreated},
		{"leading-padding", padded, http.StatusCreated},
		{"header-only", iso[:24], http.StatusUnsupportedMediaType},
		{"binary-data", make([]byte, 1024), http.StatusUnsupportedMediaType},
		{"audio-only", audio, http.StatusUnsupportedMediaType},
		{"transport-stream", transport, http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := featureFixture(t, 256_000_000)
			channel := textChannel(t, f)
			path := "/api/v1/conversations/" + channel.ID + "/attachments?size=" + strconv64(int64(len(tc.data))) + "&filename=" + url.QueryEscape("video.mp4")
			req, err := http.NewRequest("POST", f.server.URL+path, bytes.NewReader(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+f.owner.Token)
			req.Header.Set("Content-Type", "video/mp4")
			response, err := f.server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != tc.want {
				t.Fatalf("upload got %d, want %d: %v: %s", response.StatusCode, tc.want, err, body)
			}
			if tc.want != http.StatusCreated {
				var used int64
				if err := f.pool.QueryRow(context.Background(), `SELECT attachment_bytes FROM chatxo.storage WHERE singleton`).Scan(&used); err != nil || used != 0 {
					t.Fatalf("rejected media retained %d storage bytes: %v", used, err)
				}
				return
			}
			var item attachments.Attachment
			if err := json.Unmarshal(body, &item); err != nil || item.ContentType != "video/mp4" {
				t.Fatalf("incorrect attachment metadata: %v: %s", err, body)
			}
			var message chat.Message
			f.call(t, "POST", "/api/v1/conversations/"+channel.ID+"/messages", f.owner.Token, map[string]any{"body": "video", "request_id": identity.NewID(), "attachment_ids": []string{item.ID}}, 201, &message)
			req, err = http.NewRequest("GET", f.server.URL+"/api/v1/attachments/"+item.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+f.owner.Token)
			response, err = f.server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err = io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "video/mp4" || !bytes.Equal(body, tc.data) {
				t.Fatalf("uploaded video did not round-trip unchanged: status=%d error=%v", response.StatusCode, err)
			}
		})
	}
}
