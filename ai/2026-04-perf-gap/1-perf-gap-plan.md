# jailtime Performance Gap Plan

## Problem Statement

In production with ~150 Apache vhosts (~5,000 lines/day per vhost), `jailtimed` uses approximately 3× more CPU than fail2ban for the same workload. The goal is < 1% CPU on a 2-core machine with up to a dozen jails watching hundreds of files.

Profiling and review identified six root causes, all fixable without changing the Go implementation or overall architecture.

---

## Root Cause Analysis

### RC-1 — 50 ms `batchTicker` in `FsnotifyBackend` (CRITICAL)

**File:** `internal/watch/fsnotify.go`

The `batchTicker` fires every 50 ms regardless of whether any files have changed. With the current production config (150 watched files, 12 jails), this creates **72,000 goroutine wakeups per hour** doing zero useful work. Even when completely idle the scheduler is busy.

**Fix:** Replace the constant ticker with a **one-shot lazy drain timer** (the nil-channel pattern). The timer is armed only when the first WRITE event marks a file dirty. It fires once after `minLatency` (default 2 s), drains all dirty files, then goes nil again. When idle, the select loop sleeps with no timer running.

```
state: drainTimerC = nil (not running)

WRITE event → dirty[p] = true
              if drainTimerC == nil:
                  t := time.NewTimer(drainInterval)
                  drainTimerC = t.C

<drainTimerC fires> → drain all dirty files → drainTimerC = nil
                       (returns to blocking select; no rescheduling)
```

A nil channel in a Go `select` case blocks forever — this is idiomatic Go and avoids any goroutine overhead when idle.

---

### RC-2 — `rescanTicker` fires every 2 s calling `filepath.Glob` (HIGH)

**File:** `internal/watch/fsnotify.go`

A second ticker calls `rescan()` every 2 s, which runs `filepath.Glob` for every pattern. For `/var/log/apache2/*/access.log` across 150 vhosts, each glob run calls `os.Lstat` for ~151 directory entries. At 30 rescans/minute that is **~4,500 unnecessary syscalls per minute** in steady state when no new vhosts are being added.

**Fix:** Replace the periodic rescan with **CREATE-event-driven file discovery**:
1. At startup, watch each glob's "parent directory" (everything before the first wildcard character) with fsnotify in addition to individual files.
2. On a CREATE event for a path inside a watched parent dir: check if the path matches any glob pattern via `filepath.Match`. If yes, open a new FileTailer and watch the file.
3. On a CREATE event for an existing watched file: treat as log rotation — reopen via `Reopen(false)` (read new file from start).

With this design, `filepath.Glob` is called at startup and on directory CREATE events (rare: new vhosts are almost never added in production). Steady-state syscall overhead for file discovery drops to zero.

For the poll backend (no inotify), the periodic `filepath.Glob` per tick is unavoidable, but the ReadLines optimization below still applies.

---

### RC-3 — `ReadLines` calls `os.Stat` + `Seek` + `reader.Reset` on every invocation (HIGH)

**File:** `internal/watch/tail.go`

`ReadLines` currently:
1. Calls `os.Stat(ft.path)` — 1 syscall — to detect inode changes (rotation).
2. Calls `ft.file.Seek(ft.offset, io.SeekStart)` — 1 syscall — to rewind to the last safe position before reading.
3. Calls `ft.reader.Reset(ft.file)` — discards the `bufio.Reader` buffer.

With 150 watched files and a 50 ms batchTicker firing 20× per second, this is **up to 6,000 Stat + 6,000 Seek syscalls per second** even when files are completely idle.

**Root cause of Seek:** The `bufio.Reader` reads ahead in 4096-byte chunks. After a `ReadLines` call that emits complete lines up to a partial line, `ft.offset` points to the start of the partial line, but the fd is positioned past it (inside the bufio buffer). The next call seeks back to `ft.offset` and resets the reader to re-read from there.

**Fix:** Don't seek + reset between calls. The `bufio.Reader` already holds the partial line in its internal buffer. On the next call to `ReadLines`, `ReadString('\n')` will continue from where it left off, completing the partial line when the file grows. The seek+reset was a "return to known state" operation that is unnecessary given the bufio contract.

The only time seek + reset is needed is after **rotation**, handled by the new `Reopen()` method.

