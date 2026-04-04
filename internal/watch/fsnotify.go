package watch

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// FsnotifyBackend implements Backend using fsnotify for event-driven watching,
// with periodic glob rescanning to pick up new matching files.
type FsnotifyBackend struct {
	pollInterval time.Duration
	mu           sync.RWMutex
	specs        []WatchSpec
}

func NewFsnotifyBackend(pollInterval time.Duration) *FsnotifyBackend {
	return &FsnotifyBackend{pollInterval: pollInterval}
}

func (b *FsnotifyBackend) Name() string { return "fsnotify" }

// UpdateSpecs replaces the current watch specs. The change takes effect on
// the next rescan tick.
func (b *FsnotifyBackend) UpdateSpecs(specs []WatchSpec) {
	b.mu.Lock()
	b.specs = specs
	b.mu.Unlock()
}

func (b *FsnotifyBackend) getSpecs() []WatchSpec {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.specs
}

// Start watches files using fsnotify for WRITE/CREATE events.
// Periodically rescans globs to pick up new matching files.
// One FileTailer is maintained per unique file path (shared across jails);
// each line is fanned out to every jail whose globs match that path.
// On CREATE event for a watched path, the FileTailer is reopened.
func (b *FsnotifyBackend) Start(ctx context.Context, specs []WatchSpec, out chan<- Event) error {
	b.UpdateSpecs(specs)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Info("fsnotify watcher unavailable, falling back to poll backend", "error", err)
		// Fall back to poll backend.
		pb := NewPollBackend(b.pollInterval)
		return pb.Start(ctx, b.getSpecs(), out)
	}
	defer watcher.Close()
	slog.Info("fsnotify backend started")

	// tailers maps file path → FileTailer (one per unique path across all jails).
	tailers := make(map[string]*FileTailer)
	// pathToJails maps file path → list of jail names watching it.
	pathToJails := make(map[string][]string)

	rescan := func() {
		currentSpecs := b.getSpecs()

		// Rebuild pathToJails from current specs to handle jail additions/removals.
		newPathToJails := make(map[string][]string)
		pathReadFromEnd := make(map[string]bool)
		for _, spec := range currentSpecs {
			for _, pattern := range spec.Globs {
				paths, err := filepath.Glob(pattern)
				if err != nil {
					continue
				}
				for _, p := range paths {
					newPathToJails[p] = append(newPathToJails[p], spec.JailName)
					if _, set := pathReadFromEnd[p]; !set {
						pathReadFromEnd[p] = spec.ReadFromEnd
					}
				}
			}
		}

		// Open tailers for newly matched paths.
		for p := range newPathToJails {
			if _, ok := tailers[p]; !ok {
				ft, err := NewFileTailer(p, pathReadFromEnd[p])
				if err != nil {
					continue
				}
				tailers[p] = ft
				// Ignore error if already watched.
				_ = watcher.Add(p)
			}
		}

		// Close tailers for paths no longer matched by any spec.
		for p, ft := range tailers {
			if _, ok := newPathToJails[p]; !ok {
				ft.Close()
				delete(tailers, p)
				_ = watcher.Remove(p)
			}
		}

		pathToJails = newPathToJails
	}

	readAndSend := func(p string) {
		ft, ok := tailers[p]
		if !ok {
			return
		}
		lines, err := ft.ReadLines()
		if err != nil {
			return
		}
		for _, line := range lines {
			for _, jailName := range pathToJails[p] {
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
				}
			}
		}
	}

	// Initial scan.
	rescan()

	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			for _, ft := range tailers {
				ft.Close()
			}
			return ctx.Err()

		case <-ticker.C:
			rescan()

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			p := event.Name
			if event.Has(fsnotify.Create) {
				// File was recreated (rotation): reopen tailer from start.
				if ft, ok := tailers[p]; ok {
					ft.Close()
				}
				ft, err := NewFileTailer(p, false)
				if err != nil {
					delete(tailers, p)
					continue
				}
				tailers[p] = ft
			}
			readAndSend(p)

		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
		}
	}
}
