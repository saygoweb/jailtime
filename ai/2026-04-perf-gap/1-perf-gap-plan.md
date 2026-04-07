# jailtime Performance Gap Plan

## Problem Statement

In production (~150 Apache vhosts, ~5,000 lines/day each, 12 jails), `jailtimed` uses ~3× more CPU than fail2ban. Target: < 1% CPU on a 2-core / 4 GB machine.

---

## Root Causes (severity order)

| # | Severity | File | Issue | Impact |
|---|----------|------|-------|--------|
| RC-1 | CRITICAL | `fsnotify.go` | 50 ms `batchTicker` fires 72 k/hr when idle | Constant goroutine wakeups even with zero file changes |
| RC-2 | HIGH | `fsnotify.go` | `rescanTicker` runs `filepath.Glob` every 2 s | ~4,500 syscalls/min in steady state |
| RC-3 | HIGH | `tail.go` | `ReadLines` does `os.Stat` + `Seek` + `reader.Reset` every call | Up to 6,000 stat + 6,000 seek syscalls/sec on idle files |
| RC-4 | MEDIUM | `cpu_cgroup.go` | `readUsageUsec()` opens/closes fd + allocates Scanner each call | 20 open/close/alloc per second |
| RC-5 | MEDIUM | `manager.go` | Separate timer + batchQueue + enqueue goroutine | Extra goroutine, mutex, latency jitter |
| RC-6 | LOW | `perf.go` | CPU sampled on every timer fire including idle | Wasted work; fixed by RC-1 (timer only fires when dirty) |

---

## Target Architecture

**One goroutine.** The backend owns the only `select` loop. The Manager is a pure processing unit called synchronously by the backend — no goroutine, no timer, no channel.

```
fsnotify kernel events
    │
    ▼
Backend.Start() — THE single goroutine, one select loop:

  var drainTimerC <-chan time.Time   // nil = idle (blocks forever in select)
  var lastDrainTime time.Duration   // measured duration of previous drain

  select {
    case <-ctx.Done():
        cleanup, return

    case event := <-watcher.Events:
        CREATE → check glob match → open tailer + watch (new file or rotation)
        WRITE  → dirty[path] = true
                 if drainTimerC == nil:
                     wait = max(targetLatency - lastDrainTime, 1ms)
                     drainTimerC = time.NewTimer(wait).C

    case <-drainTimerC:
        drainTimerC = nil                       // disarm; re-arms on next WRITE
        batch = collect lines from dirty files   // ReadLines per dirty path
        drain(ctx, batch)                        // ← Manager.processDrain inline
        lastDrainTime = measured wall time
  }

Manager.processDrain() — called inline by backend, same goroutine:
    currentInterval = time.Since(lastDrainAt)   // measure inter-drain wall time
    processBatch(ctx, lines):
        for each line → regex match → HitTracker
        threshold hit → ActionRunner.Submit(ip, fn)   ← non-blocking
    perf.RecordExecution(execTime, currentInterval, batchSize)

ActionRunner.Submit(ip, fn):
    if inflight[ip] → DROP duplicate, log, return false
    else → mark inflight, go func(){ defer delete; fn() }()

Manager.Run():
    start jails → build specs → backend.Start(ctx, specs, m.processDrain)
```

**Properties:**
- Idle = zero wakeups (no tickers, nil timer channel)
- One timer only — armed on WRITE, disarmed after drain, never auto-repeats
- `ReadLines` = pure `bufio.ReadString('\n')` — no stat, no seek
- Actions never block the backend — goroutine per (jail, ip), duplicates dropped
- `currentInterval ≈ executionTime + (targetLatency - executionTime) ≈ targetLatency`

---

## Execution Order and Dependencies

