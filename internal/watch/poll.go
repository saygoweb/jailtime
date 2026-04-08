package watch

import (
	"bufio"
	"context"
	"log/slog"
	"os"
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

type tailPathInfo struct {
	jails       []string
	readFromEnd bool
}

type staticPathInfo struct {
	jails []string
}

// buildExcludeSet expands all ExcludeGlobs patterns from specs and returns
// a set of paths to exclude.
func buildExcludeSet(specs []WatchSpec) map[string]struct{} {
	excluded := make(map[string]struct{})
	for _, spec := range specs {
		for _, pattern := range spec.ExcludeGlobs {
			paths, err := filepath.Glob(pattern)
			if err != nil {
				continue
			}
			for _, p := range paths {
				excluded[p] = struct{}{}
			}
		}
	}
	return excluded
}

// Start begins polling. Every interval it expands globs across all WatchSpecs,
// maintains one FileTailer per unique file path (shared across tail-mode jails),
// and for static-mode jails diffs the entire file against a snapshot.
func (b *PollBackend) Start(ctx context.Context, specs []WatchSpec, drain DrainFunc) error {
	b.UpdateSpecs(specs)

	tailers := make(map[string]*FileTailer)
	staticSnapshots := make(map[string]map[string]bool)

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

			excluded := buildExcludeSet(currentSpecs)

			tailPaths := make(map[string]*tailPathInfo)
			staticPaths := make(map[string]*staticPathInfo)

			for _, spec := range currentSpecs {
				for _, pattern := range spec.Globs {
					paths, err := filepath.Glob(pattern)
					if err != nil || paths == nil {
						continue
					}
					for _, p := range paths {
						if _, ex := excluded[p]; ex {
							continue
						}
						if spec.WatchMode == "static" {
							pi, ok := staticPaths[p]
							if !ok {
								pi = &staticPathInfo{}
								staticPaths[p] = pi
							}
							pi.jails = append(pi.jails, spec.JailName)
						} else {
							pi, ok := tailPaths[p]
							if !ok {
								pi = &tailPathInfo{readFromEnd: spec.ReadFromEnd}
								tailPaths[p] = pi
							}
							pi.jails = append(pi.jails, spec.JailName)
						}
					}
				}
			}

			var batch []RawLine
			now := time.Now()

			// Tail-mode: open/update FileTailers and read new lines.
			for p, pi := range tailPaths {
				if _, ok := tailers[p]; !ok {
					ft, err := NewFileTailer(p, pi.readFromEnd)
					if err != nil {
						continue
					}
					tailers[p] = ft
				}
			}
			for p, ft := range tailers {
				pi, matched := tailPaths[p]
				if !matched {
					ft.Close()
					delete(tailers, p)
					continue
				}
				if _, err := ft.CheckRotation(); err != nil {
					continue
				}
				lines, err := ft.ReadLines()
				if err != nil {
					continue
				}
				for _, line := range lines {
					if slog.Default().Enabled(ctx, slog.LevelDebug) && ft.debugLog.Allow() {
						slog.DebugContext(ctx, "line notified", "jails", pi.jails, "file", p, "line", line)
					}
					batch = append(batch, RawLine{FilePath: p, Line: line, Jails: pi.jails, EnqueueAt: now, Kind: EventTail})
				}
			}

			// Static-mode: re-read entire file, diff against snapshot.
			for p, pi := range staticPaths {
				current := readAllLines(p)
				prev := staticSnapshots[p]
				if prev == nil {
					prev = make(map[string]bool)
				}
				// Added lines.
				for line := range current {
					if !prev[line] {
						batch = append(batch, RawLine{FilePath: p, Line: line, Jails: pi.jails, EnqueueAt: now, Kind: EventAdded})
					}
				}
				// Removed lines.
				for line := range prev {
					if !current[line] {
						batch = append(batch, RawLine{FilePath: p, Line: line, Jails: pi.jails, EnqueueAt: now, Kind: EventRemoved})
					}
				}
				staticSnapshots[p] = current
			}
			// Clean up snapshots for paths no longer watched in static mode.
			for p := range staticSnapshots {
				if _, ok := staticPaths[p]; !ok {
					delete(staticSnapshots, p)
				}
			}

			if len(batch) > 0 {
				drain(ctx, batch)
			}
		}
	}
}

// readAllLines reads every non-empty line from path and returns them as a set.
func readAllLines(path string) map[string]bool {
	f, err := os.Open(path)
	if err != nil {
		return make(map[string]bool)
	}
	defer f.Close()
	lines := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines[line] = true
		}
	}
	return lines
}
