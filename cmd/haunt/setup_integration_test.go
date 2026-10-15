package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This test launches the production binary, with an explicitly dedicated test
// database. It never falls back to deployment settings or a production database.
func TestFirstRunBinary(t *testing.T) {
	databaseURL, binary := os.Getenv("HAUNT_WIZARD_TEST_DATABASE_URL"), os.Getenv("HAUNT_WIZARD_TEST_BINARY")
	if databaseURL == "" || binary == "" {
		t.Skip("explicit first-run binary/database fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	db, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal("could not connect to dedicated first-run fixture")
	}
	defer db.Close(context.Background())
	var dbName string
	if err = db.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	if dbName != "haunt_sync_iteration" && !strings.HasPrefix(dbName, "haunt_wizard_test_") {
		t.Fatal("refusing database outside the dedicated test namespace")
	}
	var schema *string
	if err = db.QueryRow(ctx, `SELECT to_regnamespace('chatxo')::text`).Scan(&schema); err != nil || schema != nil {
		t.Fatal("first-run fixture requires an empty application schema")
	}
	defer func() {
		var id string
		if err := db.QueryRow(context.Background(), `SELECT id FROM chatxo.instances WHERE singleton`).Scan(&id); err == nil && id != "" {
			_, _ = db.Exec(context.Background(), `DROP SCHEMA chatxo CASCADE`)
		}
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	base := "http://" + address
	dir := t.TempDir()
	password := "Owner password chosen in rendered setup 59374"
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HAUNT_") {
			env = append(env, entry)
		}
	}
	env = append(env, "HAUNT_DATA_DIR="+dir, "HAUNT_LISTEN_ADDR="+address)
	var command *exec.Cmd
	var logPath string
	var logFile *os.File
	start := func(extra ...string) {
		logFile, err = os.CreateTemp(dir, "process-log-*")
		if err != nil {
			t.Fatal(err)
		}
		logPath = logFile.Name()
		command = exec.CommandContext(ctx, binary)
		command.Env = append(append([]string{}, env...), extra...)
		command.Stdout = logFile
		command.Stderr = logFile
		if err = command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	stop := func() {
		if command == nil {
			return
		}
		_ = command.Process.Signal(syscall.SIGTERM)
		err := command.Wait()
		_ = logFile.Close()
		command = nil
		logs, _ := os.ReadFile(logPath)
		if err != nil || bytes.Contains(logs, []byte("DATA RACE")) {
			t.Fatalf("server did not exit cleanly: %v", err)
		}
		if bytes.Contains(logs, []byte(password)) {
			t.Fatal("owner password appeared in logs")
		}
	}
	defer func() {
		if command != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			_ = logFile.Close()
		}
	}()
	request := func(method, path, token, origin string, value any) (int, map[string]any) {
		var body bytes.Buffer
		if value != nil {
			_ = json.NewEncoder(&body).Encode(value)
		}
		req, _ := http.NewRequestWithContext(ctx, method, base+path, &body)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer res.Body.Close()
		var result map[string]any
		_ = json.NewDecoder(res.Body).Decode(&result)
		return res.StatusCode, result
	}
	wait := func(path string) {
		t.Helper()
		for i := 0; i < 200; i++ {
			code, _ := request("GET", path, "", "", nil)
			if code == 200 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("production HTTP listener did not become ready")
	}
	start()
	wait("/readyz")
	competitor := exec.CommandContext(ctx, binary)
	competitor.Env = append(append([]string{}, env...), "HAUNT_LISTEN_ADDR=127.0.0.1:8089")
	output, competeErr := competitor.CombinedOutput()
	if competeErr == nil || !bytes.Contains(output, []byte("another setup process owns this data directory")) {
		t.Fatal("competing setup could alter installation state")
	}
	firstState, err := os.ReadFile(filepath.Join(dir, "configuration.json"))
	if err != nil {
		t.Fatal(err)
	}
	firstToken, err := os.ReadFile(filepath.Join(dir, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	stop()
	start()
	wait("/readyz")
	resumedState, _ := os.ReadFile(filepath.Join(dir, "configuration.json"))
	resumedToken, _ := os.ReadFile(filepath.Join(dir, "setup-token"))
	if !bytes.Equal(firstState, resumedState) || !bytes.Equal(firstToken, resumedToken) {
		t.Fatal("interrupted setup replaced identity or claim")
	}
	_, health := request("GET", "/readyz", "", "", nil)
	if health["status"] != "setup" {
		t.Fatal("fresh binary bypassed setup")
	}
	logs, _ := os.ReadFile(logPath)
	match := regexp.MustCompile(`"setup_url":"[^"#]+#([A-Za-z0-9_-]+)"`).FindSubmatch(logs)
	if len(match) != 2 {
		t.Fatal("private startup link not emitted")
	}
	token := string(match[1])
	if code, _ := request("GET", "/setup/config", "", "", nil); code != 401 {
		t.Fatal("setup without claim accepted")
	}
	form := map[string]any{"values": map[string]string{"HAUNT_DATABASE_URL": databaseURL, "HAUNT_PUBLIC_URL": base, "HAUNT_SESSION_DAYS": "0"}, "username": "owner", "password": password}
	if code, _ := request("POST", "/setup/complete", token, "https://other.example", form); code != 403 {
		t.Fatal("cross-origin claim accepted")
	}
	if code, _ := request("POST", "/setup/complete", token, base, form); code != 400 {
		t.Fatal("invalid settings silently accepted")
	}
	// Drive the actual served UI when requested; CI can also verify the exact
	// production HTTP path without requiring a local display/browser installation.
	if os.Getenv("HAUNT_WIZARD_TEST_UI") == "1" {
		root, err := filepath.Abs("../..")
		if err != nil {
			t.Fatal(err)
		}
		ui := exec.CommandContext(ctx, "node", filepath.Join(root, "scripts/test-wizard-ui.mjs"))
		ui.Env = append(os.Environ(), "HAUNT_WIZARD_URL="+base, "HAUNT_WIZARD_TOKEN="+token, "HAUNT_WIZARD_PASSWORD="+password, "HAUNT_WIZARD_DATABASE_URL="+databaseURL, "HAUNT_WIZARD_SCREENSHOTS="+filepath.Join(root, "var/wizard-ui/standalone"))
		output, err := ui.CombinedOutput()
		if err != nil {
			if path := os.Getenv("HAUNT_WIZARD_TEST_LOG_PATH"); path != "" {
				logs, _ := os.ReadFile(logPath)
				_ = os.WriteFile(path, logs, 0600)
			}
			t.Fatalf("rendered standalone wizard: %v\n%s", err, output)
		}
		t.Log(strings.TrimSpace(string(output)))
	} else {
		form["values"] = map[string]string{"HAUNT_DATABASE_URL": databaseURL, "HAUNT_PUBLIC_URL": base, "HAUNT_NAME": "Wizard community", "HAUNT_TEXT_RETENTION_DAYS": "44"}
		if code, _ := request("POST", "/setup/complete", token, base, form); code != 202 {
			t.Fatal("production setup submission failed", code)
		}
	}
	wait("/api/v1/instance")
	_, instance := request("GET", "/api/v1/instance", "", "", nil)
	if instance["name"] != "Wizard community" || instance["public_url"] != base {
		t.Fatal("wizard choices did not reach actual discovery")
	}
	id := instance["instance_id"]
	login := map[string]string{"username": "owner", "password": password, "device_name": "wizard test"}
	if code, _ := request("POST", "/api/v1/auth/login", "", "", login); code != 200 {
		t.Fatal("chosen password cannot log in")
	}
	if code, _ := request("POST", "/setup/complete", token, "", form); code != 404 {
		t.Fatal("setup remained enabled after owner creation")
	}
	saved, err := os.ReadFile(filepath.Join(dir, "configuration.json"))
	if err != nil || bytes.Contains(saved, []byte(password)) || bytes.Contains(saved, []byte("HAUNT_BOOTSTRAP_PASSWORD")) {
		t.Fatal("configuration contains owner password")
	}
	info, _ := os.Stat(filepath.Join(dir, "configuration.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("configuration permissions")
	}
	if _, err = os.Stat(filepath.Join(dir, "setup-token")); !os.IsNotExist(err) {
		t.Fatal("setup claim not removed")
	}
	stop()
	start("HAUNT_BOOTSTRAP_USERNAME=owner", "HAUNT_BOOTSTRAP_PASSWORD=Unrequested replacement password")
	wait("/api/v1/instance")
	_, restored := request("GET", "/api/v1/instance", "", "", nil)
	if restored["instance_id"] != id || restored["name"] != instance["name"] {
		t.Fatal("restart lost saved identity/settings")
	}
	if code, _ := request("POST", "/api/v1/auth/login", "", "", login); code != 200 {
		t.Fatal("existing owner password was replaced")
	}
	login["password"] = "Unrequested replacement password"
	if code, _ := request("POST", "/api/v1/auth/login", "", "", login); code != 401 {
		t.Fatal("replacement bootstrap password became effective")
	}
	stop()
	logs, _ = os.ReadFile(logPath)
	if bytes.Contains(logs, []byte("setup_url")) {
		t.Fatal("existing owner reopened setup")
	}
	t.Log(fmt.Sprintf("PASS: production binary same-port setup, chosen owner password, private persistence, identity preservation and existing-owner bypass on restart"))
}
