package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/embeds"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/chat"
)

func TestHistoryScale(t *testing.T) {
	if os.Getenv("HAUNT_SCALE_TEST") != "1" {
		t.Skip("set HAUNT_SCALE_TEST=1 for the isolated history capacity check")
	}
	f, features := featureFixture(t, 8<<30)
	reader := f.register(t, "scale_reader")
	channel := textChannel(t, f)
	first := sendMessage(t, f, f.owner.Token, channel.ID, "Scale history message with representative text and a retained conversation reference.")
	report := []map[string]any{}
	defer func() {
		data, _ := json.MarshalIndent(report, "", "  ")
		if destination := os.Getenv("HAUNT_SCALE_REPORT"); destination != "" {
			if err := os.WriteFile(destination, data, 0600); err != nil {
				t.Error(err)
			}
		}
	}()
	previous := 1
	counts := []int{10_000, 100_000, 1_000_000}
	if os.Getenv("HAUNT_SCALE_CONNECT_ONLY") == "1" {
		counts = []int{1_000_000}
	}
	if os.Getenv("HAUNT_SCALE_UI_ONLY") == "1" {
		counts = []int{10000}
	}
	for _, count := range counts {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		_, err := f.pool.Exec(ctx, `INSERT INTO chatxo.messages(id,conversation_id,author_id,seq,body,attachment_ids,created_at,deleted,request_id,request_hash,bytes)
		SELECT md5($1::text||':'||n)::uuid,$1::uuid,$2::uuid,n,$3,'{}',now()-interval '1 day'+n*interval '1 millisecond',false,md5('request:'||$1::text||':'||n)::uuid,decode(repeat('00',32),'hex'),512 FROM generate_series($4::bigint,$5::bigint) n`, channel.ID, first.AuthorID, first.Body, previous+1, count)
		if err == nil {
			_, err = f.pool.Exec(ctx, `UPDATE chatxo.conversations SET last_seq=$2 WHERE id=$1`, channel.ID, count)
		}
		if err == nil {
			_, err = f.pool.Exec(ctx, `UPDATE chatxo.storage SET text_bytes=(SELECT sum(bytes) FROM chatxo.messages)+(SELECT COALESCE(sum(bytes),0) FROM chatxo.events); ANALYZE chatxo.messages`)
		}
		var actual int
		if err == nil {
			err = f.pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.messages WHERE conversation_id=$1 AND created_at>now()-interval '30 days'`, channel.ID).Scan(&actual)
		}
		cancel()
		if err != nil || actual != count {
			t.Fatalf("seed count=%d actual=%d: %v", count, actual, err)
		}
		if os.Getenv("HAUNT_SCALE_UI_ONLY") == "1" {
			if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.messages SET reply_to_id=$2 WHERE conversation_id=$1 AND seq=$3`, channel.ID, first.ID, count); err != nil {
				t.Fatal(err)
			}
		}
		previous = count
		readersToTest := []int{1, 10, 50, 200, 500, 1000}
		if os.Getenv("HAUNT_SCALE_CONNECT_ONLY") == "1" || os.Getenv("HAUNT_SCALE_UI_ONLY") == "1" {
			readersToTest = nil
		}

		for _, readers := range readersToTest {
			result := runHistoryBurst(t, f, channel.ID, count, readers)
			report = append(report, result)
			if readers <= 200 && result["failures"].(int) != 0 {
				t.Errorf("%d readers failed: %v", readers, result)
			}
		}
		if count == 1_000_000 && len(readersToTest) > 0 {
			report = append(report, runMixedScale(t, f, features, channel.ID))
		}

		if (count == 10_000 || os.Getenv("HAUNT_SCALE_CONNECT_ONLY") == "1") && os.Getenv("HAUNT_SCALE_CLIENT_DIR") != "" {
			fixture := map[string]any{"origin": f.server.URL, "conversation": channel.ID, "login": reader, "messages": count}
			data, _ := json.Marshal(fixture)
			input := filepath.Join(t.TempDir(), "scale.json")
			if err := os.WriteFile(input, data, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("node", "scripts/check-history-scale.mjs", input)
			cmd.Dir = os.Getenv("HAUNT_SCALE_CLIENT_DIR")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type scaleSample struct {
	ms  float64
	err string
}

func scaleHistory(f *fixture, channel string) scaleSample {
	at := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	query := "after=0"
	if os.Getenv("HAUNT_SCALE_LATEST") == "1" {
		query = "latest=true"
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", f.server.URL+"/api/v1/conversations/"+channel+"/messages?"+query+"&limit=100", nil)
	req.Header.Set("Authorization", "Bearer "+f.owner.Token)
	response, err := f.server.Client().Do(req)
	if err == nil {
		var page chat.History
		err = json.NewDecoder(response.Body).Decode(&page)
		response.Body.Close()
		if response.StatusCode != 200 || len(page.Messages) != 100 {
			err = fmt.Errorf("status=%d messages=%d", response.StatusCode, len(page.Messages))
		}
	}
	sample := scaleSample{ms: float64(time.Since(at).Microseconds()) / 1000}
	if err != nil {
		sample.err = err.Error()
	}
	return sample
}

func scaleSummary(samples []scaleSample) map[string]any {
	all, successful := []float64{}, []float64{}
	failures := map[string]int{}
	for _, sample := range samples {
		all = append(all, sample.ms)
		if sample.err == "" {
			successful = append(successful, sample.ms)
		} else {
			failures[sample.err]++
		}
	}
	sort.Float64s(all)
	sort.Float64s(successful)
	result := map[string]any{"requests": len(all), "failures": len(all) - len(successful), "failure_kinds": failures}
	for name, values := range map[string][]float64{"": all, "successful_": successful} {
		if len(values) == 0 {
			continue
		}
		for label, pct := range map[string]int{"p50_ms": 50, "p95_ms": 95, "p99_ms": 99} {
			result[name+label] = values[(len(values)-1)*pct/100]
		}
	}
	return result
}

func scaleMemory() func() map[string]any {
	done, stopped := make(chan struct{}), make(chan struct{})
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	start, peak, goroutines := memory.HeapAlloc, memory.HeapAlloc, runtime.NumGoroutine()
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&memory)
				peak = max(peak, memory.HeapAlloc)
				goroutines = max(goroutines, runtime.NumGoroutine())
			}
		}
	}()
	return func() map[string]any {
		close(done)
		<-stopped
		return map[string]any{"start_heap_mib": float64(start) / (1 << 20), "peak_heap_mib": float64(peak) / (1 << 20), "peak_goroutines": goroutines, "scope": "server and in-process load generator"}
	}
}