**New contract:**

```go
// ReadLines reads all new complete lines from the current reader position.
// Does NOT stat or seek. Caller is responsible for calling Reopen() if rotation
// is detected externally (via CREATE event or CheckRotation()).
func (ft *FileTailer) ReadLines() ([]string, error)

// Reopen reopens the file at ft.path from scratch (rotation handling).
// Resets the reader. If readFromEnd is true, seeks to EOF.
func (ft *FileTailer) Reopen(readFromEnd bool) error

// CheckRotation stats the file and calls Reopen(false) if inode changed or
// file shrank. Returns true if rotation was detected. Used by poll backend.
func (ft *FileTailer) CheckRotation() (bool, error)
```

---

### RC-4 — `cgroupCPUSampler` opens + closes fd + allocates `bufio.Scanner` on every `Sample()` (MEDIUM)

**File:** `internal/engine/cpu_cgroup.go`

`readUsageUsec()` is called on every `RecordExecution` call, which happens every timer fire. It:
1. Opens `/sys/fs/cgroup/.../cpu.stat`
2. Creates a new `bufio.Scanner` (heap allocation)
3. Reads the file
4. Closes the file

With a 50 ms timer this is **20 open/close/alloc operations per second**. On cgroup v2, `cpu.stat` is a pseudo-file that must be seeked to position 0 before each read (cannot be kept open and re-read without seeking).

**Fix:**
1. Keep the `*os.File` open in the struct.
2. Use a pre-allocated `[512]byte` stack array (no heap alloc).
3. On each `Sample()`: `Seek(0, io.SeekStart)`, `Read(buf[:])`, parse the fixed bytes manually.
4. Parse by scanning for `"usage_usec "` prefix without a Scanner.
5. Add a `Close()` method for graceful shutdown.
6. CPU sampling is only needed when there is actual work to report. Only call `Sample()` when processing a non-empty batch.

---

### RC-5 — Manager has a separate timer + batchQueue + enqueue goroutine (MEDIUM)

**File:** `internal/engine/manager.go`

The current architecture has two timer layers:
- Backend: 50 ms `batchTicker` (collects WRITE events into dirty set)
- Manager: 2–10 s adaptive EMA timer (drains `batchQueue` → calls `processBatch`)

With the backend's lazy one-shot drain timer (RC-1 fix), the backend now fires at `minLatency` (2 s). The manager's separate timer is redundant. The `batchQueue` mutex + enqueue goroutine adds latency and CPU overhead.

**Fix:** Remove the `batchQueue`, the enqueue goroutine, and the manager's timer. The manager's `Run()` loop simply reads from `rawLines` using a **channel drain** pattern:

```go
case line, ok := <-rawLines:
    batch := []watch.RawLine{line}
    // Non-blocking drain: collect all lines the backend sent in this drain cycle
drain:
    for {
        select {
        case l := <-rawLines:
            batch = append(batch, l)
        default:
            break drain
        }
    }
    processBatch(ctx, batch)
```

This is correct because the backend sends all lines from a drain cycle before the next event is processed. The manager wakes up only when the backend fires, collects everything available, and processes it. No timer, no mutex, no goroutine overhead.

**About `adaptInterval`:** With async on_match goroutines (from the fix/multi-trigger change), `processBatch` returns in microseconds — it only dispatches goroutines. The EMA interval adaptation was designed to avoid processing taking longer than the interval. Since `processBatch` is now non-blocking, the EMA is no longer necessary for correctness. However, the `adaptInterval` function and its tests are preserved as-is since they represent valid algorithmic work that may be useful if synchronous processing is ever needed.

`currentInterval` is retained as a field for perf API reporting; it is set to `minLatency` at construction and remains unchanged.

---

### RC-6 — CPU sample called on every timer fire, including idle cycles (LOW)

**File:** `internal/engine/perf.go`

`RecordExecution` calls `cpuSampler.Sample()` on every manager timer fire, including when `batchSize == 0`. With the 50 ms batchTicker this is 20 samples/second. After RC-1 (lazy drain timer), this becomes irrelevant — the timer only fires when there is work. CPU sampling is retained but only called when `batchSize > 0`.

---

## Target Architecture

