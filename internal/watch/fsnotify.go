package watch

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// rotationScanMultiple is the ratio of drainInterval used as the rotation-scan
// fallback period. The scan calls CheckRotation for every tailer so that
// copytruncate rotations and dropped Create events (inotify queue overflow on
// busy hosts) are caught within rotationScanMultiple × drainInterval.
const rotationScanMultiple = 60

// rotationScanMin is the floor for the rotation-scan period so the fallback
// fires even when the drain interval is very short (e.g. in tests).
const rotationScanMin = 5 * time.Second

// FsnotifyBackend implements Backend using fsnotify for event-driven watching.
// It uses a lazy one-shot drain timer: the timer is only armed when a dirty
// path is detected, so the goroutine is truly idle when no files change.
type FsnotifyBackend struct {
	mu            sync.RWMutex
	drainInterval time.Duration
	specs         []WatchSpec
}

func NewFsnotifyBackend(drainInterval time.Duration) *FsnotifyBackend {
	return &FsnotifyBackend{drainInterval: drainInterval}
}

func (b *FsnotifyBackend) Name() string { return "fsnotify" }

// SetInterval updates the drain interval. The change takes effect when the
// next drain timer is armed.
func (b *FsnotifyBackend) SetInterval(d time.Duration) {
	b.mu.Lock()
	b.drainInterval = d
	b.mu.Unlock()
}

func (b *FsnotifyBackend) getDrainInterval() time.Duration {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.drainInterval
}

// UpdateSpecs replaces the current watch specs. The change takes effect on
// the next rescan.
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

