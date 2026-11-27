package ai

import (
	"encoding/json"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"strings"
	"testing"
)

func TestClockCalendarDayAcrossDST(t *testing.T) {
	result, err := clockTool(provider.ToolCall{Arguments: json.RawMessage(`{"at":"2026-03-07T12:00:00-05:00","timezone":"America/New_York","calendar_days":1}`)})
	if err != nil || result.IsError || !strings.Contains(result.Content, "2026-03-08T12:00:00-04:00") {
		t.Fatal(result, err)
	}
}
