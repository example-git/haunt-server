package embeds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLinksProvidersAndAddressPolicy(t *testing.T) {
	got := Links("`https://code.example.com` <https://quiet.example.com> see https://public.example.com/article_(test). https://public.example.com/article_(test) https://other.example.com https://third.example.com https://fourth.example.com")
	want := []string{"https://public.example.com/article_(test)", "https://other.example.com/", "https://third.example.com/"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatal(got)
	}
	for _, raw := range []string{"file:///etc/passwd", "https://user:secret@example.com", "https://example.com:8443/", "https://example.com\\@127.0.0.1/", "https://[fe80::1%25eth0]/"} {
		u, err := ParseURL(raw)
		if err == nil && publicHost(u.Hostname()) {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.2.3.4", "172.16.1.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "168.63.129.16", "192.0.0.8", "198.18.0.1", "203.0.113.1", "224.0.0.1", "240.1.1.1", "0.0.0.0", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::a00:1", "2002:7f00:1::1", "2001:db8::1"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Fatal("private/reserved address", ip)
		}
	}
	for _, ip := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(ip)) {
			t.Fatal("public rejected", ip)
		}
	}
	for _, host := range []string{"localhost", "api.localhost", "metadata.internal", "printer.local", "private", "hidden.onion"} {
		if publicHost(host) {
			t.Fatal(host)
		}
	}
	for _, raw := range []string{"https://youtu.be/abcdefghijk?t=12", "https://www.youtube.com/watch?v=abcdefghijk", "https://youtube.com/shorts/abcdefghijk", "https://www.youtube.com/live/abcdefghijk"} {
		if YouTube(raw) != "abcdefghijk" {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{"https://youtube.com.evil.com/watch?v=abcdefghijk", "https://youtube.com/watch?v=bad", "https://youtu.be/abcdefghijk/extra"} {
		if YouTube(raw) != "" {
			t.Fatal(raw)
		}
	}
}
func TestDialPinsValidatedDNSAndRejectsMixedAnswers(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		calls := 0
		lookups := 0
		lookup := func(context.Context, string, string) ([]netip.Addr, error) {
			lookups++
			ips := []netip.Addr{netip.MustParseAddr("93.184.216.34")}
			if mixed {
				ips = append(ips, netip.MustParseAddr("127.0.0.1"))
			}
			return ips, nil
		}
		dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
			calls++
			if address != "93.184.216.34:443" {
				t.Fatal(address)
			}
			return nil, nil
		}
		_, err := dialPublic(context.Background(), "tcp", "public.example.com:443", lookup, dial)
		if mixed {
			if err == nil || calls != 0 {
				t.Fatal("mixed answers dialed")
			}
		} else if err != nil || calls != 1 || lookups != 1 {
			t.Fatal(err, calls, lookups)
		}
	}
}
func imageBytes() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 32, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{20, 130, 210, 255})
		}
	}
	var data bytes.Buffer
	_ = png.Encode(&data, img)
	return data.Bytes()
}
func fixtureService(t *testing.T, handler http.Handler) (*Service, *atomic.Int32) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s := New()
	s.client.CloseIdleConnections()
	calls := &atomic.Int32{}
	transport := s.client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialPublic(ctx, network, address, func(_ context.Context, _ string, host string) ([]netip.Addr, error) {
			if strings.Contains(host, "127.0.0.1") {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}, func(ctx context.Context, network, address string) (net.Conn, error) {
			calls.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "http://"))
		})
	}
	s.mediaClient.Transport = transport.Clone()
	t.Cleanup(s.Close)
	return s, calls
}
func TestMetadataCacheImagesAndRedirectValidation(t *testing.T) {
	var requests atomic.Int32
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Error("credentials forwarded")
		}
		switch r.URL.Path {
		case "/article":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<head><title>fallback</title><meta property="og:title" content="A &amp; B"><meta property="og:title" content="Ignored"><meta name="description" content="Plain description"><meta property="og:site_name" content="Test site"><meta property="article:author" content="Example Writer"><meta property="article:published_time" content="2026-10-18T12:30:00-04:00"><meta name="theme-color" content="#A1B2C3"><meta property="og:image" content="/picture"><meta property="og:url" content="https://evil.example.com/"></head><body><meta property="og:title" content="Ignored body">`)
		case "/picture":
			w.Header().Set("Content-Type", "image/png")
			w.Write(imageBytes())
		case "/redirect":
			http.Redirect(w, r, "http://127.0.0.1/secret", 302)
		case "/big":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, strings.Repeat("x", maxHTML+1))
		case "/svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			fmt.Fprint(w, `<svg onload="alert(1)"></svg>`)
		default:
			http.Error(w, "bad", 500)
		}
	}))
	ctx := context.Background()
	p, err := s.Page(ctx, "http://preview.example.com/article")
	if err != nil || p.Title != "A & B" || p.Description != "Plain description" || p.Author != "Example Writer" || p.PublishedAt != "2026-10-18T16:30:00Z" || p.ThemeColor != "#a1b2c3" || !p.Image || p.URL != "http://preview.example.com/article" || p.ImageURL != "http://preview.example.com/picture" {
		t.Fatal(p, err)
	}
	_, err = s.Page(ctx, p.URL)
	if err != nil || requests.Load() != 1 {
		t.Fatal("cache failed", err, requests.Load())
	}
	a, err := s.Image(ctx, p.ImageURL)
	if err != nil || a.Type != "image/png" || len(a.Data) == 0 {
		t.Fatal(err)
	}
	p, err = s.Page(ctx, p.ImageURL)
	if err != nil || p.Kind != "image" || p.Asset == nil {
		t.Fatal(p, err)
	}
	for _, path := range []string{"redirect", "big", "svg"} {
		if _, err = s.Page(ctx, "http://preview.example.com/"+path); err == nil {
			t.Fatal(path)
		}
	}
	before := requests.Load()
	if _, err = s.Page(ctx, "http://preview.example.com/redirect"); err == nil || requests.Load() != before {
		t.Fatal("failure wasn't cached")
	}
	if _, err = s.Image(ctx, "http://preview.example.com/svg"); err == nil {
		t.Fatal("SVG accepted")
	}
}
func TestSingleFlightBoundedConcurrencyCancellationAndClose(t *testing.T) {
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>shared</title>")
	}))
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.Page(context.Background(), "http://preview.example.com/shared")
			if err != nil || p.Title != "shared" {
				t.Error(err, p)
			}
		}()
	}
	<-entered
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.Page(context.Background(), fmt.Sprintf("http://host%d.example.com/path", i))
		}(i)
	}
	for i := 0; i < 7; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent fetch blocked")
		}
	}
	if _, err := s.Page(context.Background(), "http://overflow.example.com/"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Page(ctx, "http://preview.example.com/shared"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	s.Close()
	if _, err := s.Page(context.Background(), "http://preview.example.com/shared"); err == nil {
		t.Fatal("closed service accepted work")
	}
}
func TestCacheMemoryBoundAndHTMLNeverExecutes(t *testing.T) {
	s := New()
	defer s.Close()
	for i := 0; i < 520; i++ {
		_, err := s.load(context.Background(), fmt.Sprint(i), func(context.Context) cached { return cached{asset: Asset{Data: make([]byte, 128<<10)}} })
		if err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bytes > maxCache || s.order.Len() > 512 || len(s.running) != 0 {
		t.Fatal(s.bytes, s.order.Len())
	}
	values, title := metadata([]byte(`<title>&lt;img onerror=alert(1)&gt;</title><script><meta property="og:title" content="script"></script><meta property="og:description" content="Text only">`))
	if title != "<img onerror=alert(1)>" || values["og:title"] != "" {
		t.Fatal(values, title)
	}
}

func TestMetadataQueueAndMediaConnectionIsolation(t *testing.T) {
	entered, release := make(chan struct{}, 9), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	picture := imageBytes()
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/picture" {
			w.Header().Set("Content-Type", "image/png")
			w.Write(picture)
			return
		}
		entered <- struct{}{}
		<-release
		fmt.Fprint(w, "<title>Ready</title>")
	}))
	done := make(chan error, 9)
	for i := range 8 {
		host := "same.example.com"
		if i >= 4 {
			host = fmt.Sprintf("other%d.example.com", i)
		}
		go func() {
			_, err := s.Page(context.Background(), fmt.Sprintf("http://%s/blocked/%d", host, i))
			done <- err
		}()
	}
	for range 8 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("metadata did not fill provider slots")
		}
	}
	go func() { _, err := s.Page(context.Background(), "http://same.example.com/queued"); done <- err }()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	asset, err := s.Image(ctx, "http://same.example.com/picture")
	cancel()
	if err != nil || !bytes.Equal(asset.Data, picture) {
		t.Fatal("media blocked behind metadata", err)
	}
	select {
	case err := <-done:
		t.Fatal("metadata queue returned before release", err)
	default:
	}
	once.Do(func() { close(release) })
	for range 9 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestImageLimitsAndAnimatedGIFPreservation(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second.SetColorIndex(0, 0, 1)
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 10}, LoopCount: 0}); err != nil {
		t.Fatal(err)
	}
	asset, err := readImage(bytes.NewReader(encoded.Bytes()))
	if err != nil || asset.Type != "image/gif" || !bytes.Equal(asset.Data, encoded.Bytes()) {
		t.Fatal("animation modified", err)
	}
	if _, err = readImage(strings.NewReader(strings.Repeat("x", maxImage+1))); err == nil {
		t.Fatal("oversized image accepted")
	}
	var huge bytes.Buffer
	if err = png.Encode(&huge, image.NewRGBA(image.Rect(0, 0, 8193, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err = readImage(bytes.NewReader(huge.Bytes())); err == nil {
		t.Fatal("oversized dimensions accepted")
	}
}
