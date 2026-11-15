package codex

import "github.com/example-git/haunt-server/internal/ai/provider"

var catalog = []provider.Model{
	{ID: "gpt-5.6-luna", Name: "GPT-5.6-Luna", Vision: true, ContextWindow: 272000, MaxOutputTokens: 128000},
	{ID: "gpt-6-astra", Name: "GPT-6-Astra", Vision: true, ContextWindow: 272000, MaxOutputTokens: 128000},
	{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol", Vision: true, ContextWindow: 272000, MaxOutputTokens: 128000},
	{ID: "gpt-5.6-terra", Name: "GPT-5.6-Terra", Vision: true, ContextWindow: 272000, MaxOutputTokens: 128000},
	{ID: "gpt-6.1-sol", Name: "GPT-6.1-Sol", Vision: true, ContextWindow: 272000, MaxOutputTokens: 128000},
	{ID: "gpt-6-sol", Name: "GPT-6-Sol", Vision: true, ContextWindow: 272000, MaxOutputTokens: 128000},
	{ID: "gpt-6-luna", Name: "GPT-6-Luna", Vision: true, ContextWindow: 272000, MaxOutputTokens: 128000},
	{ID: "gpt-5.5", Name: "GPT-5.5", Vision: true, ContextWindow: 272000, MaxOutputTokens: 128000},
}
