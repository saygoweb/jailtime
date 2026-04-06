# Jailtime Optimization Plan

## Goals & Priorities

1. **Very low CPU usage** — target: < 1% of 2 cores (< 0.5% per core) with a dozen jails and hundreds of files
2. **Low memory usage** — minimize allocations on hot paths; reuse buffers
3. **Acceptable latency** — up to 10 seconds is tolerable; latency is the time from a file-change trigger to execution of the queued task processing that trigger

## Current Architecture Summary

```
Watch Backend (fsnotify or poll)
  → Event Channel (65536 buf)
    → Manager.Run event loop
      → CPU throttle check
        → Task Queue (4096 buf)
          → Worker Pool (GOMAXPROCS*4 goroutines)
            → JailRuntime.HandleEvent()
              → filter.Match() → HitTracker.Record() → action.RunAll()
```

**Current flow:** Watch backends (poll every 2s, or fsnotify with 50ms coalesce) emit one `Event` per line per jail. The manager routes events through a worker pool. CPU throttling is reactive — it measures Go runtime CPU and adds dispatch delays when exceeding 2%.

### Key Observations from Current Implementation

| Aspect | Current State | Issue |
|--------|--------------|-------|
| Glob expansion | Every poll cycle, all jails, all patterns | Redundant work if multiple jails share patterns |
| File tailers | One per unique file path (shared) ✓ | Good — no duplicate reads |
| Fsnotify coalescing | 50ms batch interval | Good but hard-coded; not adaptive |
| Event fan-out | One Event per line per watching jail | Events duplicated across jails sharing files |
| Worker pool | GOMAXPROCS*4 goroutines always alive | Over-provisioned for low-CPU goal |
| Filter compilation | Once at startup ✓ | Good |
| Template parsing | Re-parsed every action.Run() call | Wasteful on hot path (though infrequent) |
| HitTracker | Single mutex for all IPs in a jail | Contention under high match rates |
| CPU sampling | Go runtime metrics (`/cpu/classes/*`) | Measures process CPU, not cgroup CPU |
| Dispatch delay | Reactive, after CPU exceeds 2% | Does not provide proactive pacing |

---

## Comparison with fail2ban Architecture

fail2ban is the reference implementation for log-based intrusion prevention. Key architectural differences:

| Aspect | fail2ban | jailtime (current) | jailtime (proposed) |
|--------|----------|--------------------|--------------------|
| **Event model** | Ticket queue: Filter → FailTicket → FailManager → BanTicket | Direct event → HandleEvent pipeline | Task queue with timer-based drain |
| **Aggregation** | FailManager deduplicates by IP (dict lookup O(1)) | HitTracker sliding window per IP | Keep, but reduce lock contention |
| **Backend** | pyinotify (event-driven) or poll | fsnotify or poll | Same, but with shared glob dedup |
| **Regex** | Compiled once, cached on Filter object | Compiled once per JailRuntime ✓ | Add cross-jail regex dedup |
| **Thread model** | One filter thread + one action thread per jail | Shared worker pool (GOMAXPROCS*4) | Reduce workers; timer-paced execution |
| **CPU control** | Implicit (Python GIL + sleep between polls) | Reactive CPU throttle | Proactive: configurable min/max latency timer |
| **Memory** | `__slots__` on Ticket classes | Standard Go structs | Pool Event structs; minimize allocs |

**Key takeaway from fail2ban:** The ticket/queue model naturally decouples detection from processing. fail2ban's efficiency comes from: (a) compiled regex reuse, (b) IP deduplication in FailManager, (c) event-driven inotify with no wasted polls, and (d) minimal CPU through sleep-based pacing. jailtime already has (a) and partial (c). The optimization plan adds (b) improvements and (d) through the timer-based architecture.

---

## Proposed Architecture: Timer-Based Task Queue

### Core Concept

Replace the current "event → immediate dispatch to worker" model with a **two-phase architecture**:

1. **Phase 1 — Collect:** Watch backends and event sources enqueue tasks into a pending queue
2. **Phase 2 — Execute:** A timer fires at adaptive intervals and drains the queue in a batch

