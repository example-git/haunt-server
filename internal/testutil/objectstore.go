package testutil

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ObjectStore is an isolated S3 protocol fixture. It exercises the real AWS SDK,
// including signed requests, multipart uploads and byte-range downloads.
type ObjectStore struct {
	Server    *httptest.Server
	Fail      atomic.Bool
	Multipart atomic.Int64
	mu        sync.Mutex
	objects   map[string][]byte
	uploads   map[string]map[int][]byte
	serial    int
}

func NewObjectStore(t *testing.T) *ObjectStore {
	t.Helper()
	s := &ObjectStore{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Server.Close)
	return s
}
func (s *ObjectStore) Count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.objects) }
func (s *ObjectStore) serve(w http.ResponseWriter, r *http.Request) {
	if s.Fail.Load() {
		w.WriteHeader(503)
		fmt.Fprint(w, "<Error><Code>ServiceUnavailable</Code></Error>")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		w.WriteHeader(403)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		w.WriteHeader(400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.URL.Path
	query := r.URL.Query()
	upload := query.Get("uploadId")
	digest := md5.Sum(body)
	etag := "\"" + hex.EncodeToString(digest[:]) + "\""
	switch {
	case r.Method == "POST" && query.Has("uploads"):
		s.serial++
		id := strconv.Itoa(s.serial)
		s.uploads[id] = map[int][]byte{}
		s.Multipart.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>test</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", strings.TrimPrefix(key, "/test/"), id)
	case r.Method == "PUT" && upload != "":
		part, _ := strconv.Atoi(query.Get("partNumber"))
		if s.uploads[upload] == nil {
			w.WriteHeader(404)
			return
		}
		s.uploads[upload][part] = body
		w.Header().Set("ETag", etag)
	case r.Method == "POST" && upload != "":
		var complete struct {
			Parts []struct {
				Number int    `xml:"PartNumber"`
				ETag   string `xml:"ETag"`
			} `xml:"Part"`
		}
		if xml.Unmarshal(body, &complete) != nil || len(complete.Parts) == 0 {
			w.WriteHeader(400)
			return
		}
		sort.Slice(complete.Parts, func(i, j int) bool { return complete.Parts[i].Number < complete.Parts[j].Number })
		var data []byte
		for _, part := range complete.Parts {
			b := s.uploads[upload][part.Number]
			sum := md5.Sum(b)
			if part.ETag != "\""+hex.EncodeToString(sum[:])+"\"" {
				w.WriteHeader(400)
				return
			}
			data = append(data, b...)
		}
		s.objects[key] = data
		delete(s.uploads, upload)
		fmt.Fprintf(w, "<CompleteMultipartUploadResult><Bucket>test</Bucket><Key>%s</Key><ETag>\"complete\"</ETag></CompleteMultipartUploadResult>", strings.TrimPrefix(key, "/test/"))
	case r.Method == "DELETE" && upload != "":
		delete(s.uploads, upload)
		w.WriteHeader(204)
	case r.Method == "PUT":
		s.objects[key] = body
		w.Header().Set("ETag", etag)
	case r.Method == "DELETE":
		delete(s.objects, key)
		w.WriteHeader(204)
	case r.Method == "HEAD" || r.Method == "GET":
		data, ok := s.objects[key]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		offset := 0
		status := 200
		if h := r.Header.Get("Range"); h != "" {
			if _, err := fmt.Sscanf(h, "bytes=%d-", &offset); err != nil || offset >= len(data) {
				w.WriteHeader(416)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data)))
			status = 206
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)-offset))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(status)
		if r.Method == "GET" {
			_, _ = w.Write(data[offset:])
		}
	default:
		w.WriteHeader(400)
	}
}
