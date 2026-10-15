package httpapi

import (
	"crypto/sha256"
	"sync"
	"time"
)

type limitEntry struct {
	count int
	until time.Time
}
type limitShard struct {
	sync.Mutex
	entries map[[32]byte]limitEntry
}
type limiter struct{ shards [32]limitShard }

func newLimiter() *limiter {
	l := &limiter{}
	for i := range l.shards {
		l.shards[i].entries = make(map[[32]byte]limitEntry)
	}
	return l
}

// Short shard locks protect only counters. No network/database work happens
// inside them. Live entries have a strict memory bound; full shards fail closed.
func (l *limiter) allow(key string, max int, window time.Duration) bool {
	digest := sha256.Sum256([]byte(key))
	shard := &l.shards[int(digest[0])%len(l.shards)]
	now := time.Now()
	shard.Lock()
	defer shard.Unlock()
	entry, ok := shard.entries[digest]
	if ok && now.Before(entry.until) {
		if entry.count >= max {
			return false
		}
		entry.count++
		shard.entries[digest] = entry
		return true
	}
	if !ok && len(shard.entries) >= 512 {
		for key, value := range shard.entries {
			if !now.Before(value.until) {
				delete(shard.entries, key)
			}
		}
		if len(shard.entries) >= 512 {
			return false
		}
	}
	shard.entries[digest] = limitEntry{count: 1, until: now.Add(window)}
	return true
}
