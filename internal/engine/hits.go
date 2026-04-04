package engine

import (
	"sync"
	"time"
)

// HitWindow tracks the rolling hit count for one (jail, IP) pair.
type HitWindow struct {
	Count        int
	WindowExpiry time.Time
}

// HitTracker manages HitWindows for all IPs in a jail.
type HitTracker struct {
	mu      sync.Mutex
	windows map[string]*HitWindow
}

func NewHitTracker() *HitTracker {
	return &HitTracker{
		windows: make(map[string]*HitWindow),
	}
}

// Record records a hit for key at time t with findTime window.
// Returns (newCount, triggered) where triggered=true if count >= threshold.
// If triggered, the window count is reset to prevent retriggering.
func (ht *HitTracker) Record(key string, t time.Time, findTime time.Duration, threshold int) (count int, triggered bool) {
	ht.mu.Lock()
	defer ht.mu.Unlock()

	w, ok := ht.windows[key]
	if !ok {
		w = &HitWindow{}
		ht.windows[key] = w
	}

	if t.After(w.WindowExpiry) {
		w.Count = 0
	}

	w.Count++
	w.WindowExpiry = t.Add(findTime)

	count = w.Count
	triggered = count >= threshold
	if triggered {
		w.Count = 0
	}
	return count, triggered
}
