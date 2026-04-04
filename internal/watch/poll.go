package watch

import (
	"context"
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

// Start begins polling. For each WatchSpec it expands globs every interval,
// maintains a FileTailer per matched file, and sends new lines as Events.
func (b *PollBackend) Start(ctx context.Context, specs []WatchSpec, out chan<- Event) error {
	b.UpdateSpecs(specs)

	type tailerKey struct {
		jailName string
		path     string
	}
	tailers := make(map[tailerKey]*FileTailer)

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
			for _, spec := range b.getSpecs() {
				matched := make(map[string]bool)
				for _, pattern := range spec.Globs {
					paths, err := filepath.Glob(pattern)
					if err != nil {
						continue
					}
					for _, p := range paths {
						matched[p] = true
					}
				}

				// Open tailers for newly seen files.
				for p := range matched {
					k := tailerKey{spec.JailName, p}
					if _, ok := tailers[k]; !ok {
						ft, err := NewFileTailer(p, true)
						if err != nil {
							continue
						}
						tailers[k] = ft
					}
				}

				// Read lines from existing tailers; remove tailers for gone files.
				for k, ft := range tailers {
					if k.jailName != spec.JailName {
						continue
					}
					if !matched[k.path] {
						ft.Close()
						delete(tailers, k)
						continue
					}
					lines, err := ft.ReadLines()
					if err != nil {
						continue
					}
					for _, line := range lines {
						select {
						case out <- Event{
							JailName: k.jailName,
							FilePath: k.path,
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
