package providers

import (
	"github.com/example-git/haunt-server/internal/ai/clientidentity"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/ai/providers/antigravity"
	"github.com/example-git/haunt-server/internal/ai/providers/claude"
	"github.com/example-git/haunt-server/internal/ai/providers/codex"
	"net/http"
)

func New(identity *clientidentity.Manager, httpClient *http.Client) map[provider.ID]provider.Adapter {
	return map[provider.ID]provider.Adapter{provider.Claude: claude.New(identity, httpClient), provider.Codex: codex.New(identity, httpClient), provider.Antigravity: antigravity.New(identity, httpClient)}
}