```
Watch Backend (fsnotify / poll)
  → lines read from files
    → enqueue RawLine{file, line, jails[]}
      ↓
  [ Pending Queue ]
      ↓
  Timer (adaptive: min_latency..max_latency)
      ↓
  Batch Drain:
    for each RawLine:
      for each jail:
        compiled regex match
        → hit tracking
        → action execution (if threshold)
```

### Adaptive Timer Design

```
                    ┌─────────────────────────────────┐
                    │     Timer Interval Adapter       │
                    │                                  │
                    │  if queue_empty for N cycles:    │
                    │    interval = min(interval*2,    │
                    │                   max_latency)   │
                    │                                  │
                    │  if queue_non_empty:             │
                    │    interval = min_latency        │
                    │                                  │
                    │  measure: execution_time of      │
                    │    last batch drain              │
                    │  if exec_time > interval * 0.5:  │
                    │    interval = min(interval*1.5,  │
                    │                   max_latency)   │
                    └─────────────────────────────────┘
```

**Configuration:**
```yaml
engine:
  min_latency: 2s      # Minimum time between batch executions (default)
  max_latency: 10s     # Maximum time between batch executions (default)
```

**Latency measurement:** The internal latency is `time.Now()` at drain start minus the `time.Now()` that was recorded when the item was enqueued. This is purely internal processing latency — the log-line timestamp is irrelevant.

**Timer resolution:** Use `time.NewTimer` with millisecond resolution (Go timers have nanosecond resolution internally; millisecond is more than sufficient). Reset the timer after each drain.

### Benefits

- **CPU savings:** No work when queue is empty — timer backs off to max_latency. Under normal load (few matches), the system barely wakes.
- **Batch efficiency:** Processing multiple events per wake reduces goroutine scheduling overhead.
- **Predictable pacing:** CPU usage becomes a function of `1/interval × work_per_drain` rather than being driven by log volume.
- **Measurable latency:** The enqueue timestamp vs drain timestamp gives exact internal latency.

---

## Detailed Optimization Plan

### Phase 1: Shared File Infrastructure (watch backends)

#### 1.1 — Deduplicate Glob Patterns Across Jails

**Problem:** If jails A and B both watch `/var/log/nginx/*/access.log`, the glob is expanded twice per poll cycle.

**Solution:** In `buildSpecs()` or within the backend, collect unique glob patterns before expansion. Map `glob_pattern → []jail_names`. Expand each unique glob once, then fan results to all interested jails.

**Implementation:**
```go
// In the watch backend's poll/rescan cycle:
type globResult struct {
    pattern string
    paths   []string
}

// Deduplicate: pattern → globResult
uniqueGlobs := make(map[string]*globResult)
for _, spec := range specs {
    for _, pattern := range spec.Globs {
        if _, exists := uniqueGlobs[pattern]; !exists {
            paths, _ := filepath.Glob(pattern)
            uniqueGlobs[pattern] = &globResult{pattern: pattern, paths: paths}
        }
    }
}
```

**Files changed:** `internal/watch/poll.go`, `internal/watch/fsnotify.go`

#### 1.2 — Shared File Tailers with Multi-Jail Fan-Out

**Current state:** File tailers are already shared per-path (good). But the fan-out creates separate `Event` structs per jail. 

**Optimization:** Instead of emitting N events for N jails watching the same file, emit a single `RawLine` struct that carries the list of interested jail names. The processing phase handles fan-out.

**New types:**
```go
// internal/watch/backend.go
type RawLine struct {
    FilePath  string
    Line      string
    Jails     []string   // All jails interested in this file
    EnqueueAt time.Time  // For latency measurement
}
```

This reduces channel pressure from `O(lines × jails_per_file)` to `O(lines)`.

**Files changed:** `internal/watch/backend.go`, `internal/watch/poll.go`, `internal/watch/fsnotify.go`, `internal/engine/manager.go`

#### 1.3 — Reduce Poll/Rescan Frequency for Glob Expansion

**Problem:** Glob expansion runs every 2 seconds even if no files are created/deleted.