func runHistoryBurst(t *testing.T, f *fixture, channel string, count, readers int) map[string]any {
	t.Helper()
	samples := make([]scaleSample, readers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	pool := f.pools.For(postgres.WithWorkload(context.Background(), config.History))
	before := pool.Stat()
	finishMemory := scaleMemory()
	at := time.Now()
	for index := range readers {
		wg.Go(func() { <-start; samples[index] = scaleHistory(f, channel) })
	}
	close(start)
	wg.Wait()
	after := pool.Stat()
	result := scaleSummary(samples)
	result["kind"], result["messages"], result["readers"] = "burst", count, readers
	result["duration_ms"] = float64(time.Since(at).Microseconds()) / 1000
	result["memory"] = finishMemory()
	result["db_acquires"] = after.AcquireCount() - before.AcquireCount()
	result["db_empty_acquires"] = after.EmptyAcquireCount() - before.EmptyAcquireCount()
	result["db_acquire_ms"] = float64((after.AcquireDuration() - before.AcquireDuration()).Microseconds()) / 1000
	result["db_connections"] = after.MaxConns()
	data, _ := json.Marshal(result)
	t.Log(string(data))
	return result
}

type scalePreviews struct{}

func (scalePreviews) Page(ctx context.Context, url string) (embeds.Preview, error) {
	select {
	case <-ctx.Done():
		return embeds.Preview{}, ctx.Err()
	case <-time.After(125 * time.Millisecond):
	}
	return embeds.Preview{URL: url, Title: "Scale preview", Kind: "article", Image: true, ImageURL: "https://scale.example.com/image"}, nil
}
func (scalePreviews) Image(ctx context.Context, url string) (embeds.Asset, error) {
	select {
	case <-ctx.Done():
		return embeds.Asset{}, ctx.Err()
	case <-time.After(125 * time.Millisecond):
	}
	return embeds.Asset{Type: "image/png", Data: make([]byte, 128<<10)}, nil
}
func (scalePreviews) Close() {}

func runMixedScale(t *testing.T, f *fixture, features *Features, channel string) map[string]any {
	t.Helper()
	message := sendMessage(t, f, f.owner.Token, channel, "https://scale.example.com/post")
	features.Previews.Close()
	features.Previews = scalePreviews{}
	base := "/api/v1/conversations/" + channel + "/messages"
	embedPath := base + "/" + message.ID + "/embeds"
	var mu sync.Mutex
	samples := map[string][]scaleSample{}
	var wg sync.WaitGroup
	finishMemory := scaleMemory()
	at := time.Now()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	launch := func(kind string, call func() scaleSample) {
		wg.Go(func() { result := call(); mu.Lock(); samples[kind] = append(samples[kind], result); mu.Unlock() })
	}
	request := func(method, path string, body any, want int) scaleSample {
		start := time.Now()
		status, _, err := f.request(method, path, f.owner.Token, body)
		sample := scaleSample{ms: float64(time.Since(start).Microseconds()) / 1000}
		if err != nil {
			sample.err = err.Error()
		} else if status != want {
			sample.err = fmt.Sprintf("status=%d", status)
		}
		return sample
	}
	for index := range 2000 {
		<-ticker.C
		launch("history", func() scaleSample { return scaleHistory(f, channel) })
		if index%25 == 0 {
			launch("embeds", func() scaleSample { return request("GET", embedPath, nil, 200) })
			launch("media", func() scaleSample { return request("GET", embedPath+"/image?index=0", nil, 200) })
		}
		if index%200 == 0 {
			launch("sends", func() scaleSample {
				return request("POST", base, map[string]string{"body": "Message during mixed traffic", "request_id": identity.NewID()}, 201)
			})
		}
	}
	wg.Wait()
	result := map[string]any{"kind": "sustained_mixed", "messages": 1_000_000, "duration_ms": float64(time.Since(at).Microseconds()) / 1000, "target_history_rps": 200, "provider_delay_ms": 125, "memory": finishMemory(), "pools": databaseStats(f.pools)}
	for kind, values := range samples {
		summary := scaleSummary(values)
		result[kind] = summary
		if summary["failures"].(int) != 0 {
			t.Errorf("mixed %s: %v", kind, summary)
		}
	}
	data, _ := json.Marshal(result)
	t.Log(string(data))
	return result
}
