package watch

import (
"context"
"log/slog"
"path/filepath"
"sync"
"time"

"github.com/fsnotify/fsnotify"
)

// FsnotifyBackend implements Backend using fsnotify for event-driven watching.
// It uses a lazy one-shot drain timer: the timer is only armed when a dirty
// path is detected, so the goroutine is truly idle when no files change.
type FsnotifyBackend struct {
drainInterval time.Duration
mu            sync.RWMutex
specs         []WatchSpec
}

func NewFsnotifyBackend(drainInterval time.Duration) *FsnotifyBackend {
return &FsnotifyBackend{drainInterval: drainInterval}
}

func (b *FsnotifyBackend) Name() string { return "fsnotify" }

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
return NewPollBackend(b.drainInterval).Start(ctx, b.getSpecs(), drain)
}
defer watcher.Close()
slog.Info("fsnotify backend started")

tailers := make(map[string]*FileTailer)
pathToJails := make(map[string][]string)
dirty := make(map[string]struct{})
parentDirs := make(map[string][]string) // parent dir → []glob patterns

var drainTimerC <-chan time.Time // nil = idle
var lastDrainTime time.Duration // previous drain wall time

readLines := func(p string) []RawLine {
ft, ok := tailers[p]
if !ok {
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
result = append(result, RawLine{FilePath: p, Line: line, Jails: jails, EnqueueAt: now})
}
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
_ = watcher.Add(p)
}

initialScan := func() {
currentSpecs := b.getSpecs()
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
// Watch parent dir for CREATE events.
pd := globParentDir(pattern)
parentDirs[pd] = appendUniq(parentDirs[pd], pattern)
_ = watcher.Add(pd)
}
}
for p := range newPathToJails {
openTailer(p, pathReadFromEnd[p])
}
pathToJails = newPathToJails
}

handleCreate := func(name string) {
// Case 1: known file recreated (rotation).
if ft, ok := tailers[name]; ok {
_ = ft.Reopen(false)
dirty[name] = struct{}{}
return
}
// Case 2: new file matching a glob.
currentSpecs := b.getSpecs()
for _, spec := range currentSpecs {
for _, pattern := range spec.Globs {
if matched, err := filepath.Match(pattern, name); err == nil && matched {
pathToJails[name] = appendUniq(pathToJails[name], spec.JailName)
openTailer(name, spec.ReadFromEnd)
return
}
}
}
// Case 3: new directory — check if its parent dir is being watched for globs.
// Watch the new dir so CREATE events inside it are detected.
if patterns, ok := parentDirs[filepath.Dir(name)]; ok {
_ = watcher.Add(name)
for _, pattern := range patterns {
paths, err := filepath.Glob(pattern)
if err != nil {
continue
}
for _, p := range paths {
for _, spec := range currentSpecs {
for _, sp := range spec.Globs {
if sp == pattern {
pathToJails[p] = appendUniq(pathToJails[p], spec.JailName)
}
}
}
openTailer(p, false)
}
}
}
}

initialScan()

// Arm drain timer if initial scan found dirty files.
if len(dirty) > 0 {
wait := b.drainInterval
if wait < time.Millisecond {
wait = time.Millisecond
}
drainTimerC = time.NewTimer(wait).C
}

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
if len(dirty) > 0 && drainTimerC == nil {
wait := b.drainInterval - lastDrainTime
if wait < time.Millisecond {
wait = time.Millisecond
}
drainTimerC = time.NewTimer(wait).C
}
case event.Has(fsnotify.Write):
if _, known := pathToJails[event.Name]; known {
dirty[event.Name] = struct{}{}
if drainTimerC == nil {
wait := b.drainInterval - lastDrainTime
if wait < time.Millisecond {
wait = time.Millisecond
}
drainTimerC = time.NewTimer(wait).C
}
}
}

case <-drainTimerC:
drainStart := time.Now()
drainTimerC = nil
var batch []RawLine
for p := range dirty {
batch = append(batch, readLines(p)...)
delete(dirty, p)
}
drain(ctx, batch)
lastDrainTime = time.Since(drainStart)

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