**Optimization:** For fsnotify backend, watch parent directories for CREATE/DELETE events. Only re-expand globs when a directory event is received. For poll backend, increase the glob rescan interval independently of the file-read interval (e.g., rescan globs every 30s, read files every 2s).

**Files changed:** `internal/watch/fsnotify.go`, `internal/watch/poll.go`

### Phase 2: Timer-Based Execution Engine

#### 2.1 — Pending Queue & Batch Drain

Replace the current `events chan → worker pool` pipeline with:

```go
// internal/engine/manager.go
type pendingItem struct {
    line      watch.RawLine
    enqueueAt time.Time
}

type batchQueue struct {
    mu    sync.Mutex
    items []pendingItem
}

func (q *batchQueue) Enqueue(line watch.RawLine) {
    q.mu.Lock()
    q.items = append(q.items, pendingItem{
        line:      line,
        enqueueAt: time.Now(),
    })
    q.mu.Unlock()
}

func (q *batchQueue) Drain() []pendingItem {
    q.mu.Lock()
    batch := q.items
    q.items = q.items[:0] // Reuse backing array
    q.mu.Unlock()
    return batch
}
```

The timer fires, drains the queue, and processes all items sequentially (or with a small worker pool of 1-2 goroutines for action execution):

```go
func (m *Manager) runDrainLoop(ctx context.Context) {
    timer := time.NewTimer(m.minLatency)
    defer timer.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-timer.C:
            batch := m.queue.Drain()
            start := time.Now()
            m.processBatch(ctx, batch)
            execTime := time.Since(start)
            
            m.perf.RecordExecution(execTime, batch, start)
            nextInterval := m.adaptInterval(execTime, len(batch))
            timer.Reset(nextInterval)
        }
    }
}
```

**Files changed:** `internal/engine/manager.go` (major refactor of `Run`)

#### 2.2 — Adaptive Interval Logic

```go
func (m *Manager) adaptInterval(execTime time.Duration, batchSize int) time.Duration {
    if batchSize == 0 {
        // No work — back off toward max_latency
        m.currentInterval = min(m.currentInterval*2, m.maxLatency)
    } else {
        // Work done — use min_latency
        m.currentInterval = m.minLatency
        // But if execution took > 50% of interval, stretch it
        if execTime > m.currentInterval/2 {
            m.currentInterval = min(
                time.Duration(float64(execTime)*1.5),
                m.maxLatency,
            )
        }
    }
    return m.currentInterval
}
```

#### 2.3 — Batch Processing with Cross-Jail Fan-Out

```go
func (m *Manager) processBatch(ctx context.Context, batch []pendingItem) {
    for _, item := range batch {
        for _, jailName := range item.line.Jails {
            jr, exists := m.jails[jailName]
            if !exists || jr.Status() != StatusStarted {
                continue
            }
            // HandleEvent with the line
            evt := watch.Event{
                JailName: jailName,
                FilePath: item.line.FilePath,
                Line:     item.line.Line,
                Time:     item.line.EnqueueAt,
            }
            if err := jr.HandleEvent(ctx, evt); err != nil {
                slog.Warn("event processing error", "jail", jailName, "error", err)
            }
        }
    }
}
```

This processes events inline on the timer goroutine. For the target workload (< 1% CPU), inline processing avoids goroutine scheduling overhead. If action execution (shell commands) is slow, only the action execution needs to be async — filtering and hit tracking are fast.

**Refinement:** Split HandleEvent into two parts:
1. `MatchAndTrack(line) → (triggered bool, actCtx action.Context)` — always inline, fast
2. `ExecuteActions(ctx, actCtx)` — only when triggered; can be async if needed

#### 2.4 — Remove Worker Pool

The current `GOMAXPROCS*4` worker pool is over-provisioned for the CPU target. With timer-based batching:

- Filtering/matching runs inline on timer goroutine (fast: just regex match + map lookup)
- Action execution (shell commands) runs on a single dedicated goroutine with its own queue
- This reduces from `GOMAXPROCS*4` (e.g., 8 goroutines on 2-core) to 2-3 total goroutines

#### 2.5 — Configuration Changes

