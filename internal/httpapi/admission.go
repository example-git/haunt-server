package httpapi

import (
	"container/list"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/platform/postgres"
)

var errCapacity = errors.New("request capacity reached")

type requestQueue struct {
	mu       sync.Mutex
	active   int
	limit    int
	queued   int
	wait     time.Duration
	waiting  list.List
	admitted uint64
	rejected uint64
	timedOut uint64
}

func (q *requestQueue) acquire(ctx context.Context) error {
	q.mu.Lock()
	if err := ctx.Err(); err != nil {
		q.mu.Unlock()
		return err
	}
	if q.active < q.limit {
		q.active++
		q.admitted++
		q.mu.Unlock()
		return nil
	}
	if q.waiting.Len() >= q.queued {
		q.rejected++
		q.mu.Unlock()
		return errCapacity
	}
	ready := make(chan struct{})
	element := q.waiting.PushBack(ready)
	q.mu.Unlock()
	timer := time.NewTimer(q.wait)
	defer timer.Stop()
	var err error
	select {
	case <-ready:
		if err = ctx.Err(); err == nil {
			return nil
		}
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errCapacity
	}
	q.mu.Lock()
	select {
	case <-ready:
		q.releaseLocked()
	default:
		q.waiting.Remove(element)
	}
	if err == errCapacity {
		q.timedOut++
	}
	q.mu.Unlock()
	return err
}

func (q *requestQueue) releaseLocked() {
	if first := q.waiting.Front(); first != nil {
		q.waiting.Remove(first)
		q.admitted++
		close(first.Value.(chan struct{}))
	} else {
		q.active--
	}
}

func (q *requestQueue) release() {
	q.mu.Lock()
	q.releaseLocked()
	q.mu.Unlock()
}

func (q *requestQueue) stats() map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	return map[string]any{"active": q.active, "queued": q.waiting.Len(), "active_limit": q.limit, "queue_limit": q.queued, "admitted": q.admitted, "rejected": q.rejected, "queue_timeouts": q.timedOut}
}

type admissionController map[config.Workload]*requestQueue

func newAdmission(c config.Config) admissionController {
	a := admissionController{}
	for workload, limits := range c.WorkloadCapacities() {
		a[workload] = &requestQueue{limit: limits.Active, queued: limits.Queued, wait: c.QueueWait()}
	}
	return a
}

func requestWorkload(pattern string) config.Workload {
	method, path, _ := strings.Cut(pattern, " ")
	switch {
	case path == "/api/v1/events", path == "/api/v1/media", strings.HasPrefix(path, "/api/v1/voice/"):
		return config.Realtime
	case path == "/api/v1/admin/maintenance", path == "/api/v1/me/dm-storage/purge":
		return config.Maintenance
	case strings.HasSuffix(path, "/embeds/video"), strings.HasSuffix(path, "/embeds/image"):
		return config.Media
	case strings.Contains(path, "/embeds"), strings.HasSuffix(path, "/attachments/{id}/metadata"):
		return config.Embeds
	case strings.Contains(path, "/attachments"), strings.Contains(path, "/audio-plugins"), strings.HasSuffix(path, "/image"), strings.HasSuffix(path, "/avatar"), strings.HasSuffix(path, "/icon"), strings.HasSuffix(path, "/banner"), strings.HasSuffix(path, "/speech"), strings.HasPrefix(path, "/api/v1/client-updates/{version}/"):
		return config.Media
	case strings.HasPrefix(path, "/api/v1/conversations/{id}/messages"), strings.HasPrefix(path, "/api/v1/conversations/{id}/encryption"), path == "/api/v1/read-state":
		if method == "GET" {
			return config.History
		}
		return config.Messages
	case path == "/api/v1/conversations/{id}/read", path == "/api/v1/dms":
		return config.Messages
	default:
		return config.Control
	}
}

func (a admissionController) wrap(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		workload := requestWorkload(pattern)
		r = r.WithContext(postgres.WithWorkload(r.Context(), workload))
		if pattern == "GET /healthz" || pattern == "GET /api/v1/events" || pattern == "GET /api/v1/media" {
			next.ServeHTTP(w, r)
			return
		}
		queue := a[workload]
		if err := queue.acquire(r.Context()); err != nil {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "request_capacity_reached"})
			return
		}
		defer queue.release()
		next.ServeHTTP(w, r)
	})
}

func (a admissionController) stats() map[string]any {
	stats := map[string]any{}
	for workload, queue := range a {
		stats[string(workload)] = queue.stats()
	}
	return stats
}
