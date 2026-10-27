package embeds

import (
	"context"
	"encoding/json"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/platform/postgres"
)

type pageCache struct{ pool postgres.Pool }
type storedPage struct {
	Preview   Preview
	ImageURL  string
	AvatarURL string
	MediaURLs [][2]string
}

func NewPersistent(pool postgres.Pool) *Service {
	s := New()
	s.cache = &pageCache{pool: pool}
	return s
}
func (c *pageCache) get(ctx context.Context, key string) (Preview, bool) {
	if c == nil {
		return Preview{}, false
	}
	ctx, cancel := context.WithTimeout(postgres.WithWorkload(ctx, config.Embeds), 500*time.Millisecond)
	defer cancel()
	var data []byte
	if c.pool.QueryRow(ctx, `SELECT payload FROM chatxo.embed_cache WHERE key=$1 AND expires_at>now()`, key).Scan(&data) != nil {
		return Preview{}, false
	}
	var stored storedPage
	if json.Unmarshal(data, &stored) != nil {
		return Preview{}, false
	}
	p := stored.Preview
	p.ImageURL = stored.ImageURL
	p.AvatarURL = stored.AvatarURL
	if len(p.Media) != len(stored.MediaURLs) {
		return Preview{}, false
	}
	for i, urls := range stored.MediaURLs {
		p.Media[i].URL = urls[0]
		p.Media[i].PosterURL = urls[1]
	}
	return p, true
}
func (c *pageCache) put(ctx context.Context, key string, p Preview) {
	if c == nil || p.Asset != nil {
		return
	}
	stored := storedPage{Preview: p, ImageURL: p.ImageURL, AvatarURL: p.AvatarURL}
	for _, m := range p.Media {
		stored.MediaURLs = append(stored.MediaURLs, [2]string{m.URL, m.PosterURL})
	}
	data, err := json.Marshal(stored)
	if err != nil || len(data) > 65536 {
		return
	}
	ctx, cancel := context.WithTimeout(postgres.WithWorkload(ctx, config.Embeds), 500*time.Millisecond)
	defer cancel()
	_, _ = c.pool.Exec(ctx, `INSERT INTO chatxo.embed_cache(key,payload,expires_at) VALUES($1,$2,now()+interval '15 minutes') ON CONFLICT(key) DO UPDATE SET payload=excluded.payload,expires_at=excluded.expires_at`, key, data)
	_, _ = c.pool.Exec(ctx, `DELETE FROM chatxo.embed_cache WHERE expires_at<=now() OR key IN (SELECT key FROM chatxo.embed_cache ORDER BY expires_at DESC OFFSET 2048)`)
}
