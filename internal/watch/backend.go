package watch

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Event represents a new log line read from a watched file.
type Event struct {
	JailName string
	FilePath string
	Offset   int64
	Line     string
	Time     time.Time
}

// WatchSpec defines what a jail wants to watch.
type WatchSpec struct {
	JailName    string
	Globs       []string
	ReadFromEnd bool
}

// Backend is the abstraction for file watching implementations.
type Backend interface {
	Name() string
	Start(ctx context.Context, specs []WatchSpec, out chan<- Event) error
	// UpdateSpecs replaces the set of watch specs. New jails are picked up on
	// the next rescan/poll cycle; removed jails stop generating events.
	UpdateSpecs(specs []WatchSpec)
}

// debugRateLimiter allows at most maxPerSec log entries per second.
type debugRateLimiter struct {
	mu          sync.Mutex
	windowStart time.Time
	count       int
	maxPerSec   int
	now         func() time.Time
}

func newDebugRateLimiter(maxPerSec int) *debugRateLimiter {
	return &debugRateLimiter{maxPerSec: maxPerSec, now: time.Now}
}

func (r *debugRateLimiter) Allow() bool {
	t := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.windowStart.IsZero() || t.Sub(r.windowStart) >= time.Second {
		r.windowStart = t
		r.count = 0
	}
	if r.count < r.maxPerSec {
		r.count++
		return true
	}
	return false
}

// NewAuto selects the best available backend based on mode.
//
// Accepted modes:
//   - "poll":                use the filesystem-polling backend.
//   - "auto", "fsnotify",
//     "inotify", "os":       use the fsnotify backend (inotify on Linux),
//     with automatic fallback to poll if fsnotify is unavailable.
//
// The selected backend is logged at Info level. For fsnotify/inotify modes,
// the actual backend in use (fsnotify vs poll fallback) is also logged when
// Start() is called.
func NewAuto(mode string, pollInterval time.Duration) Backend {
	switch mode {
	case "poll":
		slog.Info("watch backend selected", "requested_mode", mode, "backend", "poll")
		return NewPollBackend(pollInterval)
	default: // "auto", "fsnotify", "inotify", "os"
		// "inotify" and "os" are aliases for the fsnotify backend, which uses
		// the kernel inotify API on Linux.
		slog.Info("watch backend selected", "requested_mode", mode, "backend", "fsnotify")
		return NewFsnotifyBackend(pollInterval)
	}
}
