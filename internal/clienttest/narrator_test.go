package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/haunt-server/internal/serverextension"
	"github.com/example-git/haunt-server/internal/testutil"
)

func narratorExtension(t *testing.T, directory string) *serverextension.Store {
	t.Helper()
	path := testutil.InstallSpeechExtension(t, directory, "example-tts")
	audio, err := os.ReadFile("../../client/tests/fixtures/narrator-tone.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "audio.mp3"), audio, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := serverextension.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