Add to `EngineConfig`:
```go
type EngineConfig struct {
    WatcherMode  string   `yaml:"watcher_mode"`
    PollInterval Duration `yaml:"poll_interval"`
    ReadFromEnd  bool     `yaml:"read_from_end"`
    MinLatency   Duration `yaml:"min_latency"`   // NEW: default 2s
    MaxLatency   Duration `yaml:"max_latency"`   // NEW: default 10s
}
```

**Files changed:** `internal/config/types.go`, `internal/config/load.go`, `internal/config/validate.go`

### Phase 3: Cross-Jail Regex Optimization

#### 3.1 — Compiled Regex Registry

**Problem:** If multiple jails use the same filter pattern (e.g., common SSH brute-force regex), the regex is compiled independently for each jail. More importantly, the same line may be matched against the same regex multiple times if multiple jails watch the same file.

**Solution:** Create a global `CompiledFilterRegistry` that deduplicates compiled regexes by pattern string:

```go
// internal/filter/registry.go
type Registry struct {
    mu      sync.RWMutex
    filters map[string]*CompiledFilter // pattern → compiled
}

func (r *Registry) Get(pattern string) (*CompiledFilter, error) {
    r.mu.RLock()
    if cf, ok := r.filters[pattern]; ok {
        r.mu.RUnlock()
        return cf, nil
    }
    r.mu.RUnlock()
    
    cf, err := Compile(pattern)
    if err != nil {
        return nil, err
    }
    
    r.mu.Lock()
    r.filters[pattern] = cf
    r.mu.Unlock()
    return cf, nil
}
```

**Benefit:** Memory savings when jails share patterns. More importantly, enables Phase 3.2.

**Files changed:** New file `internal/filter/registry.go`, modifications to `internal/engine/jail_runtime.go`

#### 3.2 — Match Cache for Shared Lines

When a line from a shared file is processed for multiple jails, and those jails have overlapping include/exclude filters, the same regex is evaluated against the same line multiple times.

**Optimization:** Per-batch match cache:
```go
// Keyed by (line_ptr, regex_ptr) → match result
type matchCache map[matchCacheKey]*MatchResult

type matchCacheKey struct {
    lineHash  uint64    // xxhash of line content
    regexAddr uintptr   // pointer identity of compiled regex
}
```

This cache lives only for the duration of a batch drain — no long-term memory growth. For a batch of 100 lines going to 10 jails with 50% regex overlap, this can halve regex evaluations.

**Files changed:** `internal/filter/match.go`, `internal/engine/manager.go`

### Phase 4: Micro-Optimizations

#### 4.1 — Template Pre-Compilation

**Problem:** `action.Render()` calls `template.New().Parse()` on every invocation.

**Solution:** Pre-compile templates at JailRuntime initialization:
```go
type JailRuntime struct {
    // ...
    onMatchTemplates []*template.Template  // pre-compiled
    queryTemplate    *template.Template     // pre-compiled
}
```

Parse once in `NewJailRuntime()` and `Reconfigure()`. Execute with `tpl.Execute(&buf, ctx)` in the hot path.

**Files changed:** `internal/action/template.go`, `internal/engine/jail_runtime.go`

#### 4.2 — HitTracker Lock Reduction

**Current:** Single `sync.Mutex` protects entire `map[string]*HitWindow`.

**Options (in order of complexity):**

1. **sync.Map** — lock-free reads, good for read-heavy workloads. But `Record()` is always a write.
2. **Sharded map** — partition by IP hash into N buckets, each with its own mutex. Reduces contention by factor of N.
3. **Per-IP atomic counters** — more complex, but eliminates mutex entirely.

**Recommended:** Option 2 (sharded map with 16 shards). Simple, effective, no API change:
```go
type HitTracker struct {
    shards [16]hitShard
}

type hitShard struct {
    mu      sync.Mutex
    windows map[string]*HitWindow
}

func (ht *HitTracker) shard(ip string) *hitShard {
    h := fnv.New32a()
    h.Write([]byte(ip))
    return &ht.shards[h.Sum32()%16]
}
```

**Files changed:** `internal/engine/hits.go`