```
fsnotify inotify kernel events
    │
    ▼
FsnotifyBackend.Start() — single goroutine:

  var drainTimerC <-chan time.Time  // nil = idle; never fires in select

  select:
    case <-ctx.Done()        → cleanup + return
    case event = watcher.Events:
      if CREATE:
        → check event.Name against glob patterns
        → if match: NewFileTailer + watcher.Add (new file or rotated file)
        → if dir in parent dirs: filepath.Glob that pattern family, add new tailers
      if WRITE:
        → dirty[event.Name] = true
        → if drainTimerC == nil:
              t = time.NewTimer(drainInterval)  // minLatency, default 2s
              drainTimerC = t.C
    case <-drainTimerC:
      → drainTimerC = nil     // disarm; will re-arm on next WRITE
      → for p in dirty: ft.ReadLines() → send to rawLines channel
      → clear dirty set

Manager.Run() — single goroutine:

  select:
    case <-ctx.Done()         → stop jails + return
    case err = <-backendErr   → return err
    case line = <-rawLines:   → collect all available lines (non-blocking drain)
                                → processBatch(ctx, batch)
                                → if len(batch) > 0: perf.RecordExecution (samples CPU)
```

**Key properties:**
- Zero goroutine wakeups when idle (no tickers)
- Zero stat/seek/alloc syscalls on idle files
- One timer only, armed on WRITE, disarmed after drain
- CPU sampling only on actual work

---

## Implementation Plan

Each unit below is self-contained and can be implemented independently by a sub-agent. Units 1 and 4 are prerequisites for Units 2 and 5 respectively. Unit 3 is independent.

---

### Unit 1: Refactor `FileTailer` — eliminate hot-path stat/seek/reset

**File:** `internal/watch/tail.go`

**Goal:** Remove `os.Stat`, `Seek`, and `reader.Reset` from the `ReadLines` hot path. Add `Reopen()` and `CheckRotation()` for explicit rotation handling.

**Changes:**

1. **Remove from `ReadLines`:**
   - The entire `os.Stat` block (inode check + rotation detection)
   - `ft.file.Seek(ft.offset, io.SeekStart)` call
   - `ft.reader.Reset(ft.file)` call
   - Keep only: the `ReadString('\n')` loop advancing `ft.offset` for complete lines

2. **Add `Reopen(readFromEnd bool) error` method:**
   ```go
   func (ft *FileTailer) Reopen(readFromEnd bool) error {
       ft.file.Close()
       f, err := os.Open(ft.path)
       if err != nil {
           return err
       }
       ft.file = f
       ft.reader.Reset(f)
       ft.offset = 0
       if readFromEnd {
           off, err := f.Seek(0, io.SeekEnd)
           if err != nil {
               f.Close()
               return err
           }
           ft.offset = off
       }
       // Update inode
       if info, err := f.Stat(); err == nil {
           if st, ok := info.Sys().(*syscall.Stat_t); ok {
               ft.inode = st.Ino
           }
       }
       return nil
   }
   ```

3. **Add `CheckRotation() (bool, error)` method:**
   ```go
   func (ft *FileTailer) CheckRotation() (bool, error) {
       info, err := os.Stat(ft.path)
       if err != nil {
           return false, nil // file disappeared; caller decides what to do
       }
       var curInode uint64
       if st, ok := info.Sys().(*syscall.Stat_t); ok {
           curInode = st.Ino
       }
       rotated := (curInode != 0 && curInode != ft.inode) || info.Size() < ft.offset
       if rotated {
           return true, ft.Reopen(false)
       }
       return false, nil
   }
   ```

4. **Keep `NewFileTailer` unchanged** — it still stats at creation to get the initial inode.

**Test changes (`internal/watch/watch_test.go`):**

- Existing rotation tests (`TestPollBackendRotation`) must continue to pass — rotation is now handled by `CheckRotation()` in the poll backend, not by `ReadLines` itself.
- Add a unit test for `FileTailer` directly:
  - Write 3 lines, call `ReadLines`, get 3 lines
  - Write 2 more lines, call `ReadLines` again, get 2 more lines (no seek between calls)
  - Verify no stat or seek is called (can verify by counting calls or by checking offset behavior)
- Add a unit test for `Reopen()`: write to file, read, call `Reopen(false)`, read again from start.

---

### Unit 2: Rewrite `FsnotifyBackend` — lazy drain timer + CREATE-driven discovery

