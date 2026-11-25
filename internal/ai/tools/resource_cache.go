package tools

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type channelKey struct{}

func WithChannel(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, channelKey{}, id)
}

type resourceKey struct{ channel, url string }
type cachedResource struct {
	resource fetchedResource
	expires  time.Time
}

// Cache only explicitly immutable, fresh responses without redirects or request variants.
func immutableLifetime(h http.Header) time.Duration {
	if h.Get("Vary") != "" || h.Get("Set-Cookie") != "" {
		return 0
	}
	immutable := false
	var age int64
	for _, field := range strings.Split(strings.ToLower(strings.Join(h.Values("Cache-Control"), ",")), ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(field), "=")
		switch name {
		case "no-store", "private", "no-cache":
			return 0
		case "immutable":
			immutable = true
		case "max-age":
			age, _ = strconv.ParseInt(strings.Trim(value, "\""), 10, 32)
		}
	}
	used, _ := strconv.ParseInt(h.Get("Age"), 10, 32)
	if !immutable || age <= used || used < 0 {
		return 0
	}
	return time.Duration(min(age-used, 300)) * time.Second
}
func (r *Registry) remember(key resourceKey, resource fetchedResource, life time.Duration) {
	if key.channel == "" || life <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	total := 0
	for k, v := range r.resources {
		if !v.expires.After(now) || k == key {
			delete(r.resources, k)
		} else {
			total += len(v.resource.data)
		}
	}
	if len(r.resources) >= 32 || total+len(resource.data) > 16<<20 {
		return
	}
	r.resources[key] = cachedResource{resource, now.Add(life)}
	// Expire even if the channel never reads another URL.
	time.AfterFunc(life, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.resources[key].expires.After(time.Now()) {
			delete(r.resources, key)
		}
	})
}
func (r *Registry) pace(ctx context.Context, u *url.URL) error {
	host := strings.ToLower(u.Hostname())
	r.mu.Lock()
	now := time.Now()
	for h, t := range r.nextFetch {
		if !t.After(now) {
			delete(r.nextFetch, h)
		}
	}
	next := r.nextFetch[host]
	if next.Before(now) {
		next = now
	}
	r.nextFetch[host] = next.Add(time.Duration(r.limits.HostIntervalMS) * time.Millisecond)
	r.mu.Unlock()
	if delay := time.Until(next); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.cooldown(u)
}
