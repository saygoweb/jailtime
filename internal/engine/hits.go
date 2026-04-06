package engine

import (
	"sync"
	"time"
)

const numShards = 16

// HitTracker tracks hit counts per IP using a sharded map to reduce
// lock contention. 16 shards means at most 1/16 of IPs contend on
// a given lock.
type HitTracker struct {
	shards [numShards]hitShard
}

type hitShard struct {
	mu      sync.Mutex
	windows map[string]*HitWindow
}

// HitWindow tracks the hit count and sliding window expiry for one IP.
type HitWindow struct {
	Count        int
	WindowExpiry time.Time
}

// NewHitTracker creates a new HitTracker with initialized shards.
func NewHitTracker() *HitTracker {
	var ht HitTracker
	for i := range ht.shards {
		ht.shards[i].windows = make(map[string]*HitWindow)
	}
	return &ht
}

// shard returns the shard for the given key using inline FNV-1a.
func (ht *HitTracker) shard(key string) *hitShard {
	h := uint32(2166136261) // FNV-1a offset basis
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return &ht.shards[h%numShards]
}

// Record records a hit for the given IP. It returns the current hit count
// and whether the threshold was crossed (triggering an action).
// When triggered, the count is reset to zero so a fresh window begins.
func (ht *HitTracker) Record(ip string, t time.Time, findTime time.Duration, threshold int) (count int, triggered bool) {
	s := ht.shard(ip)
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.windows[ip]
	if !ok {
		w = &HitWindow{}
		s.windows[ip] = w
	}
	if t.After(w.WindowExpiry) {
		w.Count = 0
	}
	w.Count++
	w.WindowExpiry = t.Add(findTime)

	if w.Count >= threshold {
		count = w.Count
		w.Count = 0
		return count, true
	}
	return w.Count, false
}
