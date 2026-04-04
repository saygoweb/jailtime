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

	type tailerKey struct {
		jailName string
		path     string
	}
	tailers := make(map[tailerKey]*FileTailer)
	// pathToKeys maps a watched path to the set of tailerKeys using it.
	pathToKeys := make(map[string][]tailerKey)

	addFile := func(spec WatchSpec, p string) {
		k := tailerKey{spec.JailName, p}
		if _, ok := tailers[k]; ok {
			return
		}
		ft, err := NewFileTailer(p, spec.ReadFromEnd)
		if err != nil {
			return
		}
		tailers[k] = ft
		pathToKeys[p] = append(pathToKeys[p], k)
		// Ignore error if already watched.
		_ = watcher.Add(p)
	}

	rescan := func() {
		for _, spec := range b.getSpecs() {
			for _, pattern := range spec.Globs {
				paths, err := filepath.Glob(pattern)
				if err != nil {
					continue
				}
				for _, p := range paths {
					addFile(spec, p)
				}
			}
		}
	}

	readAndSend := func(k tailerKey) {
		ft, ok := tailers[k]
		if !ok {
			return
		}
		lines, err := ft.ReadLines()
		if err != nil {
			return
		}
		for _, line := range lines {
			if slog.Default().Enabled(ctx, slog.LevelDebug) && ft.debugLog.Allow() {
				slog.DebugContext(ctx, "line notified",
					"jail", k.jailName,
					"file", k.path,
					"line", line,
				)
			}
			select {
			case out <- Event{
				JailName: k.jailName,
				FilePath: k.path,
				Offset:   ft.offset,
				Line:     line,
				Time:     time.Now(),
			}:
			case <-ctx.Done():
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
			keys := pathToKeys[event.Name]
			for _, k := range keys {
				if event.Has(fsnotify.Create) {
					// File was recreated (rotation): reopen tailer from start.
					if ft, ok := tailers[k]; ok {
						ft.Close()
					}
					ft, err := NewFileTailer(k.path, false)
					if err != nil {
						delete(tailers, k)
						continue
					}
					tailers[k] = ft
				}
				readAndSend(k)
			}

		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
		}
	}
}
