package tools

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/example-git/haunt-server/internal/ai/provider"
)

func TestLivePublicTools(t *testing.T) {
	if os.Getenv("HAUNT_AI_LIVE_TOOLS") != "1" {
		t.Skip("set HAUNT_AI_LIVE_TOOLS=1 to exercise external public sources")
	}
	r := New()
	for _, tc := range []struct{ name, args string }{{"read_url", `{"url":"https://example.org","max_chars":1024}`}, {"web_search", `{"query":"Go programming language official","max_results":3}`}} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := r.Execute(t.Context(), provider.ToolCall{ID: "live-check", Name: tc.name, Arguments: json.RawMessage(tc.args)})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError || result.Content == "" {
				t.Fatalf("empty or failed result: %+v", result)
			}
			t.Log(result.Content)
		})
	}
}
