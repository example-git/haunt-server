package embeds

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
)

const VideoChunk = 8 << 20

var rangePattern = regexp.MustCompile(`^bytes=(\d*)-(\d*)$`)
var contentRangePattern = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+)$`)

func mediaKind(kind string) string {
	switch kind {
	case "video/mp4", "video/webm":
		return "video"
	case "audio/mpeg", "audio/ogg", "application/ogg", "audio/wav", "audio/x-wav", "audio/flac":
		return "audio"
	}
	return ""
}

func mediaType(header []byte) string {
	kind := http.DetectContentType(header)
	if mediaKind(kind) != "" {
		return kind
	}
	if bytes.HasPrefix(header, []byte("fLaC")) {
		return "audio/flac"
	}
	if len(header) >= 16 && string(header[4:8]) == "ftyp" {
		size := int(binary.BigEndian.Uint32(header[:4]))
		if size >= 16 && size <= len(header) && size%4 == 0 {
			for offset := 8; offset+4 <= size; offset += 4 {
				if offset == 12 {
					continue
				}
				switch string(header[offset : offset+4]) {
				case "isom", "iso2", "iso4", "iso5", "iso6", "avc1", "mp41", "mp42", "M4V ", "MSNV", "dash":
					return "video/mp4"
				}
			}
		}
	}
	return ""
}

func (s *Service) Video(ctx context.Context, raw, byteRange string) (*http.Response, error) {
	u, err := ParseURL(raw)
	if err != nil {
		return nil, err
	}
	if byteRange == "" {
		byteRange = "bytes=0-"
	}
	parts := rangePattern.FindStringSubmatch(byteRange)
	if parts == nil || parts[1] == "" && parts[2] == "" {
		return nil, ErrUnavailable
	}
	var start, end int64
	if parts[1] == "" {
		suffix, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || suffix <= 0 {
			return nil, ErrUnavailable
		}
		byteRange = fmt.Sprintf("bytes=-%d", min(suffix, VideoChunk))
	} else {
		start, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || start > 1<<60 {
			return nil, ErrUnavailable
		}
		end = start + VideoChunk - 1
		if parts[2] != "" {
			requested, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil || requested < start {
				return nil, ErrUnavailable
			}
			end = min(end, requested)
		}
		byteRange = fmt.Sprintf("bytes=%d-%d", start, end)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", byteRange)
	req.Header.Set("Accept", "video/mp4,video/webm,audio/mpeg,audio/ogg,audio/wav,audio/flac")
	req.Header.Set("User-Agent", "Haunt/0.1 (+link-preview)")
	req.Header.Set("Accept-Encoding", "identity")
	response, err := s.mediaClient.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	valid := false
	defer func() {
		if !valid {
			response.Body.Close()
		}
	}()
	kind, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if kind == "application/octet-stream" {
		value, typeErr := s.load(ctx, "image:media-type:"+u.String(), func(ctx context.Context) cached {
			probe, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
			if err != nil {
				return cached{err: ErrUnavailable}
			}
			probe.Header.Set("Range", "bytes=0-511")
			probe.Header.Set("User-Agent", "Haunt/0.1 (+link-preview)")
			probe.Header.Set("Accept-Encoding", "identity")
			r, err := s.mediaClient.Do(probe)
			if err != nil {
				return cached{err: ErrUnavailable}
			}
			defer r.Body.Close()
			if r.StatusCode != 200 && r.StatusCode != 206 {
				return cached{err: ErrUnavailable}
			}
			prefix, err := io.ReadAll(io.LimitReader(r.Body, 512))
			actual := mediaType(prefix)
			if err != nil || actual == "" {
				return cached{err: ErrUnavailable}
			}
			return cached{preview: Preview{Kind: actual}}
		})
		if typeErr != nil {
			return nil, typeErr
		}
		kind = value.preview.Kind
	}
	if mediaKind(kind) == "" {
		return nil, ErrUnavailable
	}
	if response.ContentLength <= 0 || response.ContentLength > VideoChunk {
		return nil, ErrUnavailable
	}
	if response.StatusCode == 206 {
		match := contentRangePattern.FindStringSubmatch(response.Header.Get("Content-Range"))
		if match == nil {
			return nil, ErrUnavailable
		}
		lo, e1 := strconv.ParseInt(match[1], 10, 64)
		hi, e2 := strconv.ParseInt(match[2], 10, 64)
		total, e3 := strconv.ParseInt(match[3], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || lo > hi || hi >= total || hi-lo+1 != response.ContentLength || parts[1] != "" && (lo != start || hi > end) {
			return nil, ErrUnavailable
		}
	} else if response.StatusCode != 200 || start != 0 {
		return nil, ErrUnavailable
	}
	response.Header.Set("Content-Type", kind)
	valid = true
	return response, nil
}
