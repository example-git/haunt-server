CREATE TABLE chatxo.embed_cache (key text PRIMARY KEY, payload jsonb NOT NULL, expires_at timestamptz NOT NULL);
CREATE INDEX embed_cache_expiry ON chatxo.embed_cache(expires_at);