func (b *FsnotifyBackend) Start(ctx context.Context, specs []WatchSpec, drain DrainFunc) error {
	b.UpdateSpecs(specs)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Info("fsnotify unavailable, falling back to poll", "error", err)
		return NewPollBackend(b.getDrainInterval()).Start(ctx, b.getSpecs(), drain)
	}
	defer watcher.Close()
	slog.Info("fsnotify backend started")

	tailers := make(map[string]*FileTailer)
	pathToJails := make(map[string][]string) // tail-mode paths
	staticPathToJails := make(map[string][]string)
	staticSnapshots := make(map[string]map[string]bool)
	dirty := make(map[string]struct{})       // tail-mode dirty paths
	staticDirty := make(map[string]struct{}) // static-mode dirty paths
	parentDirs := make(map[string][]string)  // parent dir → []glob patterns
	watchedDirs := make(map[string]struct{}) // directories watched for creates

	var drainTimerC <-chan time.Time // nil = idle
	var lastDrainTime time.Duration  // previous drain wall time
	var pendingTriggerAt time.Time   // when the current pending drain was first triggered

	ensureDirWatch := func(dir string) {
		if _, ok := watchedDirs[dir]; ok {
			return
		}
		if err := watcher.Add(dir); err == nil {
			watchedDirs[dir] = struct{}{}
		}
	}

	readTailLines := func(p string) []RawLine {
		ft, ok := tailers[p]
		if !ok {
			return nil
		}
		// CheckRotation handles copytruncate-style rotation (size shrank) and
		// self-heals when a previous Reopen attempt failed (inode mismatch).
		if _, err := ft.CheckRotation(); err != nil {
			return nil
		}
		lines, err := ft.ReadLines()
		if err != nil {
			return nil
		}
		jails := pathToJails[p]
		now := time.Now()
		var result []RawLine
		for _, line := range lines {
			if slog.Default().Enabled(ctx, slog.LevelDebug) && ft.debugLog.Allow() {
				slog.DebugContext(ctx, "line notified", "jails", jails, "file", p, "line", line)
			}
			result = append(result, RawLine{FilePath: p, Line: line, Jails: jails, EnqueueAt: now, Kind: EventTail})
		}
		return result
	}

	diffStaticLines := func(p string) []RawLine {
		jails := staticPathToJails[p]
		current := readAllLines(p)
		prev := staticSnapshots[p]
		if prev == nil {
			prev = make(map[string]bool)
		}
		now := time.Now()
		var result []RawLine
		for line := range current {
			if !prev[line] {
				result = append(result, RawLine{FilePath: p, Line: line, Jails: jails, EnqueueAt: now, Kind: EventAdded})
			}
		}
		for line := range prev {
			if !current[line] {
				result = append(result, RawLine{FilePath: p, Line: line, Jails: jails, EnqueueAt: now, Kind: EventRemoved})
			}
		}
		staticSnapshots[p] = current
		return result
	}

	openTailer := func(p string, readFromEnd bool) {
		if _, ok := tailers[p]; ok {
			return
		}
		ft, err := NewFileTailer(p, readFromEnd)
		if err != nil {
			return
		}
		tailers[p] = ft
		dirty[p] = struct{}{}
		ensureDirWatch(filepath.Dir(p))
		_ = watcher.Add(p)
	}

	openStatic := func(p string) {
		ensureDirWatch(filepath.Dir(p))
		if _, ok := staticPathToJails[p]; ok {
			// Already tracked; mark dirty to do initial diff.
			staticDirty[p] = struct{}{}
			return
		}
		staticDirty[p] = struct{}{}
		_ = watcher.Add(p)
	}

	armDrainTimer := func() {
		if drainTimerC != nil {
			return
		}
		pendingTriggerAt = time.Now()
		wait := b.getDrainInterval() - lastDrainTime
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		drainTimerC = time.NewTimer(wait).C
	}

	initialScan := func() {
		currentSpecs := b.getSpecs()

		newPathToJails := make(map[string][]string)
		newStaticPathToJails := make(map[string][]string)
		pathReadFromEnd := make(map[string]bool)

		for _, spec := range currentSpecs {
			for _, pattern := range spec.Globs {
				paths, err := filepath.Glob(pattern)
				if err != nil || paths == nil {
					paths = []string{}
				}
				for _, p := range paths {
					if isExcluded(p, currentSpecs) {
						continue
					}
					if spec.WatchMode == "static" {
						newStaticPathToJails[p] = appendUniq(newStaticPathToJails[p], spec.JailName)
					} else {
						newPathToJails[p] = appendUniq(newPathToJails[p], spec.JailName)
						if _, set := pathReadFromEnd[p]; !set {
							pathReadFromEnd[p] = spec.ReadFromEnd
						}
					}
				}
				// Watch parent dir for CREATE events.
				pd := globParentDir(pattern)
				parentDirs[pd] = appendUniq(parentDirs[pd], pattern)
				ensureDirWatch(pd)
			}
		}
		for p := range newPathToJails {
			openTailer(p, pathReadFromEnd[p])
		}
		for p, jails := range newStaticPathToJails {
			staticPathToJails[p] = jails
			openStatic(p)
		}
		pathToJails = newPathToJails
	}

	handleCreate := func(name string) {
		currentSpecs := b.getSpecs()
		if isExcluded(name, currentSpecs) {
			return
		}

		// Check static specs first: collect all matching jails before calling
		// openStatic so that every jail watching this path is registered.
		for _, spec := range currentSpecs {
			if spec.WatchMode != "static" {
				continue
			}
			for _, pattern := range spec.Globs {
				if matched, err := filepath.Match(pattern, name); err == nil && matched {
					staticPathToJails[name] = appendUniq(staticPathToJails[name], spec.JailName)
				}
			}
		}
		if len(staticPathToJails[name]) > 0 {
			openStatic(name)
			return
		}

		// Case 1: known tail file recreated (rotation).
		if ft, ok := tailers[name]; ok {
			ensureDirWatch(filepath.Dir(name))
			_ = watcher.Add(name)
			_ = ft.Reopen(false)
			dirty[name] = struct{}{}
			return
		}
		// Case 2: new tail file matching a glob. Collect all matching jails across
		// all specs before calling openTailer — an early return after the first
		// match would leave subsequent jails unregistered for this file.
		firstReadFromEnd := false
		anyTailMatch := false
		for _, spec := range currentSpecs {
			if spec.WatchMode == "static" {
				continue
			}
			for _, pattern := range spec.Globs {
				if matched, err := filepath.Match(pattern, name); err == nil && matched {
					if !anyTailMatch {
						firstReadFromEnd = spec.ReadFromEnd
						anyTailMatch = true
					}
					pathToJails[name] = appendUniq(pathToJails[name], spec.JailName)
				}
			}
		}
		if anyTailMatch {
			openTailer(name, firstReadFromEnd)
			return
		}
		// Case 3: new directory — check if its parent dir is being watched for globs.
		if patterns, ok := parentDirs[filepath.Dir(name)]; ok {
			_ = watcher.Add(name)
			for _, pattern := range patterns {
				paths, err := filepath.Glob(pattern)
				if err != nil {
					continue
				}
				for _, p := range paths {
					if isExcluded(p, currentSpecs) {
						continue
					}
					// Determine mode for this pattern.
					mode := "tail"
					readFromEnd := false
					for _, spec := range currentSpecs {
						for _, sp := range spec.Globs {
							if sp == pattern {
								mode = spec.WatchMode
								readFromEnd = spec.ReadFromEnd
								pathToJails[p] = appendUniq(pathToJails[p], spec.JailName)
							}
						}
					}
					if mode == "static" {
						openStatic(p)
					} else {
						openTailer(p, readFromEnd)
					}
				}
			}
		}
	}

	initialScan()

	// Arm drain timer if initial scan found dirty files.
	if len(dirty) > 0 || len(staticDirty) > 0 {
		armDrainTimer()
	}

	// Rotation-scan fallback: periodically call CheckRotation for every tailer
	// so that copytruncate rotations and inotify-overflow-dropped Create events
	// are caught even when no fsnotify event arrives.
	scanInterval := time.Duration(rotationScanMultiple) * b.getDrainInterval()
	if scanInterval < rotationScanMin {
		scanInterval = rotationScanMin
	}
	rotationScanTicker := time.NewTicker(scanInterval)
	defer rotationScanTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			for _, ft := range tailers {
				ft.Close()
			}
			return ctx.Err()

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			switch {
			case event.Has(fsnotify.Create):
				handleCreate(event.Name)
				if (len(dirty) > 0 || len(staticDirty) > 0) && drainTimerC == nil {
					armDrainTimer()
				}
			case event.Has(fsnotify.Write):
				if _, known := pathToJails[event.Name]; known {
					dirty[event.Name] = struct{}{}
					armDrainTimer()
				}
				if _, known := staticPathToJails[event.Name]; known {
					staticDirty[event.Name] = struct{}{}
					armDrainTimer()
				}
			case event.Has(fsnotify.Rename):
				// Atomic replacements (e.g. rename(tmp, dst)) trigger Rename on
				// the destination file. Treat as a create for static paths.
				if _, known := staticPathToJails[event.Name]; known {
					staticDirty[event.Name] = struct{}{}
					armDrainTimer()
				}
				// For tail-mode paths, drain any lines written since the last
				// drain but before the rotation so they are not silently lost.
				if _, known := pathToJails[event.Name]; known {
					dirty[event.Name] = struct{}{}
					armDrainTimer()
				}
			}

		case <-drainTimerC:
			drainStart := time.Now()
			drainTimerC = nil
			triggerAt := pendingTriggerAt
			pendingTriggerAt = time.Time{}
			var batch []RawLine
			for p := range dirty {
				batch = append(batch, readTailLines(p)...)
				delete(dirty, p)
			}
			for p := range staticDirty {
				batch = append(batch, diffStaticLines(p)...)
				delete(staticDirty, p)
			}
			// Backfill EnqueueAt to the event trigger time so callers can
			// compute true event-to-drain latency.
			if !triggerAt.IsZero() {
				for i := range batch {
					batch[i].EnqueueAt = triggerAt
				}
			}
			drain(ctx, batch)
			lastDrainTime = time.Since(drainStart)

		case <-rotationScanTicker.C:
			// Fallback: detect rotations that were missed because the inotify
			// event queue overflowed, or because a previous Reopen failed.
			for p, ft := range tailers {
				rotated, err := ft.CheckRotation()
				if err != nil {
					// Reopen failed (new file not yet present); will retry next tick.
					continue
				}
				if rotated {
					dirty[p] = struct{}{}
					armDrainTimer()
				}
			}

		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
		}
	}
}

// globParentDir returns the deepest directory prefix before the first wildcard character.
func globParentDir(pattern string) string {
	for i, ch := range pattern {
		if ch == '*' || ch == '?' || ch == '[' {
			return filepath.Dir(pattern[:i])
		}
	}
	return filepath.Dir(pattern)
}

// appendUniq appends s to slice only if it is not already present.
func appendUniq(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}
