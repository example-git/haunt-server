package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/embeds"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func waitQueued(t *testing.T, q *requestQueue, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q.stats()["queued"] == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue did not reach %d: %v", count, q.stats())
}

func TestAdmissionFIFOOverflowCancellationAndTimeout(t *testing.T) {
	q := &requestQueue{limit: 1, queued: 2, wait: time.Second}
	ctx := context.Background()
	if err := q.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	first, second := make(chan error, 1), make(chan error, 1)
	canceled, cancel := context.WithCancel(ctx)
	go func() { first <- q.acquire(canceled) }()
	waitQueued(t, q, 1)
	go func() { second <- q.acquire(ctx) }()
	waitQueued(t, q, 2)
	if err := q.acquire(ctx); !errors.Is(err, errCapacity) {
		t.Fatal(err)
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitQueued(t, q, 1)
	q.release()
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	q.release()
	if q.stats()["active"] != 0 {
		t.Fatal(q.stats())
	}
	q.wait = 10 * time.Millisecond
	if err := q.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := q.acquire(ctx); !errors.Is(err, errCapacity) {
		t.Fatal(err)
	}
	waitQueued(t, q, 0)
	q.release()
	if q.stats()["queue_timeouts"] != uint64(1) {
		t.Fatal(q.stats())
	}
}

func TestAdmissionCancellationHandoff(t *testing.T) {
	q := &requestQueue{limit: 1, queued: 1, wait: time.Second}
	for range 100 {
		if err := q.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- q.acquire(ctx) }()
		waitQueued(t, q, 1)
		go cancel()
		q.release()
		if err := <-done; err == nil {
			q.release()
		}
		cancel()
		if q.stats()["active"] != 0 || q.stats()["queued"] != 0 {
			t.Fatal(q.stats())
		}
	}
}

func TestAdmissionHTTPQueueAndFeatureIsolation(t *testing.T) {
	c := config.Config{}
	c.Capacity = c.WorkloadCapacities()
	for workload, limits := range c.Capacity {
		limits.Active, limits.Queued = 1, 1
		c.Capacity[workload] = limits
	}
	a := newAdmission(c)
	mux := http.NewServeMux()
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages", func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-release; w.WriteHeader(200) })
	for _, route := range []string{"POST /api/v1/conversations/{id}/messages", "GET /api/v1/conversations/{id}/messages/{message}/embeds", "GET /api/v1/attachments/{id}", "GET /api/v1/voice/{id}", "GET /api/v1/instance", "GET /healthz"} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	}
	server := httptest.NewServer(a.wrap(mux, mux))
	defer server.Close()
	defer once.Do(func() { close(release) })
	done := make(chan int, 2)
	request := func(method, path string) int {
		req, _ := http.NewRequest(method, server.URL+path, nil)
		response, err := server.Client().Do(req)
		if err != nil {
			return 0
		}
		response.Body.Close()
		return response.StatusCode
	}
	go func() { done <- request("GET", "/api/v1/conversations/a/messages") }()
	<-entered
	go func() { done <- request("GET", "/api/v1/conversations/a/messages") }()
	waitQueued(t, a[config.History], 1)
	if status := request("GET", "/api/v1/conversations/a/messages"); status != 503 {
		t.Fatal(status)
	}
	for _, route := range [][2]string{{"POST", "/api/v1/conversations/a/messages"}, {"GET", "/api/v1/conversations/a/messages/b/embeds"}, {"GET", "/api/v1/attachments/a"}, {"GET", "/api/v1/voice/a"}, {"GET", "/api/v1/instance"}, {"GET", "/healthz"}} {
		if status := request(route[0], route[1]); status != 204 {
			t.Fatal(route, status)
		}
	}
	once.Do(func() { close(release) })
	for range 2 {
		if status := <-done; status != 200 {
			t.Fatal(status)
		}
	}
}

func TestDatabaseWorkloadIsolationThroughHTTP(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	message := sendMessage(t, f, f.owner.Token, channel.ID, "https://preview.example.com/article")
	features.Previews.Close()
	features.Previews = &previewFixture{}
	base := "/api/v1/conversations/" + channel.ID + "/messages"
	paths := map[config.Workload]string{config.Control: "/api/v1/me", config.History: base + "?latest=true", config.Embeds: base + "/" + message.ID + "/embeds", config.Media: base + "/" + message.ID + "/embeds/image?index=0"}
	for blocked, path := range paths {
		t.Run(string(blocked), func(t *testing.T) {
			pool := f.pools.For(postgres.WithWorkload(context.Background(), blocked))
			held := []*pgxpool.Conn{}
			defer func() {
				for _, conn := range held {
					conn.Release()
				}
			}()
			for range pool.Config().MaxConns {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				conn, err := pool.Acquire(ctx)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, conn)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			req, _ := http.NewRequestWithContext(ctx, "GET", f.server.URL+path, nil)
			req.Header.Set("Authorization", "Bearer "+f.owner.Token)
			response, err := f.server.Client().Do(req)
			cancel()
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("saturated %s did not block its own request: %v", blocked, err)
			}
			for workload, available := range paths {
				if workload != blocked {
					if status, _, err := f.request("GET", available, f.owner.Token, nil); status != 200 || err != nil {
						t.Fatal(workload, status, err)
					}
				}
			}
			conn := wsConnect(t, f, f.owner.Token)
			wsSend(t, conn, map[string]any{"type": "subscribe", "conversations": []string{channel.ID}})
			wsRead(t, conn, "subscribed")
			sent := sendMessage(t, f, f.owner.Token, channel.ID, "send during "+string(blocked)+" saturation")
			event := wsRead(t, conn, "message.created")
			var received struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event["data"], &received) != nil || received.ID != sent.ID {
				t.Fatal("event delivery failed")
			}
			conn.CloseNow()
			for _, conn := range held {
				conn.Release()
			}
			held = nil
			if status, _, err := f.request("GET", path, f.owner.Token, nil); status != 200 || err != nil {
				t.Fatal(status, err)
			}
		})
	}
}

func TestSlowEmbedsDoNotBlockMessagesHistoryOrRealtime(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	message := sendMessage(t, f, f.owner.Token, channel.ID, "https://preview.example.com/article")
	entered, release := make(chan struct{}, 8), make(chan struct{})
	defer close(release)
	features.Previews.Close()
	features.Previews = &previewFixture{page: func(ctx context.Context, url string) (embeds.Preview, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return embeds.Preview{URL: url, Kind: "article", Title: "Ready"}, nil
		case <-ctx.Done():
			return embeds.Preview{}, ctx.Err()
		}
	}}
	path := "/api/v1/conversations/" + channel.ID + "/messages"
	for range 8 {
		go func() { _, _, _ = f.request("GET", path+"/"+message.ID+"/embeds", f.owner.Token, nil) }()
	}
	for range 8 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("embed request did not reach provider")
		}
	}
	f.call(t, "GET", path+"?latest=true", f.owner.Token, nil, 200, nil)
	conn := wsConnect(t, f, f.owner.Token)
	wsSend(t, conn, map[string]any{"type": "subscribe", "conversations": []string{channel.ID}})
	wsRead(t, conn, "subscribed")
	sendMessage(t, f, f.owner.Token, channel.ID, "message during slow embeds")
	wsRead(t, conn, "message.created")
}
