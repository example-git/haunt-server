package claude

import "github.com/example-git/haunt-server/internal/ai/provider"

var catalog = []provider.Model{
	{ID: "claude-fable-5-1", Name: "Claude Fable 5.1", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-fable-5", Name: "Claude Fable 5", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-opus-5-5", Name: "Claude Opus 5.5", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-haiku-5-5", Name: "Claude Haiku 5.5", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5 (latest)", Vision: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	{ID: "claude-haiku-4-5-20251001", Name: "Claude Haiku 4.5", Vision: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	{ID: "claude-opus-4-5", Name: "Claude Opus 4.5 (latest)", Vision: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	{ID: "claude-opus-4-5-20251101", Name: "Claude Opus 4.5", Vision: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	{ID: "claude-opus-4-6", Name: "Claude Opus 4.6", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-opus-4-7", Name: "Claude Opus 4.7", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-opus-4-8", Name: "Claude Opus 4.8", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-opus-5", Name: "Claude Opus 5", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5 (latest)", Vision: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	{ID: "claude-sonnet-4-5-20250929", Name: "Claude Sonnet 4.5", Vision: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", Vision: true, ContextWindow: 1000000, MaxOutputTokens: 128000},
}
