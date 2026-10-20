package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/pion/webrtc/v4"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

var contractOnce sync.Once
var contractErr error
var responseSchemas map[string]*jsonschema.Schema
var contractMux *http.ServeMux

func initializeContractValidation() {
	docs, err := GenerateDocuments()
	if err != nil {
		contractErr = err
		return
	}
	var spec map[string]any
	if err = json.Unmarshal(docs.OpenAPI, &spec); err != nil {
		contractErr = err
		return
	}
	compiler := jsonschema.NewCompiler()
	const base = "https://chatxo.invalid/openapi.json"
	if err = compiler.AddResource(base, spec); err != nil {
		contractErr = err
		return
	}
	responseSchemas = map[string]*jsonschema.Schema{}
	contractMux = http.NewServeMux()
	for _, pattern := range registeredRoutes() {
		contractMux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	}
	for path, item := range spec["paths"].(map[string]any) {
		for method, raw := range item.(map[string]any) {
			op := raw.(map[string]any)
			for status, raw := range op["responses"].(map[string]any) {
				response := raw.(map[string]any)
				content, ok := response["content"].(map[string]any)
				if !ok {
					continue
				}
				if _, ok = content["application/json"]; !ok {
					continue
				}
				pointer := base + "#/paths/" + strings.ReplaceAll(path, "/", "~1") + "/" + method + "/responses/" + status + "/content/application~1json/schema"
				schema, err := compiler.Compile(pointer)
				if err != nil {
					contractErr = err
					return
				}
				responseSchemas[strings.ToUpper(method)+" "+path+" "+status] = schema
			}
		}
	}
}
func validateDocumentedResponse(t *testing.T, method, path string, status int, data []byte) {
	t.Helper()
	if len(data) == 0 || method == "HEAD" {
		return
	}
	contractOnce.Do(initializeContractValidation)
	if contractErr != nil {
		t.Fatal(contractErr)
	}
	request := httptest.NewRequest(method, path, nil)
	_, pattern := contractMux.Handler(request)
	if pattern == "" {
		return
	} // Explicit tests of nonexistent URLs are not API operations.
	schema := responseSchemas[pattern+" "+fmt.Sprint(status)]
	if schema == nil {
		schema = responseSchemas[pattern+" default"]
	}
	if schema == nil {
		return
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s response is not JSON", pattern)
	}
	if err = schema.Validate(value); err != nil {
		t.Fatalf("%s status %d disagrees with generated response schema (payload omitted)", pattern, status)
	}
}
func TestGeneratedAPIDocuments(t *testing.T) {
	docs, err := GenerateDocuments()
	if err != nil {
		t.Fatal(err)
	}
	again, err := GenerateDocuments()
	if err != nil || !bytes.Equal(docs.OpenAPI, again.OpenAPI) || !bytes.Equal(docs.WebSocket, again.WebSocket) {
		t.Fatal("documentation generation is not deterministic", err)
	}
	for name, data := range map[string][]byte{"openapi.json": docs.OpenAPI, "websocket.json": docs.WebSocket, "endpoints.md": docs.Endpoints} {
		stored, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
		if err != nil || !bytes.Equal(stored, data) {
			t.Fatalf("%s is stale; run go generate ./internal/httpapi", name)
		}
	}
	var spec map[string]any
	if err = json.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	const base = "https://chatxo.invalid/openapi.json"
	if err = compiler.AddResource(base, spec); err != nil {
		t.Fatal(err)
	}
	emojiOperation := spec["paths"].(map[string]any)["/api/v1/emojis/{name}"].(map[string]any)["put"].(map[string]any)
	emojiName := emojiOperation["parameters"].([]any)[0].(map[string]any)["schema"].(map[string]any)
	if emojiName["format"] == "uuid" || emojiName["pattern"] != "^[a-z0-9_]{2,32}$" {
		t.Fatal("emoji name documented as an ID instead of a colon name")
	}
	// Resolve and compile every generated component, catching dangling refs and
	// invalid schema constraints without external schema/network downloads.
	for name := range spec["components"].(map[string]any)["schemas"].(map[string]any) {
		if _, err = compiler.Compile(base + "#/components/schemas/" + name); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{"HttpapiSaveRoleRequest", map[string]any{"name": "Admin", "permissions": 32768}, true},
		{"HttpapiSaveRoleRequest", map[string]any{"name": "All", "permissions": 65535}, true},
		{"HttpapiSaveRoleRequest", map[string]any{"name": "Invalid", "permissions": 65536}, false},
		{"HttpapiDisableMemberRequest", map[string]any{"disabled": false}, true},
		{"HttpapiDisableMemberRequest", map[string]any{}, false},
		{"HttpapiDisableMemberRequest", map[string]any{"disabled": nil}, false},
		{"HttpapiCreateMessageRequest", map[string]any{"body": "hello"}, false},
		{"HttpapiCreateMessageRequest", map[string]any{"request_id": "a", "body": "hello", "unexpected": true}, false},
		{"V4RTPCodecCapability", webrtc.RTPCodecCapability{MimeType: "video/VP9", ClockRate: 90000}, true},
		{"V4RTPCodecCapability", map[string]any{}, false},
		{"V4SessionDescription", webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"}, true},
	} {
		schema, err := compiler.Compile(base + "#/components/schemas/" + tc.name)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(tc.value)
		value, _ := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if valid := schema.Validate(value) == nil; valid != tc.valid {
			t.Fatalf("%s wire representation does not match schema", tc.name)
		}
	}
	paths := spec["paths"].(map[string]any)
	if _, ok := paths["/__schema/eventCommand"]; ok {
		t.Fatal("schema helper leaked as an HTTP endpoint")
	}
	upgrade := paths["/api/v1/events"].(map[string]any)["get"].(map[string]any)
	if _, ok := upgrade["security"]; ok {
		t.Fatal("WebSocket docs incorrectly require HTTP bearer headers")
	}
	if _, ok := upgrade["responses"].(map[string]any)["101"]; !ok {
		t.Fatal("WebSocket upgrade missing")
	}
}
func TestDocumentationServedWithoutDatabase(t *testing.T) {
	server := httptest.NewServer(New(config.Config{}, nil, nil).Handler)
	defer server.Close()
	for _, tc := range []struct{ path, contains string }{{"/openapi.json", `"openapi": "3.1.0"`}, {"/websocket.json", "chatxo-encoded-v1"}, {"/endpoints.md", "/api/v1/conversations"}, {"/docs/", "swagger-ui"}} {
		response, err := server.Client().Get(server.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || !strings.Contains(string(data), tc.contains) {
			t.Fatalf("%s documentation unavailable", tc.path)
		}
	}
}
