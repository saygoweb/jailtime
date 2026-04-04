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
// each file once, and fans out new lines as Events to every jail watching it.
func (b *PollBackend) Start(ctx context.Context, specs []WatchSpec, out chan<- Event) error {
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
			// Expand all globs for all specs: path → {jails watching it, readFromEnd}.
			pathInfos := make(map[string]*pathInfo)
			for _, spec := range b.getSpecs() {
				for _, pattern := range spec.Globs {
					paths, err := filepath.Glob(pattern)
					if err != nil {
						continue
					}
					for _, p := range paths {
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

			// Read each file once; fan out lines to every jail watching it.
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
					for _, jailName := range pi.jails {
						if slog.Default().Enabled(ctx, slog.LevelDebug) && ft.debugLog.Allow() {
							slog.DebugContext(ctx, "line notified",
								"jail", jailName,
								"file", p,
								"line", line,
							)
						}
						select {
						case out <- Event{
							JailName: jailName,
							FilePath: p,
							Offset:   ft.offset,
							Line:     line,
							Time:     time.Now(),
						}:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
			}
		}
	}
}
