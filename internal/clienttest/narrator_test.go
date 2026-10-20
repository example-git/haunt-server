package integration

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/serverextension"
)

// Only the third-party HTTP boundary is simulated. Browser authentication,
// configuration, participant events, speech routes and playback remain real.
type narratorTransport struct{ audio string }

func (transport narratorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.String() != serverextension.PrimaryEndpoint+"/media/api/text/speech/invoke/" ||
		request.Method != http.MethodPost {
		return nil, fmt.Errorf("unexpected narrator provider request")
	}
	if err := request.ParseForm(); err != nil {
		return nil, err
	}
	if request.Form.Get("text_speaker") == "" || request.Form.Get("req_text") == "" ||
		request.Header.Get("Cookie") != "sessionid="+strings.Repeat("a", 32) {
		return nil, fmt.Errorf("missing narrator provider inputs")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"status_code":0,"data":{"v_str":"` + transport.audio + `"}}`)),
	}, nil
}

func narratorExtension(t *testing.T, directory string) *serverextension.Store {
	t.Helper()
	audio, err := os.ReadFile("../../client/tests/fixtures/narrator-tone.mp3")
	if err != nil {
		t.Fatal(err)
	}
	store, err := serverextension.New(directory, &http.Client{
		Timeout:   5 * time.Second,
		Transport: narratorTransport{base64.StdEncoding.EncodeToString(audio)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