**File:** `internal/watch/fsnotify.go`

**Prerequisite:** Unit 1 (new `Reopen()` and `ReadLines` without stat/seek)

**Goal:** Replace 50 ms batchTicker + 2 s rescanTicker with a single one-shot drain timer; replace periodic glob rescan with CREATE-event-driven discovery.

**New fields in `FsnotifyBackend`:**
```go
type FsnotifyBackend struct {
    drainInterval time.Duration  // renamed from pollInterval; = minLatency
    mu            sync.RWMutex
    specs         []WatchSpec
}
```

**Constructor:** `NewFsnotifyBackend(drainInterval time.Duration)`

**Core `Start()` rewrite:**

```go
func (b *FsnotifyBackend) Start(ctx context.Context, specs []WatchSpec, out chan<- RawLine) error {
    b.UpdateSpecs(specs)

    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        slog.Info("fsnotify unavailable, falling back to poll", "error", err)
        return NewPollBackend(b.drainInterval).Start(ctx, b.getSpecs(), out)
    }
    defer watcher.Close()
    slog.Info("fsnotify backend started")

    tailers    := make(map[string]*FileTailer)
    pathToJails := make(map[string][]string)
    dirty      := make(map[string]struct{})

    // parentDirs maps a watched parent directory to the glob patterns it serves.
    parentDirs := make(map[string][]string)

    var drainTimerC <-chan time.Time  // nil = not running

    // initialScan: run filepath.Glob for all patterns, open tailers,
    // watch files AND parent directories.
    initialScan := func() {
        currentSpecs := b.getSpecs()
        globCache := make(map[string][]string)
        for _, spec := range currentSpecs {
            for _, pattern := range spec.Globs {
                if _, seen := globCache[pattern]; !seen {
                    paths, _ := filepath.Glob(pattern)
                    if paths == nil {
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
                    newPathToJails[p] = appendUniq(newPathToJails[p], spec.JailName)
                    if _, set := pathReadFromEnd[p]; !set {
                        pathReadFromEnd[p] = spec.ReadFromEnd
                    }
                }
                // Watch the parent directory for CREATE events.
                parent := globParentDir(pattern)
                if parent != "" {
                    parentDirs[parent] = appendUniq(parentDirs[parent], pattern)
                    _ = watcher.Add(parent)
                }
            }
        }
        for p := range newPathToJails {
            if _, ok := tailers[p]; !ok {
                ft, err := NewFileTailer(p, pathReadFromEnd[p])
                if err != nil {
                    continue
                }
                tailers[p] = ft
                dirty[p] = struct{}{}
                _ = watcher.Add(p)
            }
        }
        // Remove tailers for paths no longer matched.
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
            // debug logging (rate-limited) omitted for brevity
            select {
            case out <- RawLine{FilePath: p, Line: line, Jails: jails, EnqueueAt: time.Now()}:
            case <-ctx.Done():
            }
        }
    }

    handleCreate := func(name string) {
        // Case 1: a watched file was recreated (rotation).
        if _, ok := tailers[name]; ok {
            if err := tailers[name].Reopen(false); err != nil {
                tailers[name].Close()
                ft, err2 := NewFileTailer(name, false)
                if err2 != nil {
                    delete(tailers, name)
                    return
                }
                tailers[name] = ft
                _ = watcher.Add(name)
            }
            dirty[name] = struct{}{}
            return
        }

        // Case 2: new file or directory in a watched parent dir.
        // Check if name matches any known glob pattern directly.
        for _, spec := range b.getSpecs() {
            for _, pattern := range spec.Globs {
                matched, _ := filepath.Match(pattern, name)
                if matched {
                    ft, err := NewFileTailer(name, false)
                    if err != nil {
                        continue
                    }
                    tailers[name] = ft
                    pathToJails[name] = appendUniq(pathToJails[name], spec.JailName)
                    _ = watcher.Add(name)
                    dirty[name] = struct{}{}
                    return
                }
            }
        }

        // Case 3: new directory — check if it's a parent of any glob pattern,
        // then run filepath.Glob for those patterns.
        if patterns, ok := parentDirs[name]; ok {
            _ = watcher.Add(name) // watch new subdir too
            for _, pattern := range patterns {
                paths, _ := filepath.Glob(pattern)
                for _, p := range paths {
                    if _, exists := tailers[p]; !exists {
                        for _, spec := range b.getSpecs() {
                            for _, g := range spec.Globs {
                                if g == pattern {
                                    ft, err := NewFileTailer(p, false)
                                    if err != nil {
                                        continue
                                    }
                                    tailers[p] = ft
                                    pathToJails[p] = appendUniq(pathToJails[p], spec.JailName)
                                    _ = watcher.Add(p)
                                    dirty[p] = struct{}{}
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    initialScan()

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
            case event.Has(fsnotify.Write):
                if _, known := pathToJails[event.Name]; known {
                    dirty[event.Name] = struct{}{}
                    if drainTimerC == nil {
                        t := time.NewTimer(b.drainInterval)
                        drainTimerC = t.C
                    }
                }
            }

        case <-drainTimerC:
            drainTimerC = nil  // disarm — will re-arm on next WRITE event
            for p := range dirty {
                readAndSend(p)
                delete(dirty, p)
            }

        case _, ok := <-watcher.Errors:
            if !ok {
                return nil
            }
        }
    }
}
```

