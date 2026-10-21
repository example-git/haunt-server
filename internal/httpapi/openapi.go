package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/voice"
	"github.com/pion/webrtc/v4"
	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/openapi31"
	"github.com/swaggest/swgui"
	swaggerui "github.com/swaggest/swgui/v5"
)

//go:generate go run ../../cmd/apidocs -out ../../docs -client-out ../../client/static/api

type routeRegistrar interface {
	HandleFunc(string, func(http.ResponseWriter, *http.Request))
}
type routeList []string

func (r *routeList) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	*r = append(*r, pattern)
}
func registeredRoutes() []string {
	var routes routeList
	a := &api{}
	a.routes(&routes)
	a.clientUpdateRoutes(&routes, config.Config{})
	(&Features{}).routes(a, &routes)
	publicRoutes(&routes, config.Config{}, nil, true)
	sort.Strings(routes)
	return routes
}

type APIDocuments struct{ OpenAPI, WebSocket, Endpoints []byte }

// GenerateDocuments enumerates actual route registrations and reflects the
// shared wire types. It never loads configuration, opens a database or listens.
func GenerateDocuments() (APIDocuments, error) {
	contracts := endpointContracts()
	reflector := openapi31.NewReflector()
	reflector.DefaultOptions = append(reflector.DefaultOptions, jsonschema.InterceptSchema(func(p jsonschema.InterceptSchemaParams) (bool, error) {
		if p.Value.IsValid() && p.Value.Type() == reflect.TypeOf(webrtc.SDPTypeOffer) {
			*p.Schema = jsonschema.Schema{}
			p.Schema.AddType(jsonschema.String)
			p.Schema.WithEnum("offer", "answer", "pranswer", "rollback")
			return true, nil
		}
		// Pion codec structs use encoding/json's exported field names without
		// tags. Reflect them explicitly so client SDKs receive codec metadata.
		if p.Value.IsValid() && p.Value.Type() == reflect.TypeOf(webrtc.RTPCodecCapability{}) {
			r := jsonschema.Reflector{}
			schema, err := r.Reflect(webrtc.RTPCodecCapability{}, jsonschema.ProcessWithoutTags, jsonschema.InlineRefs)
			if err != nil {
				return false, err
			}
			for name := range schema.Properties {
				schema.Required = append(schema.Required, name)
			}
			sort.Strings(schema.Required)
			*p.Schema = schema
			return true, nil
		}
		if p.Processed && p.Value.IsValid() && p.Value.Kind() == reflect.Struct {
			t := p.Value.Type()
			if strings.HasSuffix(t.Name(), "Request") || t.Name() == "MediaCommand" || t.Name() == "MediaAuthenticateCommand" {
				p.Schema.AdditionalProperties = (&jsonschema.SchemaOrBool{}).WithTypeBoolean(false)
			} else if !strings.Contains(t.Name(), "Command") && t.Name() != "EncodedTrack" {
				// JSON response fields without omitempty are always emitted. Keep
				// client generators from marking every response field optional.
				for _, field := range reflect.VisibleFields(t) {
					name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
					if _, present := p.Schema.Properties[name]; present && name != "" && name != "-" && !strings.Contains(options, "omitempty") {
						found := false
						for _, required := range p.Schema.Required {
							found = found || required == name
						}
						if !found {
							p.Schema.Required = append(p.Schema.Required, name)
						}
					}
				}
			}
		}
		return false, nil
	}))
	spec := reflector.SpecEns()
	spec.Info.WithTitle("Haunt API").WithVersion("v1").WithDescription("Installation-scoped chat API. Use instance discovery for active capabilities and limits. Authenticate HTTP requests with the opaque session bearer token. WebSockets authenticate in their first frame. See /websocket.json for message schemas and docs/api.md for ordering, retention and recovery semantics.")
	spec.SetHTTPBearerTokenSecurity("session", "", "Opaque device-session token returned by login or registration; not a JWT.")
	var markdown strings.Builder
	markdown.WriteString("# Generated HTTP endpoints\n\nGenerated from actual route registration and shared Go wire types using swaggest/openapi-go. Regenerate with `go generate ./internal/httpapi`.\n\nInteractive reference: `/docs/`. Specifications: `/openapi.json` and `/websocket.json`. See [API behavior](api.md) and [media protocol](media-transports.md).\n\n| Method | Path | Operation | Authentication | Success |\n| --- | --- | --- | --- | --- |\n")
	seen := map[string]bool{}
	for _, pattern := range registeredRoutes() {
		contract, ok := contracts[pattern]
		if !ok {
			return APIDocuments{}, fmt.Errorf("route lacks an API contract: %s", pattern)
		}
		if seen[pattern] {
			return APIDocuments{}, fmt.Errorf("duplicate route: %s", pattern)
		}
		seen[pattern] = true
		method, path, _ := strings.Cut(pattern, " ")
		oc, err := reflector.NewOperationContext(method, path)
		if err != nil {
			return APIDocuments{}, err
		}
		oc.SetID(contract.id)
		oc.SetSummary(contract.summary)
		oc.SetDescription(contract.description)
		tag := "operations"
		parts := strings.Split(strings.TrimPrefix(path, "/api/v1/"), "/")
		if len(parts) > 0 {
			tag = parts[0]
		}
		oc.SetTags(tag)
		auth := "Bearer"
		if !contract.public {
			oc.AddSecurity("session")
		} else {
			auth = "Public"
		}
		if contract.websocket {
			auth = "First WebSocket frame"
		}
		if strings.HasPrefix(path, "/api/v1/client-updates/") {
			auth = "Public feed / Bearer local"
		}
		var fields []reflect.StructField
		for _, part := range strings.Split(path, "/") {
			if strings.HasPrefix(part, "{") {
				name := strings.Trim(part, "{}")
				tag := reflect.StructTag(`path:"` + name + `" format:"uuid"`)
				if name == "id" && strings.Contains(path, "/server-extensions/") {
					tag = reflect.StructTag(`path:"id" pattern:"^[a-z][a-z0-9-]{0,63}$"`)
				}
				if name == "name" {
					tag = reflect.StructTag(`path:"name" pattern:"^[a-z0-9_]{2,32}$"`)
				}
				fields = append(fields, reflect.StructField{Name: strings.ToUpper(name[:1]) + name[1:], Type: reflect.TypeOf(""), Tag: tag})
			}
		}
		if len(fields) > 0 {
			oc.AddReqStructure(reflect.New(reflect.StructOf(fields)).Interface())
		}
		if contract.input != nil {
			oc.AddReqStructure(contract.input)
		}
		if contract.query != nil {
			oc.AddReqStructure(contract.query)
		}
		oc.AddRespStructure(contract.output, openapi.WithHTTPStatus(contract.status))
		if contract.id == "readiness" {
			oc.AddRespStructure(unavailableResponse{}, openapi.WithHTTPStatus(503))
		}
		oc.AddRespStructure(errorResponse{}, func(cu *openapi.ContentUnit) {
			cu.IsDefault = true
			cu.Description = "Error; inspect the stable error code. Common statuses: 400 invalid request, 401 invalid session, 403 denied, 404 missing/expired, 409 conflict, 413 oversized, 429 rate limit, 503 capacity/provider unavailable, 507 attachment capacity."
		})
		if err := reflector.AddOperation(oc); err != nil {
			return APIDocuments{}, fmt.Errorf("%s: %w", pattern, err)
		}
		fmt.Fprintf(&markdown, "| %s | `%s` | %s | %s | %d |\n", method, path, contract.summary, auth, contract.status)
	}
	for pattern := range contracts {
		if !seen[pattern] {
			return APIDocuments{}, fmt.Errorf("documented route is not registered: %s", pattern)
		}
	}
	// Reflect WebSocket payloads with the same library, then move them out of
	// temporary schema-only operations. They are not fictitious HTTP endpoints.
	frames := map[string]any{"eventCommand": eventCommand{}, "mediaAuthenticate": MediaAuthenticateCommand{}, "mediaCommand": MediaCommand{}, "participant": voice.Snapshot{}, "presenceMember": voice.PresenceMember{}, "encodedTrack": voice.EncodedTrack{}, "durableEvent": chat.Event{}, "readState": chat.ReadState{}}
	names := make([]string, 0, len(frames))
	for name := range frames {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		oc, _ := reflector.NewOperationContext("POST", "/__schema/"+name)
		oc.AddReqStructure(frames[name])
		oc.AddRespStructure(nil, openapi.WithHTTPStatus(204))
		if err := reflector.AddOperation(oc); err != nil {
			return APIDocuments{}, err
		}
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return APIDocuments{}, err
	}
	var doc map[string]any
	if err = json.Unmarshal(data, &doc); err != nil {
		return APIDocuments{}, err
	}
	paths := doc["paths"].(map[string]any)
	schemas := map[string]any{}
	for _, name := range names {
		item := paths["/__schema/"+name].(map[string]any)["post"].(map[string]any)
		schemas[name] = item["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
		delete(paths, "/__schema/"+name)
	}
	for pattern, contract := range contracts {
		method, path, _ := strings.Cut(pattern, " ")
		op := paths[path].(map[string]any)[strings.ToLower(method)].(map[string]any)
		if contract.input != nil {
			if body, ok := op["requestBody"].(map[string]any); ok {
				body["required"] = true
			}
		}
		if contract.id == "register" {
			media := op["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
			media["schema"] = map[string]any{"allOf": []any{media["schema"], map[string]any{"required": []string{"invitation"}, "properties": map[string]any{"invitation": map[string]any{"type": "string", "minLength": 1}, "password": map[string]any{"minLength": 8}}}}}
		}
		responses := op["responses"].(map[string]any)
		if contract.output == nil {
			responses[fmt.Sprint(contract.status)] = map[string]any{"description": http.StatusText(contract.status)}
		}
		if contract.binaryInput {
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}}
		}
		if contract.binaryOutput {
			body := map[string]any{"description": "Authorized file bytes. Content-Type is detected from content.", "content": map[string]any{"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}}
			responses["200"] = body
			responses["206"] = body
			responses["416"] = map[string]any{"description": "Unsatisfiable byte range"}
		}
	}
	// GET attachments also serves HEAD through net/http's GET pattern.
	get := paths["/api/v1/attachments/{id}"].(map[string]any)["get"]
	copied, _ := json.Marshal(get)
	var head map[string]any
	_ = json.Unmarshal(copied, &head)
	head["operationId"] = "headAttachment"
	head["summary"] = "Read authorized attachment headers"
	head["responses"] = map[string]any{"200": map[string]any{"description": "Attachment metadata headers, no body"}, "default": head["responses"].(map[string]any)["default"]}
	paths["/api/v1/attachments/{id}"].(map[string]any)["head"] = head
	markdown.WriteString("| HEAD | `/api/v1/attachments/{id}` | Attachment headers | Bearer | 200 |\n\nGET routes also accept HEAD through Go's ServeMux. OPTIONS is handled by the origin/CORS boundary. Developer documentation is served at `/docs/`, `/openapi.json`, `/websocket.json`, and `/endpoints.md`.\n")
	ws := webSocketDocument(schemas, doc["components"])
	doc["x-websocket-document"] = "/websocket.json"
	openAPI, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return APIDocuments{}, err
	}
	webSocket, err := json.MarshalIndent(ws, "", "  ")
	if err != nil {
		return APIDocuments{}, err
	}
	return APIDocuments{OpenAPI: append(openAPI, '\n'), WebSocket: append(webSocket, '\n'), Endpoints: []byte(markdown.String())}, nil
}

var docsOnce sync.Once
var cachedDocuments APIDocuments

func serveDocumentation(mux *http.ServeMux) {
	docsOnce.Do(func() {
		var err error
		cachedDocuments, err = GenerateDocuments()
		if err != nil {
			panic(err)
		}
	})
	for _, item := range []struct {
		path, contentType string
		data              []byte
	}{{"/openapi.json", "application/json", cachedDocuments.OpenAPI}, {"/websocket.json", "application/json", cachedDocuments.WebSocket}, {"/endpoints.md", "text/markdown; charset=utf-8", cachedDocuments.Endpoints}} {
		mux.HandleFunc("GET "+item.path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", item.contentType)
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(item.data)
		})
	}
	ui := swaggerui.NewHandlerWithConfig(swgui.Config{Title: "Haunt API", SwaggerJSON: "/openapi.json", BasePath: "/docs/", SettingsUI: map[string]string{"validatorUrl": "null", "persistAuthorization": "false"}})
	mux.Handle("GET /docs/", ui)
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusTemporaryRedirect)
	})
}