```
Phase 1 (parallel — no interdependencies):
  ┌─ Unit 1: FileTailer refactor (tail.go)
  ├─ Unit 3: cgroupCPUSampler optimisation (cpu_cgroup.go, perf.go)
  └─ Unit 7: ActionRunner extraction (jail_runtime.go)

Phase 2 (depends on Unit 1):
  ┌─ Unit 2: FsnotifyBackend rewrite (fsnotify.go, backend.go)
  └─ Unit 5: PollBackend update (poll.go)

Phase 3 (depends on Unit 2):
  └─ Unit 4: Manager simplification + config changes (manager.go, types.go, load.go, config_test.go)

Run `go test ./...` after each unit.
```

**Sub-agent assignment strategy:**
- Phase 1: launch 3 sub-agents in parallel (Units 1, 3, 7)
- Phase 2: after Phase 1 completes, launch 2 sub-agents in parallel (Units 2, 5)
- Phase 3: after Phase 2 completes, launch 1 sub-agent (Unit 4)

---

## Unit 1: Refactor `FileTailer`

**Files:** `internal/watch/tail.go`, `internal/watch/watch_test.go`
**Dependencies:** None
**Parallel with:** Units 3, 7

### Problem
`ReadLines` calls `os.Stat` + `Seek` + `reader.Reset` on every invocation. The `bufio.Reader` already holds partial line data in its internal buffer; seeking back and resetting discards it needlessly.

### Changes to `internal/watch/tail.go`

**1. Simplify `ReadLines`** — remove all stat/seek/reset:

```go
func (ft *FileTailer) ReadLines() ([]string, error) {
    var lines []string
    for {
        line, err := ft.reader.ReadString('\n')
        if len(line) > 0 && line[len(line)-1] == '\n' {
            lines = append(lines, line[:len(line)-1])
            ft.offset += int64(len(line))
        } else {
            break // partial line — stays in bufio buffer for next call
        }
        if err != nil {
            break
        }
    }
    return lines, nil
}
```