#### 4.3 — Reduce Allocations in Event Path

- **Event struct pooling:** Use `sync.Pool` for `watch.Event` or the new `RawLine` if allocation profiling shows it's significant.
- **String interning for jail names:** Jail names are repeated on every event. Pre-intern them as constants.
- **Reuse `[]string` slices in ReadLines:** The `lines` slice is allocated fresh each call. Reuse via a field on `FileTailer`.

#### 4.4 — Debug Log Short-Circuit

**Current:** `slog.Default().Enabled(ctx, slog.LevelDebug)` is called on every non-matching line. While the check itself is cheap, it involves an interface call and context lookup.

**Optimization:** Cache the debug-enabled flag at JailRuntime initialization and on Reconfigure:
```go
type JailRuntime struct {
    debugEnabled bool // cached at init
}
```

Check `jr.debugEnabled` (a simple bool read, no interface dispatch) before any debug-path work.

### Phase 5: Performance API & Client Command

#### 5.1 — Performance Metrics Collector

New internal package or addition to engine:

```go
// internal/engine/perf.go
type PerfMetrics struct {
    mu sync.RWMutex

    // Configurable window size (default 3)
    windowSize int

    // Latency: time from enqueue to drain start
    latencies []time.Duration // circular buffer, last N

    // Execution delay: current timer interval
    currentDelay time.Duration

    // Execution times: duration of batch processing
    execTimes []time.Duration // circular buffer, last N

    // CPU: cgroup cpu usage samples
    cpuSamples []float64 // circular buffer, last N
}

type PerfSnapshot struct {
    CurrentLatencyMs  float64 `json:"current_latency_ms"`
    CurrentDelayMs    float64 `json:"current_delay_ms"`
    AvgExecTimeMs     float64 `json:"avg_exec_time_ms"`
    AvgCPUPercent     float64 `json:"avg_cpu_percent"`
    WindowSize        int     `json:"window_size"`
}
```

**Recording (called from drain loop):**
```go
func (p *PerfMetrics) RecordExecution(execTime time.Duration, batch []pendingItem, drainStart time.Time) {
    p.mu.Lock()
    defer p.mu.Unlock()

    // Record exec time
    p.pushExecTime(execTime)

    // Record latency from oldest item in batch
    if len(batch) > 0 {
        latency := drainStart.Sub(batch[0].enqueueAt)
        p.pushLatency(latency)
    }

    // Record current delay
    p.currentDelay = /* current timer interval */

    // Sample CPU from cgroup
    p.pushCPU(p.sampleCgroupCPU())
}
```

#### 5.2 — Cgroup CPU Sampling

Read directly from the cgroup v2 filesystem:

```go
// internal/engine/cpu_cgroup.go

const cgroupCPUPath = "/sys/fs/cgroup/system.slice/jailtimed.service/cpu.stat"

func readCgroupCPUUsage() (usageUsec int64, err error) {
    // Read /sys/fs/cgroup/system.slice/jailtimed.service/cpu.stat
    // Parse "usage_usec <value>" line
    data, err := os.ReadFile(cgroupCPUPath)
    // ... parse
}

type cgroupCPUSampler struct {
    lastUsage int64
    lastTime  time.Time
}

func (s *cgroupCPUSampler) Sample() float64 {
    usage, err := readCgroupCPUUsage()
    if err != nil {
        return 0 // graceful degradation
    }
    now := time.Now()
    elapsed := now.Sub(s.lastTime)
    if elapsed <= 0 {
        return 0
    }
    
    deltaUsec := usage - s.lastUsage
    // CPU% = delta_usage_usec / elapsed_usec * 100 / num_cpus
    cpuPercent := float64(deltaUsec) / float64(elapsed.Microseconds()) * 100.0
    
    s.lastUsage = usage
    s.lastTime = now
    return cpuPercent  // Percentage of one CPU core
}
```

**Note:** The path `/sys/fs/cgroup/system.slice/jailtimed.service/cpu.stat` assumes cgroup v2 with systemd. Make the service name configurable but default to `jailtimed.service`. If the file doesn't exist, fall back to Go runtime CPU metrics (current behavior).

