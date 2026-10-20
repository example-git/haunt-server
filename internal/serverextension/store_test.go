package serverextension

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (transport transportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

const testToken = "abcdef0123456789abcdef0123456789"

func response(status int, value string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(value))}
}
func audioResponse() *http.Response {
	return response(200, `{"status_code":0,"data":{"v_str":"`+base64.StdEncoding.EncodeToString([]byte("audio"))+`"}}`)
}
func configured(t *testing.T, transport transportFunc, enabled bool) *Store {
	t.Helper()
	store, err := New(t.TempDir(), &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(TikTokID, SaveRequest{Endpoint: PrimaryEndpoint, SessionToken: testToken, Enabled: enabled}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestServerExtensionConfigurationAndEncryptedCredentials(t *testing.T) {
	directory := t.TempDir()
	store, err := New(directory)
	if err != nil {
		t.Fatal(err)
	}
	if store.Catalog()[0].Enabled || store.Catalog()[0].Configured {
		t.Fatal("extension enabled before setup")
	}
	configuration, err := store.Save(TikTokID, SaveRequest{Endpoint: PrimaryEndpoint, SessionToken: testToken, Enabled: true})
	if err != nil || !configuration.Configured || configuration.Revision != 1 {
		t.Fatalf("save: %+v %v", configuration, err)
	}
	encoded, _ := json.Marshal(configuration)
	if bytes.Contains(encoded, []byte(testToken)) || bytes.Contains(encoded, []byte("session_token")) {
		t.Fatal("credential leaked in response")
	}
	disk, err := os.ReadFile(filepath.Join(directory, "server-extensions", "configuration.encrypted"))
	if err != nil || bytes.Contains(disk, []byte(testToken)) {
		t.Fatal("credential not encrypted")
	}
	restored, err := New(directory)
	if err != nil || !restored.Catalog()[0].Configured {
		t.Fatalf("restore: %v", err)
	}
	if _, err = restored.Save(TikTokID, SaveRequest{Revision: 0, Endpoint: PrimaryEndpoint}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale revision accepted")
	}
	if _, err = restored.Save(TikTokID, SaveRequest{Revision: 1, Endpoint: AlternateEndpoint, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if restored.value.Token != testToken {
		t.Fatal("blank input replaced saved token")
	}
	if _, err = restored.Save(TikTokID, SaveRequest{Revision: 2, Endpoint: PrimaryEndpoint, ClearToken: true}); err != nil {
		t.Fatal(err)
	}
	if restored.Catalog()[0].Configured || restored.Catalog()[0].Enabled {
		t.Fatal("credential removal failed")
	}
}

func TestServerExtensionRestrictsEndpointCredentialsAndPrompts(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []SaveRequest{
		{Endpoint: "https://attacker.test", SessionToken: testToken},
		{Endpoint: PrimaryEndpoint, SessionToken: "invalid"},
		{Endpoint: PrimaryEndpoint, Enabled: true},
		{Endpoint: PrimaryEndpoint, ClearToken: true, SessionToken: testToken},
	} {
		if _, err := store.Save(TikTokID, request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid configuration accepted: %v", err)
		}
	}
	if _, err := store.Speech(context.Background(), TikTokID, SpeechRequest{Text: strings.Repeat("x", 501), Voice: "en_us_001"}, false); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestTikTokMobileRequestAndUnicodeChunks(t *testing.T) {
	var calls atomic.Int32
	store := configured(t, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.String() != PrimaryEndpoint+"/media/api/text/speech/invoke/" || request.Method != "POST" {
			t.Fatal("incorrect destination")
		}
		if request.Header.Get("Cookie") != "sessionid="+testToken || !strings.Contains(request.UserAgent(), "Android 13") || request.Header.Get("x-common-params-v2") == "" {
			t.Fatal("missing mobile session headers")
		}
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("text_speaker") != "en_us_001" || request.Form.Get("aid") != "1233" || len(request.Form.Get("req_text")) > 300 {
			t.Fatal("incorrect speech form")
		}
		return audioResponse(), nil
	}, true)
	audio, err := store.Speech(context.Background(), TikTokID, SpeechRequest{Text: strings.Repeat("👻", 100), Voice: "en_us_001"}, false)
	if err != nil || string(audio) != "audioaudio" || calls.Load() != 2 {
		t.Fatalf("speech: %q %v calls=%d", audio, err, calls.Load())
	}
}

func TestServerExtensionTestBypassesCacheAndWorksBeforeEnable(t *testing.T) {
	var calls atomic.Int32
	store := configured(t, func(*http.Request) (*http.Response, error) { calls.Add(1); return audioResponse(), nil }, false)
	prompt := SpeechRequest{Text: "hello", Voice: "en_us_001"}
	if _, err := store.Speech(context.Background(), TikTokID, prompt, false); !errors.Is(err, ErrUnavailable) {
		t.Fatal("disabled extension served members")
	}
	if _, err := store.Speech(context.Background(), TikTokID, prompt, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(TikTokID, SaveRequest{Revision: 1, Endpoint: PrimaryEndpoint, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := store.Speech(context.Background(), TikTokID, prompt, false); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("shared speech not cached")
	}
	if _, err := store.Speech(context.Background(), TikTokID, prompt, true); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("live test used cached audio")
	}
}

func TestServerExtensionRejectsRedirectsAndRedactsProviderErrors(t *testing.T) {
	var calls atomic.Int32
	store := configured(t, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		result := response(302, testToken)
		result.Header.Set("Location", "https://attacker.test/")
		return result, nil
	}, true)
	_, err := store.Speech(context.Background(), TikTokID, SpeechRequest{Text: "hello", Voice: "en_us_001"}, false)
	if !errors.Is(err, ErrUpstream) || strings.Contains(err.Error(), testToken) || calls.Load() != 1 {
		t.Fatalf("unsafe provider error: %v", err)
	}
}

func TestServerExtensionCancellationDoesNotDisableOtherListeners(t *testing.T) {
	store := configured(t, func(request *http.Request) (*http.Response, error) {
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		return audioResponse(), nil
	}, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = store.Speech(ctx, TikTokID, SpeechRequest{Text: "cancelled", Voice: "en_us_001"}, false)
	if _, err := store.Speech(context.Background(), TikTokID, SpeechRequest{Text: "hello", Voice: "en_us_001"}, false); err != nil {
		t.Fatal("cancelled request blocked other users", err)
	}
}
