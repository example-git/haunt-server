package serverextension_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/serverextension"
	"github.com/example-git/haunt-server/internal/testutil"
)

const extensionID = "example-tts"
const testToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func configured(t *testing.T, enabled bool) (*serverextension.Store, string, string) {
	t.Helper()
	directory := t.TempDir()
	path := testutil.InstallSpeechExtension(t, directory, extensionID)
	store, err := serverextension.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Save(extensionID, serverextension.SaveRequest{Enabled: enabled, Values: map[string]string{"session_token": testToken}}); err != nil {
		t.Fatal(err)
	}
	return store, directory, path
}

func TestNoBuiltInExtension(t *testing.T) {
	store, err := serverextension.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(store.Catalog()) != 0 {
		t.Fatal("unexpected installed provider")
	}
	if _, err := store.Speech(context.Background(), "tiktok-tts", serverextension.SpeechRequest{Text: "hello"}, false); !errors.Is(err, serverextension.ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestConfigurationAndEncryptedCredentials(t *testing.T) {
	store, directory, _ := configured(t, true)
	configuration, err := store.Configuration(extensionID)
	if err != nil || !configuration.Configured || configuration.Revision != 1 || configuration.Name != "Example speech" || len(configuration.Fields) != 2 {
		t.Fatalf("configuration: %+v %v", configuration, err)
	}
	encoded, _ := json.Marshal(configuration)
	if bytes.Contains(encoded, []byte(testToken)) || configuration.Values["session_token"] != "" || len(configuration.ConfiguredSecrets) != 1 {
		t.Fatal("credential leaked")
	}
	disk, err := os.ReadFile(filepath.Join(directory, "server-extensions", "configurations.encrypted"))
	if err != nil || bytes.Contains(disk, []byte(testToken)) {
		t.Fatal("credential not encrypted")
	}
	restored, err := serverextension.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := restored.Save(extensionID, serverextension.SaveRequest{Revision: 0}); !errors.Is(err, serverextension.ErrConflict) {
		t.Fatal("stale revision accepted")
	}
	if _, err := restored.Save(extensionID, serverextension.SaveRequest{Revision: 1, Enabled: true, Values: map[string]string{"session_token": ""}}); err != nil {
		t.Fatal(err)
	}
	audio, err := restored.Speech(context.Background(), extensionID, serverextension.SpeechRequest{Text: "hello"}, false)
	if err != nil || string(audio) != "hello:example_voice" {
		t.Fatalf("external process: %q %v", audio, err)
	}
	cleared, err := restored.Save(extensionID, serverextension.SaveRequest{Revision: 2, ClearSecrets: []string{"session_token"}})
	if err != nil || cleared.Configured || cleared.Enabled {
		t.Fatal("credential removal failed", err)
	}
}

func TestManifestValidationAndExplicitConfiguration(t *testing.T) {
	store, _, _ := configured(t, false)
	for _, values := range []map[string]string{{"endpoint": "https://foreign.test"}, {"session_token": "invalid"}, {"unknown": "value"}} {
		if _, err := store.Save(extensionID, serverextension.SaveRequest{Revision: 1, Values: values}); !errors.Is(err, serverextension.ErrInvalid) {
			t.Fatal("invalid settings accepted", err)
		}
	}
	if _, err := store.Save(extensionID, serverextension.SaveRequest{Revision: 1, Values: map[string]string{"session_token": testToken}, ClearSecrets: []string{"session_token"}}); !errors.Is(err, serverextension.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := store.Speech(context.Background(), extensionID, serverextension.SpeechRequest{Text: strings.Repeat("x", 501)}, true); !errors.Is(err, serverextension.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := store.Speech(context.Background(), extensionID, serverextension.SpeechRequest{Text: "   "}, true); !errors.Is(err, serverextension.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestExternalProcessCacheAndDisabledAdministratorTest(t *testing.T) {
	store, _, path := configured(t, false)
	prompt := serverextension.SpeechRequest{Text: "hello", Voice: "chosen_voice"}
	if _, err := store.Speech(context.Background(), extensionID, prompt, false); !errors.Is(err, serverextension.ErrUnavailable) {
		t.Fatal("disabled provider served speech")
	}
	if audio, err := store.Speech(context.Background(), extensionID, prompt, true); err != nil || string(audio) != "hello:chosen_voice" {
		t.Fatalf("test: %q %v", audio, err)
	}
	if _, err := store.Save(extensionID, serverextension.SaveRequest{Revision: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := store.Speech(context.Background(), extensionID, prompt, false); err != nil {
			t.Fatal(err)
		}
	}
	calls, _ := os.ReadFile(filepath.Join(path, "calls"))
	if string(calls) != "1\n1\n" {
		t.Fatal("cache missed", string(calls))
	}
	if _, err := store.Speech(context.Background(), extensionID, prompt, true); err != nil {
		t.Fatal(err)
	}
	calls, _ = os.ReadFile(filepath.Join(path, "calls"))
	if string(calls) != "1\n1\n1\n" {
		t.Fatal("administrator test used cache")
	}
}

func TestExternalErrorsAndCancellation(t *testing.T) {
	store, _, _ := configured(t, true)
	for code, expected := range map[string]error{"invalid_request": serverextension.ErrInvalid, "credentials_rejected": serverextension.ErrCredentials, "busy": serverextension.ErrBusy, "provider_failed": serverextension.ErrUpstream, testToken: serverextension.ErrUpstream} {
		_, err := store.Speech(context.Background(), extensionID, serverextension.SpeechRequest{Text: "error:" + code}, true)
		if !errors.Is(err, expected) || strings.Contains(err.Error(), testToken) {
			t.Fatalf("error %s: %v", code, err)
		}
	}
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = store.Speech(ctx, extensionID, serverextension.SpeechRequest{Text: "cancelled"}, false)
	if _, err := store.Speech(context.Background(), extensionID, serverextension.SpeechRequest{Text: "hello"}, false); err != nil {
		t.Fatal("cancellation blocked another request", err)
	}
}

func TestSpeechCacheSeparatesVoiceAndText(t *testing.T) {
	store, _, path := configured(t, true)
	manifestPath := filepath.Join(path, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest serverextension.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.VoicePattern = ""
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	first, err := store.Speech(context.Background(), extensionID, serverextension.SpeechRequest{Text: "c", Voice: "a:b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Speech(context.Background(), extensionID, serverextension.SpeechRequest{Text: "b:c", Voice: "a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "c:a:b" || string(second) != "b:c:a" {
		t.Fatalf("mixed speech cache entries: %q %q", first, second)
	}
}

func TestReloadAddsAndRemovesPackages(t *testing.T) {
	store, directory, path := configured(t, true)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	if len(store.Catalog()) != 0 {
		t.Fatal("removed package advertised")
	}
	if _, err := store.Speech(context.Background(), extensionID, serverextension.SpeechRequest{Text: "hello"}, false); !errors.Is(err, serverextension.ErrUnavailable) {
		t.Fatal(err)
	}
	testutil.InstallSpeechExtension(t, directory, extensionID)
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	if !store.Catalog()[0].Configured || !store.Catalog()[0].Enabled {
		t.Fatal("reload discarded configuration")
	}
	if err := os.WriteFile(filepath.Join(path, "manifest.json"), []byte(`{"schema":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); !errors.Is(err, serverextension.ErrInvalid) {
		t.Fatal("invalid package accepted")
	}
}

func TestLegacyEncryptedConfigurationMigration(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "server-extensions")
	testutil.InstallSpeechExtension(t, directory, "tiktok-tts")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(map[string]any{"revision": 4, "enabled": true, "endpoint": "https://speech.example.test", "token": testToken})
	if err := os.WriteFile(filepath.Join(root, "key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "configuration.encrypted"), aead.Seal(nonce, nonce, plain, []byte("tiktok-tts")), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := serverextension.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	configuration, err := store.Configuration("tiktok-tts")
	if err != nil || configuration.Revision != 4 || !configuration.Configured || !configuration.Enabled {
		t.Fatal("migration lost settings", err)
	}
}
