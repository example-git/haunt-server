package antigravity

import "github.com/example-git/haunt-server/internal/ai/provider"

var catalog = []provider.Model{
	{ID: "claude-opus-4-6-thinking", Name: "Claude Opus 4.6 (Thinking)", Vision: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6 (Thinking)", Vision: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash", Vision: true, ContextWindow: 1048576, MaxOutputTokens: 65536},
	{ID: "gemini-3.1-flash-lite", Name: "Gemini 3.1 Flash Lite", Vision: true, ContextWindow: 1048576, MaxOutputTokens: 65535},
	{ID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash", Vision: true, ContextWindow: 1048576, MaxOutputTokens: 65536},
	{ID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B (Medium)", Vision: false, ContextWindow: 114000, MaxOutputTokens: 32768},
	{ID: "claude-opus-5-5-high", Name: "Claude Opus 5.5 (High)", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-sonnet-5-5-high", Name: "Claude Sonnet 5.5 (High)", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "gemini-4-argon-high", Name: "Gemini 4 Argon", Vision: true, ContextWindow: 1048576, MaxOutputTokens: 1048576},
	{ID: "gemini-3.5-flash-lite", Name: "Gemini 3.5 Flash Lite", Vision: true, ContextWindow: 1048576, MaxOutputTokens: 65535},
}
