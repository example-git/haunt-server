package wire

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/ai/provider"
)

func TestProviderStreamSurvivesOrdinaryClientTimeout(t *testing.T) {
	connected := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(connected)
		select {
		case <-release:
			w.Write([]byte("complete"))
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 40 * time.Millisecond
	done := make(chan error, 1)
	submitted := make(chan struct{}, 1)
	go func() {
		resp, err := StreamRequest(context.Background(), client, server.URL, map[string]string{"input": "synthetic"}, http.Header{}, func(event provider.Event) error {
			if event.Type == "submitted" {
				submitted <- struct{}{}
			}
			return nil
		})
		if err == nil {
			content, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			err = readErr
			if string(content) != "complete" {
				t.Errorf("provider stream body %q", content)
			}
		}
		done <- err
	}()
	<-connected
	select {
	case <-submitted:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("provider submission was not reported")
	}
	select {
	case err := <-done:
		close(release)
		t.Fatalf("quiet provider ended at ordinary request timeout: %v", err)
	case <-time.After(2 * client.Timeout):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 40*time.Millisecond {
		t.Fatal("ordinary request timeout changed")
	}
}