**2. Add `Reopen` method** — for rotation handling (called externally):

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
    if info, err := f.Stat(); err == nil {
        if st, ok := info.Sys().(*syscall.Stat_t); ok {
            ft.inode = st.Ino
        }
    }
    return nil
}
```

**3. Add `CheckRotation` method** — used by poll backend:

```go
func (ft *FileTailer) CheckRotation() (bool, error) {
    info, err := os.Stat(ft.path)
    if err != nil {
        return false, nil // file disappeared; caller handles
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

**4. `NewFileTailer` unchanged** — still stats at creation for initial inode.

### Tests

**Existing (must still pass):**
- `TestPollBackendRotation` — rotation now via `CheckRotation()` in poll backend (updated in Unit 5)

**New tests to add:**
- `TestFileTailerNoSeekBetweenReads`: write 3 lines → `ReadLines` → get 3; write 2 more → `ReadLines` → get 2. Verifies no seek needed between calls.
- `TestFileTailerReopen`: write lines, read them, call `Reopen(false)`, read again from start. Verify all original lines re-read.

---

## Unit 2: Rewrite `FsnotifyBackend`

**Files:** `internal/watch/fsnotify.go`, `internal/watch/backend.go`
**Dependencies:** Unit 1
**Parallel with:** Unit 5

### Problem
- 50 ms `batchTicker` fires constantly (RC-1)
- 2 s `rescanTicker` runs `filepath.Glob` constantly (RC-2)

### Changes to `internal/watch/backend.go`

**1. Add `DrainFunc` type and update `Backend` interface:**

```go
// DrainFunc is called synchronously by the backend during each drain cycle.
// lines contains all new RawLines from dirty files. Runs in the backend goroutine.
type DrainFunc func(ctx context.Context, lines []RawLine)

type Backend interface {
    Name() string
    Start(ctx context.Context, specs []WatchSpec, drain DrainFunc) error
    UpdateSpecs(specs []WatchSpec)
}
```

**2. Update `NewAuto`** — rename `pollInterval` parameter to `interval`:

```go
func NewAuto(mode string, interval time.Duration) Backend {
    switch mode {
    case "poll":
        slog.Info("watch backend selected", "requested_mode", mode, "backend", "poll")
        return NewPollBackend(interval)
    default:
        slog.Info("watch backend selected", "requested_mode", mode, "backend", "fsnotify")
        return NewFsnotifyBackend(interval)
    }
}
```

### Changes to `internal/watch/fsnotify.go`

**1. Rename field:** `pollInterval` → `drainInterval`

**2. Update constructor:** `NewFsnotifyBackend(drainInterval time.Duration)`

**3. Change `Start` signature:** `Start(ctx context.Context, specs []WatchSpec, drain DrainFunc) error`

**4. Replace both tickers with lazy one-shot drain timer + CREATE-driven discovery.** Full implementation:

```go
func (b *FsnotifyBackend) Start(ctx context.Context, specs []WatchSpec, drain DrainFunc) error {
    b.UpdateSpecs(specs)

    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        slog.Info("fsnotify unavailable, falling back to poll", "error", err)
        return NewPollBackend(b.drainInterval).Start(ctx, b.getSpecs(), drain)
    }
    defer watcher.Close()
    slog.Info("fsnotify backend started")

    tailers     := make(map[string]*FileTailer)
    pathToJails := make(map[string][]string)
    dirty       := make(map[string]struct{})
    parentDirs  := make(map[string][]string) // parent dir → glob patterns

    var drainTimerC <-chan time.Time          // nil = idle
    var lastDrainTime time.Duration          // previous drain wall time

    initialScan := func() { /* see below */ }
    readLines   := func(p string) []RawLine { /* see below */ }
    handleCreate := func(name string) { /* see below */ }

    initialScan()

    for {
        select {
        case <-ctx.Done():
            for _, ft := range tailers { ft.Close() }
            return ctx.Err()

        case event, ok := <-watcher.Events:
            if !ok { return nil }
            switch {
            case event.Has(fsnotify.Create):
                handleCreate(event.Name)
            case event.Has(fsnotify.Write):
                if _, known := pathToJails[event.Name]; known {
                    dirty[event.Name] = struct{}{}
                    if drainTimerC == nil {
                        wait := b.drainInterval - lastDrainTime
                        if wait < time.Millisecond { wait = time.Millisecond }
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
            if !ok { return nil }
        }
    }
}
```

**`initialScan` function:**
- Run `filepath.Glob` for all spec patterns (cached per unique pattern).
- Build `pathToJails` map.
- Open `FileTailer` for each matched path, mark dirty, add to fsnotify watcher.
- Compute `globParentDir(pattern)` for each glob → watch parent dirs for CREATE events.

**`handleCreate` function** — three cases:
1. **Known file recreated** (rotation): call `tailers[name].Reopen(false)`, mark dirty.
2. **New file matching a glob**: check `filepath.Match(pattern, name)` → open tailer, add to watcher, mark dirty.
3. **New directory**: if it matches a parent dir, run `filepath.Glob` for those patterns, open tailers for any new matches.

**`readLines` function:**
- Call `ft.ReadLines()` on the tailer.
- Return `[]RawLine` with `EnqueueAt: time.Now()` for each line.

**Helper functions (same file):**
```go
func globParentDir(pattern string) string {
    for i, ch := range pattern {
        if ch == '*' || ch == '?' || ch == '[' {
            return filepath.Dir(pattern[:i])
        }
    }
    return filepath.Dir(pattern)
}

func appendUniq(slice []string, s string) []string {
    for _, v := range slice {
        if v == s { return slice }
    }
    return append(slice, s)
}
```

### Tests

**Existing (must still pass, may need signature updates for `DrainFunc`):**
- `TestFsnotifyBackendBasic` — update to use `DrainFunc` callback instead of channel
- `TestFsnotifyBackendSubdirGlob` — same
- `TestFsnotifyBackendSharedFile` — same
- `TestFsnotifyBackendCoalescing` — same

**Note on test helper:** The existing `startBackend(t, b, specs)` helper creates a channel. Replace with:
```go
func startBackendDrain(t *testing.T, b Backend, specs []WatchSpec) (chan RawLine, context.CancelFunc) {
    t.Helper()
    out := make(chan RawLine, 64)
    ctx, cancel := context.WithCancel(context.Background())
    go func() {
        _ = b.Start(ctx, specs, func(_ context.Context, lines []RawLine) {
            for _, l := range lines {
                out <- l
            }
        })
    }()
    return out, cancel
}
```
All existing tests that use `startBackend` switch to `startBackendDrain`. The `waitEvent` helper is unchanged.

**New tests:**
- `TestFsnotifyBackendIdleNoDrain`: start backend with a file, write nothing for 2× `targetLatency`, verify no drain callback invoked. Then write a line, verify it arrives within `targetLatency + tolerance`.

---

## Unit 3: Optimise `cgroupCPUSampler`

**Files:** `internal/engine/cpu_cgroup.go`, `internal/engine/perf.go`
**Dependencies:** None
**Parallel with:** Units 1, 7

### Problem
`readUsageUsec()` opens, allocates a Scanner, reads, and closes `/sys/fs/cgroup/.../cpu.stat` on every call (RC-4). CPU is also sampled on idle drain cycles (RC-6).

### Changes to `internal/engine/cpu_cgroup.go`

**1. Keep fd open + pre-allocated buffer:**

```go
type cgroupCPUSampler struct {
    cgroupPath string
    file       *os.File    // kept open; nil if unavailable
    buf        [512]byte   // pre-allocated read buffer
    lastUsage  int64
    lastTime   time.Time
    useCgroup  bool
    fallback   *cpuSampler
}
```

**2. `newCgroupCPUSampler`:** open the file once and store it. If open fails → fallback.

**3. Rewrite `readUsageUsec`** — seek + read + manual parse (no Scanner):

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

**4. Add `Close() error` method:** closes the file. Called from `PerfMetrics.Close()`.

**5. `Sample()` unchanged in contract.**

### Changes to `internal/engine/perf.go`

**1. Add `Close()` to `PerfMetrics`:** calls `p.cpuSampler.Close()`.

**2. In `RecordExecution`:** only call `p.cpuSampler.Sample()` when `batchSize > 0`.

### Tests

**Existing:** perf/cpu tests should pass unchanged (interface is the same).

**New:** `TestCgroupCPUSamplerNoAlloc` — benchmark `testing.AllocsPerRun` verifies zero (or ≤1 for `string(data[start:end])`) heap allocations per `Sample()` call.

---

## Unit 4: Simplify Manager + Config Changes

**Files:** `internal/engine/manager.go`, `internal/engine/manager_test.go`, `internal/config/types.go`, `internal/config/load.go`, `internal/config/config_test.go`
**Dependencies:** Unit 2 (backend `DrainFunc` interface)
**Parallel with:** None (final unit)

### Problem
Manager has its own timer, batchQueue, enqueue goroutine — all redundant now that the backend calls `processDrain` synchronously (RC-5).

### Changes to `internal/engine/manager.go`

**1. Remove `batchQueue` struct** (lines 16–33) and `queue batchQueue` field from `Manager`.

**2. Remove `emaAlpha` constant.**

**3. Delete `adaptInterval` method** entirely.

**4. Add fields to `Manager`:**
```go
lastDrainAt     time.Time
currentInterval time.Duration
targetLatency   time.Duration
```

**5. Update `NewManager`:**
```go
targetLatency := cfg.Engine.TargetLatency.Duration
if targetLatency == 0 {
    targetLatency = 2000 * time.Millisecond
}
backend := watch.NewAuto(cfg.Engine.WatcherMode, targetLatency)
```
Remove `pollInterval` extraction. Remove `minLatency`/`maxLatency` fields.

**6. Rewrite `Run()`:**
```go
func (m *Manager) Run(ctx context.Context) error {
    for name, jr := range m.jails {
        if !jr.cfg.Enabled { continue }
        if err := jr.Start(ctx); err != nil {
            return fmt.Errorf("starting jail %q: %w", name, err)
        }
    }
    m.mu.RLock()
    specs := buildSpecs(m.jails, m.cfg.Engine.ReadFromEnd)
    m.mu.RUnlock()

    err := m.backend.Start(ctx, specs, m.processDrain)

    // Shutdown: stop all running jails.
    stopCtx := context.Background()
    m.mu.RLock()
    for _, jr := range m.jails {
        if jr.Status() == StatusStarted {
            _ = jr.Stop(stopCtx)
        }
    }
    m.mu.RUnlock()
    m.perf.Close()

    if err != nil && err != context.Canceled {
        return fmt.Errorf("watch backend: %w", err)
    }
    return nil
}
```

**7. Add `processDrain` method:**
```go
func (m *Manager) processDrain(ctx context.Context, lines []watch.RawLine) {
    drainStart := time.Now()
    if !m.lastDrainAt.IsZero() {
        m.currentInterval = drainStart.Sub(m.lastDrainAt)
    }
    m.lastDrainAt = drainStart

    m.processBatch(ctx, lines)

    execTime := time.Since(drainStart)
    m.perf.RecordExecution(execTime, m.currentInterval, len(lines))
}
```

**8. Update `processBatch` signature** — `[]watch.RawLine` instead of `[]pendingItem`:
```go
func (m *Manager) processBatch(ctx context.Context, lines []watch.RawLine)
```
Replace `item.line.Jails` with `line.Jails`, `item.line.FilePath` with `line.FilePath`, etc.

**9. Update `RecordExecution` call sites** — the `currentDelay` parameter is replaced by `currentInterval`:
- In `perf.go`, rename `currentDelay` to `currentInterval` in `RecordExecution` signature and `PerfSnapshot`.
- `PerfSnapshot.CurrentDelayMs` → `CurrentIntervalMs` (update JSON tag too).

### Changes to `internal/config/types.go`

**1. Rename field:** `MinLatency Duration` → `TargetLatency Duration` (YAML: `target_latency`)
**2. Remove field:** `MaxLatency Duration`
**3. Update defaults:**
```go
const defaultTargetLatency = 2000 * time.Millisecond
// Remove: defaultMaxLatency
```
**4. Update `EngineConfig.LogValue()`:** replace `min_latency`/`max_latency` with `target_latency`.

### Changes to `internal/config/load.go`

**1. Update `rawEngineConfig`:** rename `MinLatency` → `TargetLatency`, remove `MaxLatency`.
**2. Update default application** in `Load()`: apply `defaultTargetLatency`, remove `maxLatency` logic.
**3. Remove the `maxLatency < minLatency` validation**.

### Changes to `internal/config/config_test.go`

- `TestLoadDefaults`: change `MinLatency`/`MaxLatency` assertions → `TargetLatency`.
- `TestEngineConfigOverrides` (the test with `min_latency: 500ms`, `max_latency: 30s`): change to `target_latency: 500ms`, remove max.
- **Delete** `TestValidateMaxLatencyLessThanMinLatency` (validation no longer exists).

### Changes to `internal/engine/manager_test.go`

- **Delete all** `TestAdaptInterval_*` tests (7 tests).
- **Add** `TestManagerCurrentInterval`: create a `Manager` with a mock backend whose `Start()` calls the drain callback twice with a known sleep between calls. Verify `currentInterval` ≈ sleep duration.

### Changes to `internal/engine/integration_test.go`

- Update any test that creates a backend + channel to use the new `DrainFunc` pattern.

---

## Unit 5: Update `PollBackend`

**Files:** `internal/watch/poll.go`
**Dependencies:** Unit 1 (`CheckRotation`), Unit 2 (`DrainFunc` in `backend.go`)
**Parallel with:** Unit 2 (can start together once Unit 1 is done, but needs `backend.go` changes from Unit 2)

**Note:** If running in parallel with Unit 2, the sub-agent should import the `DrainFunc` type from `backend.go` (which Unit 2 creates). Alternatively, run after Unit 2 since it is a small change.

### Changes to `internal/watch/poll.go`

**1. Update `Start` signature:** `Start(ctx context.Context, specs []WatchSpec, drain DrainFunc) error`

**2. Add `CheckRotation` before `ReadLines`:**
```go
if _, err := ft.CheckRotation(); err != nil {
    continue
}
```

**3. Collect lines into batch, call `drain` once per tick:**
```go
var batch []RawLine
for p, ft := range tailers {
    pi, matched := pathInfos[p]
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
    now := time.Now()
    for _, line := range lines {
        batch = append(batch, RawLine{FilePath: p, Line: line, Jails: pi.jails, EnqueueAt: now})
    }
}
if len(batch) > 0 {
    drain(ctx, batch)
}
```

**4. Remove the per-line channel send** — replaced by batch collection + drain call.

### Tests

All existing poll backend tests (`TestPollBackend*`) must pass. They will use the updated `startBackendDrain` helper from Unit 2.

---

## Unit 7: Formalise `ActionRunner`

**Files:** `internal/engine/jail_runtime.go`, `internal/engine/engine_test.go`
**Dependencies:** None
**Parallel with:** Units 1, 3

### Problem
`JailRuntime` uses inline `inflight sync.Map` + `inflightWg sync.WaitGroup` + goroutine management for action deduplication. This works (from fix/multi-trigger) but should be a named type for clarity.

### Changes to `internal/engine/jail_runtime.go`

**1. Add `ActionRunner` type:**

```go
// ActionRunner manages non-blocking, deduplicated execution of on_match actions.
// At most one action per IP is in flight at any time.
// Duplicate submits are dropped.
type ActionRunner struct {
    inflight sync.Map
    wg       sync.WaitGroup
}

// Submit runs fn in a goroutine for the given ip. Returns false if an action
// for this ip is already in flight (duplicate dropped).
func (r *ActionRunner) Submit(ip string, fn func()) bool {
    if _, exists := r.inflight.LoadOrStore(ip, struct{}{}); exists {
        return false
    }
    r.wg.Add(1)
    go func() {
        defer r.wg.Done()
        defer r.inflight.Delete(ip)
        fn()
    }()
    return true
}

// Wait blocks until all in-flight actions complete.
func (r *ActionRunner) Wait() {
    r.wg.Wait()
}
```

**2. Update `JailRuntime` struct:**
- Remove `inflight sync.Map` and `inflightWg sync.WaitGroup` fields.
- Add `runner ActionRunner` field.

**3. Update `HandleEvent`** — replace manual goroutine management:

```go
submitted := jr.runner.Submit(result.IP, func() {
    cfg := jr.cfg
    // Query pre-check (if enabled)
    if cfg.QueryBeforeMatch && queryTmpl != nil {
        res, _ := action.RunCompiled(ctx, queryTmpl, actCtx, cfg.ActionTimeout.Duration)
        if res.ExitCode == 0 && res.Error == nil {
            slog.Info("query pre-check suppressed on_match",
                "jail", cfg.Name, "ip", result.IP)
            return
        }
    }
    if _, err := action.RunAllCompiled(ctx, onMatchTmpls, actCtx, cfg.ActionTimeout.Duration); err != nil {
        slog.Warn("on_match action failed", "jail", cfg.Name, "ip", result.IP, "error", err)
    }
})
if !submitted {
    slog.Info("on_match already in flight for ip, duplicate dropped",
        "jail", jr.cfg.Name, "ip", result.IP)
}
```

**4. Update `WaitForInflight`:**
```go
func (jr *JailRuntime) WaitForInflight() {
    jr.runner.Wait()
}
```

### Tests

**Existing (must pass unchanged in semantics):**
- `TestHandleEventInflightPreventsBatchRetrigger` — tests drop-duplicate; uses `WaitForInflight()`.
- All other `TestHandleEvent*` tests that call `WaitForInflight()`.

**New tests:**
- `TestActionRunnerDrop`: submit IP "1.2.3.4" with a slow fn (sleeps 100ms). Immediately submit same IP again. Verify second returns `false`. Wait. Verify fn ran exactly once.
- `TestActionRunnerSequential`: submit IP, wait for completion, submit same IP again. Verify both run (second is not blocked by first's completed entry).

---

## Test Strategy Summary

### Tests to delete
| Test | File | Reason |
|------|------|--------|
| `TestAdaptInterval_IdleCapAt2xMin` | `manager_test.go` | `adaptInterval` deleted |
| `TestAdaptInterval_HighLatencySnapToMin` | `manager_test.go` | `adaptInterval` deleted |
| `TestAdaptInterval_ModeratelyHighLatencyReduces` | `manager_test.go` | `adaptInterval` deleted |
| `TestAdaptInterval_LowLatencyGrows` | `manager_test.go` | `adaptInterval` deleted |
| `TestAdaptInterval_NeverExceedsMaxLatency` | `manager_test.go` | `adaptInterval` deleted |
| `TestAdaptInterval_NeverBelowMinLatency` | `manager_test.go` | `adaptInterval` deleted |
| `TestAdaptInterval_IdleThenBusyRecovery` | `manager_test.go` | `adaptInterval` deleted |
| `TestValidateMaxLatencyLessThanMinLatency` | `config_test.go` | validation removed |

### New tests to add
| Test | File | Unit |
|------|------|------|
| `TestFileTailerNoSeekBetweenReads` | `watch_test.go` | 1 |
| `TestFileTailerReopen` | `watch_test.go` | 1 |
| `TestFsnotifyBackendIdleNoDrain` | `watch_test.go` | 2 |
| `TestCgroupCPUSamplerNoAlloc` | engine test | 3 |
| `TestManagerCurrentInterval` | `manager_test.go` | 4 |
| `TestActionRunnerDrop` | `engine_test.go` | 7 |
| `TestActionRunnerSequential` | `engine_test.go` | 7 |

### Regression
Run `go test ./...` after each unit. All 63 existing tests (minus 8 deleted) must pass.

---

## Migration Notes

| Change | Scope | Who needs updating |
|--------|-------|-------------------|
| `Backend.Start` takes `DrainFunc` instead of `chan<- RawLine` | `backend.go` interface | `manager.go`, all watch tests |
| `NewAuto(mode, interval)` parameter renamed | `backend.go` | `manager.go` only |
| `processBatch` takes `[]watch.RawLine` not `[]pendingItem` | `manager.go` | Internal only |
| `ReadLines` no longer handles rotation | `tail.go` | `fsnotify.go` (CREATE handler), `poll.go` (CheckRotation) |
| `MinLatency`/`MaxLatency` → `TargetLatency` | `types.go`, `load.go` | `config_test.go`, `manager.go`, sample YAML files |
| `adaptInterval` + `emaAlpha` deleted | `manager.go` | `manager_test.go` |
| `inflight`/`inflightWg` → `ActionRunner` | `jail_runtime.go` | `engine_test.go` (via `WaitForInflight` — no change) |
| `PerfSnapshot.CurrentDelayMs` → `CurrentIntervalMs` | `perf.go` | `control/api.go`, `cmd/jailtime/main.go` |
| `RecordExecution` signature change | `perf.go` | `manager.go` only |

---

## Expected Outcome

| Metric | Before | After |
|--------|--------|-------|
| Idle CPU | ~0.3% (72k wakeups/hr) | ~0% (zero wakeups) |
| Active CPU (150 files, 8.7 lines/sec) | ~1.5% | < 0.5% |
| Steady-state syscalls/min | ~4,500 glob + 6,000 stat/seek | ~0 (event-driven) |
| Latency (file write → HandleEvent) | 50 ms–10 s (variable) | ≈ `targetLatency` (default 2000 ms) |
| Heap allocs per drain cycle | Scanner + bufio + fd per file | Zero (pre-allocated) |