**Helper function:**
```go
// globParentDir returns the longest path prefix before the first glob wildcard.
// For "/var/log/apache2/*/access.log" returns "/var/log/apache2".
func globParentDir(pattern string) string {
    for i, ch := range pattern {
        if ch == '*' || ch == '?' || ch == '[' {
            return filepath.Dir(pattern[:i])
        }
    }
    return filepath.Dir(pattern) // no wildcards: parent of the literal path
}

func appendUniq(slice []string, s string) []string {
    for _, v := range slice {
        if v == s {
            return slice
        }
    }
    return append(slice, s)
}
```

**Changes to `backend.go` / `NewAuto`:**
```go
// NewAuto now takes drainInterval for the fsnotify backend (= minLatency).
func NewAuto(mode string, interval time.Duration) Backend
```
Both `NewFsnotifyBackend` and `NewPollBackend` receive the same `interval` parameter; for poll it is the poll interval, for fsnotify it is the drain timer interval.

**Test changes:**

- `TestFsnotifyBackendBasic`: verify line is received within `minLatency + tolerance`.
- `TestFsnotifyBackendCoalescing`: WRITE many times in a burst, verify only one drain cycle fires (currently may over-read; new design drains once).
- New: `TestFsnotifyBackendIdleNoTimer`: start backend with a file, write nothing, verify no events emitted and no CPU wakeup occurs for at least 2× minLatency. (Indirect: verify that if we write after a long idle, the line arrives within minLatency.)
- `TestFsnotifyBackendSubdirGlob`: verify a new file created in a watched subdirectory is picked up without full rescan. Existing test should pass unchanged.
- Rotation test: write to file, verify lines received; truncate + rewrite file; verify new lines received.

---

### Unit 3: Optimize `cgroupCPUSampler` — keep fd open, pre-allocated buffer

**File:** `internal/engine/cpu_cgroup.go`

**Goal:** Eliminate per-sample open/close/alloc in `readUsageUsec()`.

**Changes:**

1. **Add `file *os.File` and `buf [512]byte` fields:**
   ```go
   type cgroupCPUSampler struct {
       cgroupPath string
       file       *os.File   // kept open; nil if unavailable
       buf        [512]byte  // pre-allocated, stack-like; avoids heap alloc
       lastUsage  int64
       lastTime   time.Time
       useCgroup  bool
       fallback   *cpuSampler
   }
   ```

2. **`newCgroupCPUSampler`:** open the file and store it in the struct. If open fails, set `useCgroup = false` and use fallback.

3. **`readUsageUsec()` — no longer opens/closes:**
   ```go
   func (s *cgroupCPUSampler) readUsageUsec() (int64, error) {
       if _, err := s.file.Seek(0, io.SeekStart); err != nil {
           return 0, err
       }
       n, err := s.file.Read(s.buf[:])
       if err != nil && err != io.EOF {
           return 0, err
       }
       data := s.buf[:n]
       // Parse: find "usage_usec " prefix, parse number to end of line
       const prefix = "usage_usec "
       idx := bytes.Index(data, []byte(prefix))
       if idx < 0 {
           return 0, fmt.Errorf("usage_usec not found")
       }
       start := idx + len(prefix)
       end := start
       for end < len(data) && data[end] >= '0' && data[end] <= '9' {
           end++
       }
       return strconv.ParseInt(string(data[start:end]), 10, 64)
   }
   ```

