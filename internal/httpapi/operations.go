package httpapi

import (
	"context"
	"github.com/example-git/haunt-server/internal/identity"
	"net/http"
	"time"
)

func (f *Features) operations(a *api, mux routeRegistrar) {
	mux.HandleFunc("GET /api/v1/admin/stats", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !p.Can(identity.ManageConfiguration) {
			a.fail(w, r, identity.ErrForbidden)
			return
		}
		var text, attachments, events, messages, files, dbBytes, walBytes int64
		err := f.Chat.Pool.QueryRow(r.Context(), `SELECT s.text_bytes,s.attachment_bytes,(SELECT count(*) FROM chatxo.events),(SELECT count(*) FROM chatxo.messages),(SELECT count(*) FROM chatxo.attachments),pg_database_size(current_database()),COALESCE((SELECT wal_bytes FROM pg_stat_wal),0)::bigint FROM chatxo.storage s WHERE singleton`).Scan(&text, &attachments, &events, &messages, &files, &dbBytes, &walBytes)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		pool := f.Chat.Pool.Stat()
		rows, err := f.Chat.Pool.Query(r.Context(), `SELECT relname,pg_total_relation_size(relid),n_dead_tup FROM pg_stat_user_tables WHERE schemaname='chatxo' ORDER BY relname`)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		relations := []map[string]any{}
		for rows.Next() {
			var name string
			var size, dead int64
			if err := rows.Scan(&name, &size, &dead); err != nil {
				rows.Close()
				a.fail(w, r, err)
				return
			}
			relations = append(relations, map[string]any{"name": name, "bytes": size, "dead_tuples": dead})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, statsResponse{Storage: map[string]any{"retained_text_bytes": text, "attachment_reserved_bytes": attachments, "messages": messages, "events": events, "attachments": files, "database_bytes": dbBytes, "wal_bytes_since_reset": walBytes, "relations": relations}, Policy: f.Chat.CurrentPolicy(), Pool: map[string]any{"total_connections": pool.TotalConns(), "acquired_connections": pool.AcquiredConns(), "idle_connections": pool.IdleConns(), "acquire_count": pool.AcquireCount(), "empty_acquire_count": pool.EmptyAcquireCount(), "acquire_duration_ns": pool.AcquireDuration().Nanoseconds()}, StorageLock: f.Chat.LockStats(), Events: f.Gateway.stats(), Media: f.Voice.Stats(), Relay: f.Relay.Stats(), CloudflareTURN: f.CloudTURN.Stats()})
	}))
	mux.HandleFunc("POST /api/v1/admin/maintenance", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !p.Can(identity.ManageConfiguration) {
			a.fail(w, r, identity.ErrForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if err := f.Maintain(ctx); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := a.identity.Cleanup(ctx); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
}
