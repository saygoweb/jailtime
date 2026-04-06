package watch

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
)

// PollBackend implements Backend by polling the filesystem at a fixed interval.
type PollBackend struct {
	interval time.Duration
	mu       sync.RWMutex
	specs    []WatchSpec
}

func NewPollBackend(interval time.Duration) *PollBackend {
	return &PollBackend{interval: interval}
}

func (b *PollBackend) Name() string { return "poll" }

// UpdateSpecs replaces the current watch specs. The change takes effect on
// the next poll cycle.
func (b *PollBackend) UpdateSpecs(specs []WatchSpec) {
	b.mu.Lock()
	b.specs = specs
	b.mu.Unlock()
}

func (b *PollBackend) getSpecs() []WatchSpec {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.specs
}

// Start begins polling. Every interval it expands globs across all WatchSpecs,
// maintains one FileTailer per unique file path (shared across jails), reads
// each file once, and emits one RawLine per line with all interested jails.
func (b *PollBackend) Start(ctx context.Context, specs []WatchSpec, out chan<- RawLine) error {
	b.UpdateSpecs(specs)

	type pathInfo struct {
		jails       []string
		readFromEnd bool
	}

	// tailers maps file path → FileTailer (one per unique path across all jails).
	tailers := make(map[string]*FileTailer)

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			for _, ft := range tailers {
				ft.Close()
			}
			return ctx.Err()
		case <-ticker.C:
			currentSpecs := b.getSpecs()

			// Step 1: expand each unique glob pattern once.
			globCache := make(map[string][]string)
			for _, spec := range currentSpecs {
				for _, pattern := range spec.Globs {
					if _, seen := globCache[pattern]; !seen {
						paths, err := filepath.Glob(pattern)
						if err != nil || paths == nil {
							paths = []string{}
						}
						globCache[pattern] = paths
					}
				}
			}

			// Step 2: build path → {jails, readFromEnd} using cached glob results.
			pathInfos := make(map[string]*pathInfo)
			for _, spec := range currentSpecs {
				for _, pattern := range spec.Globs {
					for _, p := range globCache[pattern] {
						pi, ok := pathInfos[p]
						if !ok {
							pi = &pathInfo{readFromEnd: spec.ReadFromEnd}
							pathInfos[p] = pi
						}
						pi.jails = append(pi.jails, spec.JailName)
					}
				}
			}

			// Open tailers for newly seen paths.
			for p, pi := range pathInfos {
				if _, ok := tailers[p]; !ok {
					ft, err := NewFileTailer(p, pi.readFromEnd)
					if err != nil {
						continue
					}
					tailers[p] = ft
				}
			}

			// Read each file once; emit one RawLine per line with all watching jails.
			for p, ft := range tailers {
				pi, matched := pathInfos[p]
				if !matched {
					ft.Close()
					delete(tailers, p)
					continue
				}
				lines, err := ft.ReadLines()
				if err != nil {
					continue
				}
				for _, line := range lines {
					if slog.Default().Enabled(ctx, slog.LevelDebug) && ft.debugLog.Allow() {
						slog.DebugContext(ctx, "line notified",
							"jails", pi.jails,
							"file", p,
							"line", line,
						)
					}
					select {
					case out <- RawLine{
						FilePath:  p,
						Line:      line,
						Jails:     pi.jails,
						EnqueueAt: time.Now(),
					}:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
		}
	}
}