4. **Add `Close() error` method:** `s.file.Close()` — called from manager shutdown.

5. **`Sample()` unchanged in contract**, but now only opens once.

6. **`PerfMetrics`:** add `Close()` method that calls `sampler.Close()`. Call `perf.Close()` from `Manager` when shutting down.

7. **In `RecordExecution`:** only call `s.cpuSampler.Sample()` when `batchSize > 0` (non-empty batch), since that's when real work occurred.

**Test changes:**

- Existing perf/cpu tests should pass with no change (the interface is the same).
- Add a test that calls `Sample()` 100 times and verifies no heap allocation occurs (benchmark with `testing.AllocsPerRun`).

---

### Unit 4: Simplify `Manager.Run()` — remove batchQueue, enqueue goroutine, and manager timer

**File:** `internal/engine/manager.go`

**Prerequisite:** Unit 2 (backend now drives timing)

**Goal:** The manager no longer owns a timer. It reads lines from `rawLines` and processes them as they arrive from the backend's drain cycles.

**Changes:**

1. **Remove `batchQueue` struct and field from `Manager`.**

2. **Remove the enqueue goroutine** (the `go func()` that reads from `rawLines` and calls `m.queue.Enqueue`).

3. **Remove `timer` from `Run()`.**

4. **Rewrite the drain loop:**
   ```go
   for {
       select {
       case err := <-backendErr:
           if err != nil {
               return fmt.Errorf("watch backend: %w", err)
           }
           return nil
       case <-ctx.Done():
           // ... stop jails ...
           return ctx.Err()
       case line, ok := <-rawLines:
           if !ok {
               return nil
           }
           // Collect all lines the backend flushed in this drain cycle.
           batch := make([]watch.RawLine, 0, 64)
           batch = append(batch, line)
       drainMore:
           for {
               select {
               case l, ok := <-rawLines:
                   if !ok {
                       break drainMore
                   }
                   batch = append(batch, l)
               default:
                   break drainMore
               }
           }
           drainStart := time.Now()
           m.processBatch(ctx, batch)
           execTime := time.Since(drainStart)
           var latency time.Duration
           if len(batch) > 0 {
               latency = drainStart.Sub(batch[0].EnqueueAt)
           }
           m.perf.RecordExecution(execTime, latency, len(batch), m.currentInterval)
       }
   }
   ```

5. **`processBatch` signature change:** accepts `[]watch.RawLine` directly (no `pendingItem` wrapper):
   ```go
   func (m *Manager) processBatch(ctx context.Context, batch []watch.RawLine)
   ```
   Inside: replace `item.line.Jails` with `item.Jails`, etc.

6. **`NewAuto` call in `NewManager`:**
   ```go
   backend := watch.NewAuto(cfg.Engine.WatcherMode, minLatency)
   ```
   (Passes `minLatency` instead of `pollInterval` — both backends use this as their interval.)

7. **`adaptInterval` is retained** (it has its own unit tests). It is no longer called from `Run()`, but `currentInterval` is set to `minLatency` in `NewManager` and not modified. The perf API reports `currentInterval` as the current drain interval.

8. **Wire `perf.Close()`** into the `ctx.Done()` shutdown path.

**Test changes (`internal/engine/manager_test.go`):**

- Tests for `adaptInterval` (`TestAdaptInterval_*`) remain unchanged — they test the algorithm in isolation and remain valid.
- Integration tests (`internal/engine/engine_test.go`, `internal/engine/integration_test.go`) may need small adjustments due to `processBatch` signature change.
- Verify that `TestHandleEventInflightPreventsBatchRetrigger` continues to pass.

---

### Unit 5: Update `PollBackend` to use new `FileTailer` API

**File:** `internal/watch/poll.go`

**Prerequisite:** Unit 1 (new `ReadLines`, `CheckRotation()`)

**Goal:** Poll backend no longer relies on `ReadLines` for rotation detection.

**Changes:**