**Files changed:** New `internal/engine/cpu_cgroup.go`, modified `internal/engine/perf.go`

#### 5.3 — API Endpoint

Following the existing endpoint chain pattern:

**1. API types** (`internal/control/api.go`):
```go
type PerfResponse struct {
    CurrentLatencyMs float64 `json:"current_latency_ms"`
    CurrentDelayMs   float64 `json:"current_delay_ms"`
    AvgExecTimeMs    float64 `json:"avg_exec_time_ms"`
    AvgCPUPercent    float64 `json:"avg_cpu_percent"`
    WindowSize       int     `json:"window_size"`
}
```

**2. Engine method** (`internal/engine/manager.go`):
```go
func (m *Manager) PerfStats() PerfSnapshot {
    return m.perf.Snapshot()
}
```

**3. Controller interface** (`internal/control/server.go`):
```go
type JailController interface {
    // ... existing methods ...
    PerfStats() PerfSnapshot
}
```

**4. HTTP handler** (`internal/control/server.go`):
```
GET /v1/perf → handlePerf
```

**5. Client method** (`internal/control/client.go`):
```go
func (c *Client) Perf() (*PerfResponse, error) {
    return get[PerfResponse](c, "/v1/perf")
}
```

**6. Adapter** (`cmd/jailtimed/main.go`):
```go
func (a *JailControllerAdapter) PerfStats() engine.PerfSnapshot {
    return a.m.PerfStats()
}
```

**7. CLI command** (`cmd/jailtime/main.go`):
```
jailtime perf
```

Output format:
```
Performance Metrics (window=3):
  Current latency:     45ms
  Current delay:       2000ms
  Avg execution time:  12ms
  Avg CPU usage:       0.3%
```

#### 5.4 — Configurable Window Size

Add to engine config:
```yaml
engine:
  perf_window: 3  # Number of recent samples for averaging (default 3)
```

### Phase 6: Remove CPU Throttle (Replaced by Timer)

The current reactive CPU throttle (`checkCPU()` with `dispatchDelay`) is no longer needed because:

1. The timer-based architecture inherently paces work
2. The adaptive interval naturally backs off when there's no work
3. The min/max latency bounds constrain CPU usage

The `cpuSampler` using Go runtime metrics can be removed from the dispatch loop. The cgroup-based CPU sampling in `PerfMetrics` replaces it for observability.

**Files changed:** `internal/engine/manager.go`, `internal/engine/cpu.go` (may be removed or repurposed)

---

## Implementation Order

The phases should be implemented in dependency order:

```
Phase 1.1  Deduplicate glob patterns
Phase 1.2  RawLine with multi-jail fan-out
Phase 1.3  Reduce glob rescan frequency
    ↓
Phase 2.1  Pending queue & batch drain
Phase 2.2  Adaptive interval logic
Phase 2.3  Batch processing with fan-out
Phase 2.4  Remove worker pool
Phase 2.5  Config: min_latency, max_latency
    ↓
Phase 3.1  Compiled regex registry
Phase 3.2  Per-batch match cache
    ↓
Phase 4.1  Template pre-compilation
Phase 4.2  HitTracker sharded map
Phase 4.3  Reduce allocations
Phase 4.4  Debug log short-circuit
    ↓
Phase 5.1  PerfMetrics collector
Phase 5.2  Cgroup CPU sampling
Phase 5.3  Performance API endpoint
Phase 5.4  Configurable window size
    ↓
Phase 6    Remove old CPU throttle
```

Phases 1 and 2 are the core architectural changes. Phases 3-4 are incremental optimizations. Phase 5 adds observability. Phase 6 is cleanup.

---

## Expected Impact

