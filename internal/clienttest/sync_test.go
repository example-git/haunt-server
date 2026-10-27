package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSyncClient(t *testing.T) {
	if os.Getenv("HAUNT_SYNC_E2E") != "1" {
		t.Skip("set HAUNT_SYNC_E2E=1 with the isolated sync database and client checkout")
	}
	rtc := server(t, "Archive Sync Room", false)
	client := os.Getenv("HAUNT_CLIENT_DIR")
	if client == "" {
		t.Fatal("HAUNT_CLIENT_DIR is required")
	}
	syncDir := filepath.Join(client, "..", "sync")
	manifest := filepath.Join(t.TempDir(), "sync-ui.json")
	encoded, err := json.Marshal(map[string]string{"front": "http://127.0.0.1:5173", "backend": rtc.URL, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "test", "./internal/service", "-run", "^TestBrowserProtocol$", "-count=1", "-v", "-timeout=8m")
	command.Dir = syncDir
	command.Env = append(os.Environ(), "HAUNT_SYNC_UI_FIXTURE="+manifest, "HAUNT_SYNC_CLIENT_CHECKOUT="+client)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
}
