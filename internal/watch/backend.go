package watch

import (
	"context"
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
}

// NewAuto selects the best available backend.
// mode can be "auto", "fsnotify", or "poll".
// For "auto" and "fsnotify": try fsnotify backend first, fall back to poll.
// For "poll": use poll backend directly.
func NewAuto(mode string, pollInterval time.Duration) Backend {
	if mode == "poll" {
		return NewPollBackend(pollInterval)
	}
	// "auto" or "fsnotify": try fsnotify, it will be used directly since
	// fsnotify is available; the FsnotifyBackend handles its own fallback via
	// periodic glob rescanning.
	return NewFsnotifyBackend(pollInterval)
}