| Metric | Current | After Optimization | Rationale |
|--------|---------|-------------------|-----------|
| CPU (idle, no log activity) | ~0.5% (poll every 2s, all files) | ~0.01% (timer backed off to 10s, no queue drain) | Timer sleeps when nothing to do |
| CPU (moderate: 100 lines/s across 12 jails) | ~2-3% (65536-buf events, 8 workers) | < 0.5% (batch drain every 2s, inline processing) | Batching amortizes overhead |
| CPU (burst: 10,000 lines/s) | ~5-10% (throttle kicks in) | < 1% (timer holds at min_latency, batch processes all) | Pacing prevents spike; match cache avoids redundant regex |
| Memory (steady state) | ~20MB (event channel 65536 × Event struct) | ~5MB (smaller queue, RawLine dedup) | Reduced event duplication |
| Latency (trigger → action) | 50ms-2s (fsnotify) / 2s (poll) | 2s-10s (configurable) | Acceptable per requirements |
| Goroutines | GOMAXPROCS*4 + 3 overhead | 3-4 total | Timer + backend + action executor |

## Testing Strategy

1. **Unit tests:** All new components (batchQueue, adaptInterval, PerfMetrics, cgroupCPU sampler, match cache, regex registry) get isolated unit tests.
2. **Integration tests:** Extend `internal/engine/integration_test.go` to verify end-to-end flow with timer-based execution.
3. **Benchmark tests:** Add `go test -bench` for:
   - `BenchmarkFilterMatch` — regex matching throughput
   - `BenchmarkBatchDrain` — queue drain overhead
   - `BenchmarkHitTracker` — sharded vs single mutex
4. **Existing tests:** All existing tests must continue to pass. The timer architecture is an internal change; the API surface is additive only.

## Risk Assessment

| Risk | Mitigation |
|------|-----------|
| Timer-based batching increases latency beyond acceptable | min_latency default is 2s; configurable down to 100ms if needed |
| Cgroup CPU path doesn't exist on all systems | Graceful fallback to Go runtime metrics |
| Match cache memory growth | Cache is per-batch-drain only; freed after each cycle |
| Removing worker pool bottlenecks action execution | Async action executor with dedicated goroutine |
| Shared regex registry adds complexity | Registry is read-heavy, simple sync.RWMutex; populated at startup |

## Files Changed Summary

| File | Change Type | Phase |
|------|------------|-------|
| `internal/watch/backend.go` | Modified: add `RawLine` type | 1.2 |
| `internal/watch/poll.go` | Modified: glob dedup, `RawLine` output | 1.1, 1.2 |
| `internal/watch/fsnotify.go` | Modified: glob dedup, `RawLine` output, dir watching | 1.1, 1.2, 1.3 |
| `internal/engine/manager.go` | Major refactor: timer loop, batch queue, perf integration | 2.1-2.4, 5.1, 6 |
| `internal/engine/perf.go` | New: PerfMetrics, PerfSnapshot | 5.1 |
| `internal/engine/cpu_cgroup.go` | New: cgroup v2 CPU sampling | 5.2 |
| `internal/engine/cpu.go` | Modified or removed: replaced by cgroup sampler | 6 |
| `internal/engine/hits.go` | Modified: sharded map | 4.2 |
| `internal/engine/jail_runtime.go` | Modified: template caching, debug flag caching | 4.1, 4.4 |
| `internal/filter/registry.go` | New: compiled filter registry | 3.1 |
| `internal/filter/match.go` | Modified: match cache support | 3.2 |
| `internal/action/template.go` | Modified: pre-compilation support | 4.1 |
| `internal/config/types.go` | Modified: add min/max latency, perf_window | 2.5, 5.4 |
| `internal/config/load.go` | Modified: apply defaults for new fields | 2.5, 5.4 |
| `internal/config/validate.go` | Modified: validate new fields | 2.5, 5.4 |
| `internal/control/api.go` | Modified: add PerfResponse | 5.3 |
| `internal/control/server.go` | Modified: add PerfStats to interface, handler | 5.3 |
| `internal/control/client.go` | Modified: add Perf() method | 5.3 |
| `cmd/jailtimed/main.go` | Modified: adapter PerfStats method | 5.3 |
| `cmd/jailtime/main.go` | Modified: add `perf` command | 5.3 |
| `internal/watch/watch_test.go` | Modified: update for RawLine | 1.2 |
| `internal/engine/engine_test.go` | Modified: update for timer architecture | 2 |
| `internal/engine/integration_test.go` | Modified: update for timer architecture | 2 |