1. In the per-tick read loop, before calling `ft.ReadLines()`, call `ft.CheckRotation()`:
   ```go
   for p, ft := range tailers {
       pi, matched := pathInfos[p]
       if !matched {
           ft.Close()
           delete(tailers, p)
           continue
       }
       // Check rotation once per poll cycle.
       if _, err := ft.CheckRotation(); err != nil {
           continue
       }
       lines, err := ft.ReadLines()
       // ... rest unchanged
   }
   ```

2. No other changes needed. The poll backend's ticker continues to run at `b.interval` (now passed as `minLatency` from `NewAuto`).

**Test changes:**

- `TestPollBackendRotation` should continue to pass.
- No new tests needed for this unit.

---

### Unit 6: Wire `drainInterval` through backend construction

**File:** `internal/watch/backend.go`, `internal/engine/manager.go`

**Goal:** Ensure `minLatency` flows correctly to both backends.

**Changes to `internal/watch/backend.go`:**
```go
// NewAuto creates a Backend according to mode.
// interval is used as the poll interval for PollBackend and the drain timer
// interval for FsnotifyBackend (both default to minLatency = 2s).
func NewAuto(mode string, interval time.Duration) Backend {
    switch strings.ToLower(mode) {
    case "poll":
        return NewPollBackend(interval)
    case "fsnotify", "inotify", "os":
        return NewFsnotifyBackend(interval)
    default: // "auto"
        return NewFsnotifyBackend(interval)
    }
}
```

**Changes to `internal/engine/manager.go` (`NewManager`):**
```go
minLatency := cfg.Engine.MinLatency.Duration
if minLatency == 0 {
    minLatency = 2 * time.Second
}
backend := watch.NewAuto(cfg.Engine.WatcherMode, minLatency)
```
(Remove the separate `pollInterval` extraction — it is superseded by `minLatency`.)

---

## Test Strategy

### Unit tests to update

| File | Tests affected |
|------|---------------|
| `internal/watch/watch_test.go` | All fsnotify + poll tests — verify they still pass after signature/behavior changes |
| `internal/engine/manager_test.go` | `TestAdaptInterval_*` — untouched. May need to remove/update `processBatch` tests if any use `pendingItem`. |
| `internal/engine/engine_test.go` | `TestHandleEvent*`, `WaitForInflight` calls — adjust `processBatch` call if needed |
| `internal/engine/integration_test.go` | Adjust for removed enqueue goroutine |

### New tests to add

| Test | Location | What it verifies |
|------|----------|-----------------|
| `TestFileTailerNoSeekBetweenReads` | `watch_test.go` | ReadLines called twice without seek; correct lines returned |
| `TestFileTailerReopen` | `watch_test.go` | Reopen resets reader; subsequent ReadLines starts from position 0 |
| `TestFsnotifyBackendIdleNoDrain` | `watch_test.go` | With a file and no writes, no lines emitted for 2× minLatency |
| `TestCgroupCPUSamplerNoAlloc` | `engine` | AllocsPerRun(10, sampler.Sample) == 0 (or 1 for string conversion) |

### Regression test

Run `go test ./...` after each unit. All existing tests must pass.

---

## Migration Notes

- `NewAuto(mode, pollInterval)` signature changes to `NewAuto(mode, interval)`. This is a package-internal API; only `manager.go` calls it. Update `NewManager` accordingly.
- `processBatch` changes from `[]pendingItem` to `[]watch.RawLine`. Only called internally.
- `FileTailer.ReadLines` no longer handles rotation. Any code that calls `ReadLines` on a `FileTailer` directly (e.g., in `ConfigTest`) should be verified: since `ConfigTest` calls `ReadLines` once on a freshly opened tailer, it is unaffected.
- `FsnotifyBackend` has `pollInterval` renamed to `drainInterval`. Only affects construction.

## Expected Outcome

After these changes:
- **Idle CPU: ~0%** — no goroutine wakeups when no files are being written
- **Active CPU (150 files × 8.7 lines/sec average):** < 1% of a single core
  - ~4–5 drain cycles/minute (most are idle, no-ops)
  - Each drain cycle reads only dirty files (typically 1–3 at a time)
  - Zero stat/seek/alloc per file per cycle
- **Latency:** ≤ `minLatency` (default 2 s) from file write to `HandleEvent` call
- **Memory:** no change to heap footprint (pre-allocated buffers replace per-call allocs)
