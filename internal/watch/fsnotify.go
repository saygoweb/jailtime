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
// each line is emitted as a single RawLine with all interested jails.
//
// WRITE events are coalesced: instead of reading the file on every kernel
// notification (which can be hundreds per second under high load), the path
// is added to a dirty set and the set is drained on each batchInterval tick.
// This bounds file I/O to at most len(watchedPaths)/batchInterval reads/sec
// regardless of how rapidly the file is being written to.
func (b *FsnotifyBackend) Start(ctx context.Context, specs []WatchSpec, out chan<- RawLine) error {
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
	// dirty holds paths that have received WRITE/CREATE events since last batch.
	dirty := make(map[string]struct{})

	rescan := func() {
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

		// Step 2: rebuild pathToJails using cached glob results.
		newPathToJails := make(map[string][]string)
		pathReadFromEnd := make(map[string]bool)
		for _, spec := range currentSpecs {
			for _, pattern := range spec.Globs {
				for _, p := range globCache[pattern] {
					newPathToJails[p] = append(newPathToJails[p], spec.JailName)
					if _, set := pathReadFromEnd[p]; !set {
						pathReadFromEnd[p] = spec.ReadFromEnd
					}
				}
			}
		}

		// Open tailers for newly matched paths; mark them dirty for initial read.
		for p := range newPathToJails {
			if _, ok := tailers[p]; !ok {
				ft, err := NewFileTailer(p, pathReadFromEnd[p])
				if err != nil {
					continue
				}
				tailers[p] = ft
				dirty[p] = struct{}{}
				// Ignore error if already watched.
				_ = watcher.Add(p)
			}
		}

		// Close tailers for paths no longer matched by any spec.
		for p, ft := range tailers {
			if _, ok := newPathToJails[p]; !ok {
				ft.Close()
				delete(tailers, p)
				delete(dirty, p)
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
		jails := pathToJails[p]
		for _, line := range lines {
			if slog.Default().Enabled(ctx, slog.LevelDebug) && ft.debugLog.Allow() {
				slog.DebugContext(ctx, "line notified",
					"jails", jails,
					"file", p,
					"line", line,
				)
			}
			select {
			case out <- RawLine{
				FilePath:  p,
				Line:      line,
				Jails:     jails,
				EnqueueAt: time.Now(),
			}:
			case <-ctx.Done():
			}
		}
	}

	// Initial scan.
	rescan()

	// batchInterval controls how often dirty paths are read. Coalescing multiple
	// rapid WRITE events into a single ReadLines call is the primary CPU reduction.
	// 50 ms means at most 20 reads/sec/file regardless of write frequency.
	const batchInterval = 50 * time.Millisecond
	batchTicker := time.NewTicker(batchInterval)
	defer batchTicker.Stop()

	rescanTicker := time.NewTicker(b.pollInterval)
	defer rescanTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			for _, ft := range tailers {
				ft.Close()
			}
			return ctx.Err()

		case <-rescanTicker.C:
			rescan()

		case <-batchTicker.C:
			// Drain the dirty set: read each path once, fan out to watching jails.
			for p := range dirty {
				readAndSend(p)
				delete(dirty, p)
			}

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			p := event.Name
			if event.Has(fsnotify.Create) {
				// File was recreated (rotation): reopen tailer from start immediately
				// so subsequent reads come from the new file, not the old fd.
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
			// Mark dirty; the batch ticker will call readAndSend.
			dirty[p] = struct{}{}

		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
		}
	}
}
