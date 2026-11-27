package tools

import (
	"context"
	"testing"
)

func TestFindReusesExactTurnPageAndRuneOffsets(t *testing.T) {
	const url = "https://example.invalid/gone"
	var memory PageMemory
	memory.remember(url, textSnapshot{url, "🎨 café CAFÉ"})
	r := New()
	result, _, err := r.readURL(WithPageMemory(context.Background(), &memory), readParams{URL: url, Find: "café", Offset: 3})
	if err != nil {
		t.Fatal(err)
	}
	matches := result.(map[string]any)["matches"].([]passage)
	if len(matches) != 1 || matches[0].Offset != 7 || matches[0].End != 11 {
		t.Fatal(matches)
	}
}
