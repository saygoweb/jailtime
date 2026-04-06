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
| **CPU control** | Implicit (Python GIL + sleep between polls) | Reactive CPU throttle | Reactive & adaptive: smooth latency-driven timer within configurable min/max bounds |
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

The timer is **reactive and adaptive** — it continuously adjusts its interval based on measured latency and queue pressure. The transitions are smooth (exponential moving average) rather than jumpy:

```
                    ┌──────────────────────────────────────────────────┐
                    │        Smooth Reactive Timer Adapter             │
                    │                                                  │
                    │  Inputs:                                         │
                    │    measuredLatency = drainStart - oldest.enqueue │
                    │    queueDepth      = len(batch)                  │
                    │    execTime        = duration of batch drain     │
                    │                                                  │
                    │  Smooth transition (EMA, α = 0.3):               │
                    │    target = computeTarget(inputs)                │
                    │    interval = interval*(1-α) + target*α          │
                    │    interval = clamp(interval, min, max)          │
                    │                                                  │
                    │  computeTarget logic:                            │
                    │    if queueDepth == 0:                           │
                    │      target = interval * 1.5   (back off)        │
                    │    elif measuredLatency > max * 0.8:             │
                    │      target = interval * 0.5   (speed up)        │
                    │    elif measuredLatency < min * 0.5:             │
                    │      target = interval * 1.25  (relax)           │
                    │    else:                                         │
                    │      target = interval         (hold steady)     │
                    │                                                  │
                    │  Always clamped to [min_latency, max_latency]    │
                    └──────────────────────────────────────────────────┘
```

**Key design principle:** The timer is **reactive** — it measures actual latency (enqueue time vs drain time) and adapts. When latency is well below the minimum, the interval increases (saves CPU). When the queue is growing and latency rising toward max, the interval decreases (faster processing). The exponential moving average (α = 0.3) ensures smooth transitions — no oscillation or "jumpiness" between states.

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
            drainStart := time.Now()

            // Measure latency from oldest enqueued item
            var measuredLatency time.Duration
            if len(batch) > 0 {
                measuredLatency = drainStart.Sub(batch[0].enqueueAt)
            }

            m.processBatch(ctx, batch)
            execTime := time.Since(drainStart)
            
            m.perf.RecordExecution(execTime, measuredLatency, len(batch), m.currentInterval)
            nextInterval := m.adaptInterval(execTime, len(batch), measuredLatency)
            timer.Reset(nextInterval)
        }
    }
}
```

**Files changed:** `internal/engine/manager.go` (major refactor of `Run`)

#### 2.2 — Smooth Reactive Adaptive Interval

The adaptive interval uses an exponential moving average (EMA) for smooth transitions, driven by measured latency and queue depth:

```go
const emaAlpha = 0.3 // Smoothing factor: 0.3 = moderate responsiveness

func (m *Manager) adaptInterval(execTime time.Duration, batchSize int, measuredLatency time.Duration) time.Duration {
    var target time.Duration

    switch {
    case batchSize == 0:
        // No work in queue — relax toward max_latency (save CPU)
        target = time.Duration(float64(m.currentInterval) * 1.5)
    case measuredLatency > time.Duration(float64(m.maxLatency) * 0.8):
        // Latency approaching max — speed up (shrink interval)
        target = time.Duration(float64(m.currentInterval) * 0.5)
    case measuredLatency < time.Duration(float64(m.minLatency) * 0.5):
        // Latency well below min — relax slightly (save CPU)
        target = time.Duration(float64(m.currentInterval) * 1.25)
    default:
        // Latency in acceptable range — hold steady
        target = m.currentInterval
    }

    // Additionally, if execution time is consuming too much of the interval,
    // stretch the interval to allow breathing room.
    if batchSize > 0 && execTime > m.currentInterval/2 {
        stretched := time.Duration(float64(execTime) * 2.0)
        if stretched > target {
            target = stretched
        }
    }

    // Smooth transition using EMA
    next := time.Duration(
        float64(m.currentInterval)*(1-emaAlpha) + float64(target)*emaAlpha,
    )

    // Clamp to [min, max]
    if next < m.minLatency {
        next = m.minLatency
    }
    if next > m.maxLatency {
        next = m.maxLatency
    }

    m.currentInterval = next
    return next
}
```

**Behavior examples:**
- Idle system: interval smoothly grows from 2s → 3s → 4.5s → ... → 10s (max)
- Burst arrives: interval drops smoothly from 10s → 7s → 5s → 3.5s → 2.5s → 2s (min)
- Steady load: interval holds near min_latency
- Load subsides: interval smoothly relaxes back toward max_latency

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

#### 4.2 — HitTracker Sharded Map

**Current:** Single `sync.Mutex` protects entire `map[string]*HitWindow`.

**Solution:** Sharded map with 16 shards — partition by IP hash, each shard has its own mutex. Reduces contention by factor of 16. Simple, effective, no API change:

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

---

## Detailed AI Implementation Plan

This section provides step-by-step implementation instructions organized into independent work units suitable for AI sub-agents. Each unit describes the exact files to modify, the changes to make, how to verify, and dependencies on other units.

### Prerequisites

- Working directory: `/home/cambell/src/sgw/jailtime`
- Branch: `feat/optimize`
- Go module: `github.com/sgw/jailtime`
- Build: `go build ./cmd/jailtimed && go build ./cmd/jailtime`
- Test: `go test ./...`
- All existing tests must pass after each unit.

---

### Unit 1: Config — Add min_latency, max_latency, perf_window

**Goal:** Add three new engine configuration fields with defaults and validation.

**Files to modify:**

1. **`internal/config/types.go`** — Add fields to `EngineConfig`:
   ```go
   EngineConfig struct {
       WatcherMode  string   `yaml:"watcher_mode"`
       PollInterval Duration `yaml:"poll_interval"`
       ReadFromEnd  bool     `yaml:"read_from_end"`
       MinLatency   Duration `yaml:"min_latency"`   // NEW
       MaxLatency   Duration `yaml:"max_latency"`   // NEW
       PerfWindow   int      `yaml:"perf_window"`   // NEW
   }
   ```
   Add constants:
   ```go
   const (
       defaultSocketPath   = "/run/jailtime/jailtimed.sock"
       defaultPollInterval = 2 * time.Second
       defaultMinLatency   = 2 * time.Second    // NEW
       defaultMaxLatency   = 10 * time.Second   // NEW
       defaultPerfWindow   = 3                   // NEW
   )
   ```

2. **`internal/config/load.go`** — Add to `rawEngineConfig`:
   ```go
   rawEngineConfig struct {
       WatcherMode  string   `yaml:"watcher_mode"`
       PollInterval Duration `yaml:"poll_interval"`
       ReadFromEnd  *bool    `yaml:"read_from_end"`
       MinLatency   Duration `yaml:"min_latency"`   // NEW
       MaxLatency   Duration `yaml:"max_latency"`   // NEW
       PerfWindow   *int     `yaml:"perf_window"`   // NEW (pointer for default detection)
   }
   ```
   In `applyDefaults()`, add:
   ```go
   if c.Engine.MinLatency.Duration == 0 {
       c.Engine.MinLatency.Duration = defaultMinLatency
   }
   if c.Engine.MaxLatency.Duration == 0 {
       c.Engine.MaxLatency.Duration = defaultMaxLatency
   }
   if c.Engine.PerfWindow == 0 {
       c.Engine.PerfWindow = defaultPerfWindow
   }
   ```

3. **`internal/config/validate.go`** — Add validation in `Validate()`:
   ```go
   if c.Engine.MinLatency.Duration <= 0 {
       return fmt.Errorf("engine: min_latency must be > 0")
   }
   if c.Engine.MaxLatency.Duration <= 0 {
       return fmt.Errorf("engine: max_latency must be > 0")
   }
   if c.Engine.MaxLatency.Duration < c.Engine.MinLatency.Duration {
       return fmt.Errorf("engine: max_latency must be >= min_latency")
   }
   if c.Engine.PerfWindow < 1 {
       return fmt.Errorf("engine: perf_window must be >= 1")
   }
   ```

4. **`internal/config/config_test.go`** — Add test(s) verifying defaults are applied and validation catches invalid values (max < min, perf_window < 1).

**Verification:** `go test ./internal/config/... && go build ./cmd/jailtimed && go build ./cmd/jailtime`

**Dependencies:** None — this is a leaf change.

---

### Unit 2: HitTracker — Sharded Map

**Goal:** Replace single-mutex HitTracker with 16-shard design.

**Files to modify:**

1. **`internal/engine/hits.go`** — Replace entire implementation:
   - Current `HitTracker` has `mu sync.Mutex` + `windows map[string]*HitWindow`
   - New design:
     ```go
     const numShards = 16

     type HitTracker struct {
         shards [numShards]hitShard
     }

     type hitShard struct {
         mu      sync.Mutex
         windows map[string]*HitWindow
     }

     type HitWindow struct {
         Count        int
         WindowExpiry time.Time
     }

     func NewHitTracker() *HitTracker {
         var ht HitTracker
         for i := range ht.shards {
             ht.shards[i].windows = make(map[string]*HitWindow)
         }
         return &ht
     }

     func (ht *HitTracker) shard(key string) *hitShard {
         h := uint32(2166136261) // FNV-1a offset basis
         for i := 0; i < len(key); i++ {
             h ^= uint32(key[i])
             h *= 16777619
         }
         return &ht.shards[h%numShards]
     }

     func (ht *HitTracker) Record(ip string, t time.Time, findTime time.Duration, threshold int) (count int, triggered bool) {
         s := ht.shard(ip)
         s.mu.Lock()
         defer s.mu.Unlock()

         w, ok := s.windows[ip]
         if !ok {
             w = &HitWindow{}
             s.windows[ip] = w
         }
         if t.After(w.WindowExpiry) {
             w.Count = 0
         }
         w.Count++
         w.WindowExpiry = t.Add(findTime)

         if w.Count >= threshold {
             count = w.Count
             w.Count = 0
             return count, true
         }
         return w.Count, false
     }
     ```
   - Keep the `NewHitTracker()` function signature compatible with callers.

2. **`internal/engine/engine_test.go`** — Existing HitTracker tests should still pass. Add a concurrent benchmark:
   ```go
   func BenchmarkHitTrackerConcurrent(b *testing.B) {
       ht := NewHitTracker()
       b.RunParallel(func(pb *testing.PB) {
           i := 0
           for pb.Next() {
               ip := fmt.Sprintf("10.0.0.%d", i%256)
               ht.Record(ip, time.Now(), time.Minute, 5)
               i++
           }
       })
   }
   ```

**Verification:** `go test ./internal/engine/... -run TestHit && go test -bench BenchmarkHitTracker ./internal/engine/...`

**Dependencies:** None — the `Record()` signature is unchanged.

---

### Unit 3: Cgroup CPU Sampling

**Goal:** Create a cgroup v2 CPU sampler that reads from `/sys/fs/cgroup/system.slice/jailtimed.service/cpu.stat`, with graceful fallback to Go runtime metrics.

**Files to create/modify:**

1. **`internal/engine/cpu_cgroup.go`** — New file:
   ```go
   package engine

   import (
       "bufio"
       "fmt"
       "os"
       "runtime/metrics"
       "strconv"
       "strings"
       "time"
   )

   // cgroupCPUSampler reads CPU usage from cgroup v2 cpu.stat.
   // Falls back to Go runtime metrics if cgroup is unavailable.
   type cgroupCPUSampler struct {
       cgroupPath string
       lastUsage  int64     // microseconds
       lastTime   time.Time
       useCgroup  bool

       // Fallback: Go runtime sampler
       fallback *cpuSampler
   }

   func newCgroupCPUSampler(serviceName string) *cgroupCPUSampler {
       path := fmt.Sprintf("/sys/fs/cgroup/system.slice/%s/cpu.stat", serviceName)
       s := &cgroupCPUSampler{cgroupPath: path}

       // Test if cgroup path is readable
       if usage, err := s.readUsageUsec(); err == nil {
           s.useCgroup = true
           s.lastUsage = usage
           s.lastTime = time.Now()
       } else {
           s.useCgroup = false
           s.fallback = newCPUSampler()
       }
       return s
   }

   func (s *cgroupCPUSampler) readUsageUsec() (int64, error) {
       f, err := os.Open(s.cgroupPath)
       if err != nil {
           return 0, err
       }
       defer f.Close()

       scanner := bufio.NewScanner(f)
       for scanner.Scan() {
           line := scanner.Text()
           if strings.HasPrefix(line, "usage_usec ") {
               val, err := strconv.ParseInt(strings.TrimPrefix(line, "usage_usec "), 10, 64)
               return val, err
           }
       }
       return 0, fmt.Errorf("usage_usec not found in %s", s.cgroupPath)
   }

   // Sample returns CPU usage as a percentage (0-100+) since last call.
   // For cgroup: percentage of total CPU time consumed.
   // For fallback: fraction of GOMAXPROCS cores.
   func (s *cgroupCPUSampler) Sample() float64 {
       if !s.useCgroup {
           return s.fallback.sample() * 100.0 // Convert fraction to percentage
       }

       usage, err := s.readUsageUsec()
       if err != nil {
           return 0
       }
       now := time.Now()
       elapsed := now.Sub(s.lastTime).Microseconds()
       if elapsed <= 0 {
           return 0
       }

       delta := usage - s.lastUsage
       pct := float64(delta) / float64(elapsed) * 100.0

       s.lastUsage = usage
       s.lastTime = now
       return pct
   }
   ```
   Note: The percentage is of one CPU core. On a 2-core machine, 100% means one full core.

2. **`internal/engine/cpu_cgroup_test.go`** — New file with tests:
   - Test `readUsageUsec` with a temp file containing mock cpu.stat content
   - Test `Sample()` returns 0 when cgroup unavailable (fallback works)
   - Test that `newCgroupCPUSampler` with a non-existent service falls back gracefully

**Verification:** `go test ./internal/engine/... -run TestCgroup`

**Dependencies:** None — uses existing `cpuSampler` as fallback.

---

### Unit 4: PerfMetrics Collector

**Goal:** Create the performance metrics collector that tracks latency, execution time, delay, and CPU usage in circular buffers.

**Files to create:**

1. **`internal/engine/perf.go`** — New file:
   ```go
   package engine

   import (
       "sync"
       "time"
   )

   // PerfSnapshot is a point-in-time view of performance metrics.
   type PerfSnapshot struct {
       CurrentLatencyMs float64 `json:"current_latency_ms"`
       CurrentDelayMs   float64 `json:"current_delay_ms"`
       AvgExecTimeMs    float64 `json:"avg_exec_time_ms"`
       AvgCPUPercent    float64 `json:"avg_cpu_percent"`
       WindowSize       int     `json:"window_size"`
   }

   // PerfMetrics collects performance metrics in circular buffers.
   type PerfMetrics struct {
       mu sync.RWMutex

       windowSize int

       latencies    []time.Duration // circular buffer
       latencyIdx   int
       latencyCount int

       execTimes    []time.Duration // circular buffer
       execIdx      int
       execCount    int

       cpuSamples  []float64 // circular buffer
       cpuIdx      int
       cpuCount    int

       currentDelay   time.Duration
       currentLatency time.Duration

       cpuSampler *cgroupCPUSampler
   }

   func NewPerfMetrics(windowSize int, serviceName string) *PerfMetrics {
       return &PerfMetrics{
           windowSize: windowSize,
           latencies:  make([]time.Duration, windowSize),
           execTimes:  make([]time.Duration, windowSize),
           cpuSamples: make([]float64, windowSize),
           cpuSampler: newCgroupCPUSampler(serviceName),
       }
   }

   // RecordExecution is called after each batch drain.
   func (p *PerfMetrics) RecordExecution(execTime, measuredLatency time.Duration, batchSize int, currentDelay time.Duration) {
       cpuPct := p.cpuSampler.Sample()

       p.mu.Lock()
       defer p.mu.Unlock()

       p.currentDelay = currentDelay
       if batchSize > 0 {
           p.currentLatency = measuredLatency
       }

       // Push exec time
       p.execTimes[p.execIdx%p.windowSize] = execTime
       p.execIdx++
       if p.execCount < p.windowSize {
           p.execCount++
       }

       // Push latency (only when batch had items)
       if batchSize > 0 {
           p.latencies[p.latencyIdx%p.windowSize] = measuredLatency
           p.latencyIdx++
           if p.latencyCount < p.windowSize {
               p.latencyCount++
           }
       }

       // Push CPU
       p.cpuSamples[p.cpuIdx%p.windowSize] = cpuPct
       p.cpuIdx++
       if p.cpuCount < p.windowSize {
           p.cpuCount++
       }
   }

   // Snapshot returns a point-in-time view of performance metrics.
   func (p *PerfMetrics) Snapshot() PerfSnapshot {
       p.mu.RLock()
       defer p.mu.RUnlock()

       return PerfSnapshot{
           CurrentLatencyMs: float64(p.currentLatency.Microseconds()) / 1000.0,
           CurrentDelayMs:   float64(p.currentDelay.Microseconds()) / 1000.0,
           AvgExecTimeMs:    avgDurationMs(p.execTimes, p.execCount),
           AvgCPUPercent:    avgFloat(p.cpuSamples, p.cpuCount),
           WindowSize:       p.windowSize,
       }
   }

   func avgDurationMs(buf []time.Duration, count int) float64 {
       if count == 0 {
           return 0
       }
       var sum time.Duration
       for i := 0; i < count; i++ {
           sum += buf[i]
       }
       return float64(sum.Microseconds()) / 1000.0 / float64(count)
   }

   func avgFloat(buf []float64, count int) float64 {
       if count == 0 {
           return 0
       }
       var sum float64
       for i := 0; i < count; i++ {
           sum += buf[i]
       }
       return sum / float64(count)
   }
   ```

2. **`internal/engine/perf_test.go`** — New file:
   - Test circular buffer wrapping (push N+1 items into window of N)
   - Test `Snapshot()` returns correct averages
   - Test `RecordExecution` with batchSize == 0 doesn't push latency
   - Test `NewPerfMetrics` with unavailable cgroup path still works

**Verification:** `go test ./internal/engine/... -run TestPerf`

**Dependencies:** Unit 3 (cgroup sampler).

---

### Unit 5: Watch Backend — Glob Dedup & RawLine

**Goal:** Deduplicate glob expansion across jails, and change the backend output channel from `Event` per jail to `RawLine` per file-line (carrying all interested jail names).

**Files to modify:**

1. **`internal/watch/backend.go`**:
   - Keep existing `Event` type (still used internally by engine after fan-out)
   - Add new `RawLine` type:
     ```go
     type RawLine struct {
         FilePath  string
         Line      string
         Jails     []string
         EnqueueAt time.Time
     }
     ```
   - Change `Backend` interface:
     ```go
     type Backend interface {
         Name() string
         Start(ctx context.Context, specs []WatchSpec, out chan<- RawLine) error
         UpdateSpecs(specs []WatchSpec)
     }
     ```

2. **`internal/watch/poll.go`** — Modify the poll loop:
   - In the glob expansion phase, deduplicate: collect unique glob patterns first, expand each once, then map results to jails
   - Change event emission: instead of sending one `Event` per jail, send one `RawLine` with `Jails: []string{all watching jails}`
   - Update `Start()` signature to use `chan<- RawLine`

3. **`internal/watch/fsnotify.go`** — Same dedup and RawLine changes:
   - Deduplicate globs in `rescan()`
   - Change `readAndSend()` to emit `RawLine` instead of per-jail `Event`
   - Update `Start()` signature

4. **`internal/watch/watch_test.go`** — Update tests to receive `RawLine` instead of `Event`. Verify that:
   - A line from a file watched by 2 jails produces 1 `RawLine` with both jail names
   - Glob dedup: same pattern in 2 specs only expands once

**Verification:** `go test ./internal/watch/...`

**Dependencies:** None for the watch package itself. Unit 6 depends on this.

---

### Unit 6: Timer-Based Engine — Manager Refactor

**Goal:** Replace the worker-pool event loop in `Manager.Run` with a timer-based batch queue and smooth reactive adaptive interval.

This is the largest and most critical unit. It refactors `internal/engine/manager.go`.

**Files to modify:**

1. **`internal/engine/manager.go`** — Major refactor:

   **Remove:**
   - `eventTask` struct
   - Constants: `targetCPUFraction`, `cpuCheckInterval`, `maxDispatchDelay`, `eventQueueSize`, `taskQueueSize`
   - Worker pool goroutines in `Run()`
   - `checkCPU()` closure
   - Import of `runtime` (unless needed elsewhere)

   **Add to Manager struct:**
   ```go
   type Manager struct {
       cfg            *config.Config
       configPath     string
       jails          map[string]*JailRuntime
       backend        watch.Backend
       mu             sync.RWMutex
       perf           *PerfMetrics        // NEW
       queue          batchQueue           // NEW
       minLatency     time.Duration        // NEW
       maxLatency     time.Duration        // NEW
       currentInterval time.Duration       // NEW
   }
   ```

   **Add batch queue:**
   ```go
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
       q.items = append(q.items, pendingItem{line: line, enqueueAt: time.Now()})
       q.mu.Unlock()
   }

   func (q *batchQueue) Drain() []pendingItem {
       q.mu.Lock()
       batch := q.items
       q.items = make([]pendingItem, 0, cap(batch)) // fresh slice, old backing array released with batch
       q.mu.Unlock()
       return batch
   }
   ```

   **Update `NewManager()`:**
   ```go
   func NewManager(cfg *config.Config, configPath string) (*Manager, error) {
       // ... existing jail creation ...
       // ... existing backend creation ...

       minLatency := cfg.Engine.MinLatency.Duration
       maxLatency := cfg.Engine.MaxLatency.Duration

       return &Manager{
           cfg:             cfg,
           configPath:      configPath,
           jails:           jails,
           backend:         backend,
           perf:            NewPerfMetrics(cfg.Engine.PerfWindow, "jailtimed.service"),
           minLatency:      minLatency,
           maxLatency:      maxLatency,
           currentInterval: minLatency,
       }, nil
   }
   ```

   **Replace `Run()` with three goroutines:**
   ```go
   func (m *Manager) Run(ctx context.Context) error {
       // 1. Start all enabled jails (same as before)
       // 2. Build watch specs (same as before)

       // 3. Start the enqueue loop: reads RawLines from backend, enqueues
       rawLines := make(chan watch.RawLine, 4096)
       backendErr := make(chan error, 1)
       go func() {
           if err := m.backend.Start(ctx, specs, rawLines); err != nil && err != context.Canceled {
               backendErr <- err
           }
           close(backendErr)
       }()

       // 4. Enqueue goroutine: reads from rawLines channel, pushes to batch queue
       go func() {
           for {
               select {
               case <-ctx.Done():
                   return
               case line, ok := <-rawLines:
                   if !ok {
                       return
                   }
                   m.queue.Enqueue(line)
               }
           }
       }()

       // 5. Drain loop: timer-based batch processing
       timer := time.NewTimer(m.minLatency)
       defer timer.Stop()

       for {
           select {
           case err := <-backendErr:
               if err != nil {
                   return fmt.Errorf("watch backend: %w", err)
               }
               return nil

           case <-ctx.Done():
               // Stop all jails
               stopCtx := context.Background()
               m.mu.RLock()
               for _, jr := range m.jails {
                   if jr.Status() == StatusStarted {
                       _ = jr.Stop(stopCtx)
                   }
               }
               m.mu.RUnlock()
               return ctx.Err()

           case <-timer.C:
               batch := m.queue.Drain()
               drainStart := time.Now()

               var measuredLatency time.Duration
               if len(batch) > 0 {
                   measuredLatency = drainStart.Sub(batch[0].enqueueAt)
               }

               m.processBatch(ctx, batch)
               execTime := time.Since(drainStart)

               m.perf.RecordExecution(execTime, measuredLatency, len(batch), m.currentInterval)
               nextInterval := m.adaptInterval(execTime, len(batch), measuredLatency)
               timer.Reset(nextInterval)
           }
       }
   }
   ```

   **Add `processBatch()`:**
   ```go
   func (m *Manager) processBatch(ctx context.Context, batch []pendingItem) {
       m.mu.RLock()
       defer m.mu.RUnlock()

       for _, item := range batch {
           for _, jailName := range item.line.Jails {
               jr, exists := m.jails[jailName]
               if !exists || jr.Status() != StatusStarted {
                   continue
               }
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

   **Add `adaptInterval()` — smooth reactive EMA:**
   ```go
   const emaAlpha = 0.3

   func (m *Manager) adaptInterval(execTime time.Duration, batchSize int, measuredLatency time.Duration) time.Duration {
       var target time.Duration

       switch {
       case batchSize == 0:
           target = time.Duration(float64(m.currentInterval) * 1.5)
       case measuredLatency > time.Duration(float64(m.maxLatency)*0.8):
           target = time.Duration(float64(m.currentInterval) * 0.5)
       case measuredLatency < time.Duration(float64(m.minLatency)*0.5):
           target = time.Duration(float64(m.currentInterval) * 1.25)
       default:
           target = m.currentInterval
       }

       if batchSize > 0 && execTime > m.currentInterval/2 {
           stretched := time.Duration(float64(execTime) * 2.0)
           if stretched > target {
               target = stretched
           }
       }

       next := time.Duration(
           float64(m.currentInterval)*(1-emaAlpha) + float64(target)*emaAlpha,
       )
       if next < m.minLatency {
           next = m.minLatency
       }
       if next > m.maxLatency {
           next = m.maxLatency
       }
       m.currentInterval = next
       return next
   }
   ```

   **Add `PerfStats()` method:**
   ```go
   func (m *Manager) PerfStats() PerfSnapshot {
       return m.perf.Snapshot()
   }
   ```

2. **`internal/engine/engine_test.go`** — Update tests:
   - Tests that create `JailRuntime` directly and call `HandleEvent` should still work unchanged
   - Tests that exercise `Manager.Run` need to account for timer-based execution (events are processed on timer ticks, not immediately)
   - Add test for `adaptInterval()`:
     - Empty batch → interval grows toward max
     - Non-empty batch with high latency → interval shrinks toward min
     - Smooth transitions (no jumps > emaAlpha * delta)

3. **`internal/engine/integration_test.go`** — Update:
   - Events now arrive via `RawLine` and are processed on timer ticks
   - Tests may need slightly longer timeouts (min_latency = 2s default, but tests can set smaller values like 100ms)
   - Manager initialization needs the new config fields set

**Verification:** `go test ./internal/engine/... && go build ./cmd/jailtimed`

**Dependencies:** Units 1 (config), 4 (PerfMetrics), 5 (watch RawLine).

---

### Unit 7: Performance API Endpoint

**Goal:** Add `/v1/perf` endpoint and `jailtime perf` CLI command.

**Files to modify:**

1. **`internal/control/api.go`** — Add response type:
   ```go
   type PerfResponse struct {
       CurrentLatencyMs float64 `json:"current_latency_ms"`
       CurrentDelayMs   float64 `json:"current_delay_ms"`
       AvgExecTimeMs    float64 `json:"avg_exec_time_ms"`
       AvgCPUPercent    float64 `json:"avg_cpu_percent"`
       WindowSize       int     `json:"window_size"`
   }
   ```

2. **`internal/control/server.go`** — Add to `JailController` interface:
   ```go
   type JailController interface {
       StartJail(ctx context.Context, name string) error
       StopJail(ctx context.Context, name string) error
       RestartJail(ctx context.Context, name string) error
       JailStatus(name string) (string, error)
       AllJailStatuses() map[string]string
       ConfigFiles(name string, limit int, logFiles bool) ([]string, error)
       ConfigTest(name, filePath string, limit int, returnMatching bool) (totalLines, matchingLines int, matches []string, err error)
       PerfStats() PerfResponse  // NEW
   }
   ```
   Add handler method to `Server`:
   ```go
   func (s *Server) handlePerf(w http.ResponseWriter, r *http.Request) {
       if r.Method != http.MethodGet {
           http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
           return
       }
       stats := s.controller.PerfStats()
       writeJSON(w, http.StatusOK, stats)
   }
   ```
   Register in `Serve()`:
   ```go
   mux.HandleFunc("/v1/perf", s.handlePerf)
   ```

3. **`internal/control/client.go`** — Add client method:
   ```go
   // Perf calls GET /v1/perf.
   func (c *Client) Perf() (*PerfResponse, error) {
       var resp PerfResponse
       if err := c.get("/v1/perf", &resp); err != nil {
           return nil, err
       }
       return &resp, nil
   }
   ```

4. **`cmd/jailtimed/main.go`** — Add to `JailControllerAdapter`:
   ```go
   func (a *JailControllerAdapter) PerfStats() control.PerfResponse {
       snap := a.m.PerfStats()
       return control.PerfResponse{
           CurrentLatencyMs: snap.CurrentLatencyMs,
           CurrentDelayMs:   snap.CurrentDelayMs,
           AvgExecTimeMs:    snap.AvgExecTimeMs,
           AvgCPUPercent:    snap.AvgCPUPercent,
           WindowSize:       snap.WindowSize,
       }
   }
   ```

5. **`cmd/jailtime/main.go`** — Add `perf` cobra command:
   ```go
   perfCmd := &cobra.Command{
       Use:   "perf",
       Short: "Show daemon performance metrics",
       Long:  "Display current latency, execution delay, average execution time, and CPU usage.",
       Args:  cobra.NoArgs,
       RunE: func(cmd *cobra.Command, args []string) error {
           c := client()
           resp, err := c.Perf()
           if err != nil {
               return err
           }
           fmt.Printf("Performance Metrics (window=%d):\n", resp.WindowSize)
           fmt.Printf("  Current latency:     %.0fms\n", resp.CurrentLatencyMs)
           fmt.Printf("  Current delay:       %.0fms\n", resp.CurrentDelayMs)
           fmt.Printf("  Avg execution time:  %.1fms\n", resp.AvgExecTimeMs)
           fmt.Printf("  Avg CPU usage:       %.1f%%\n", resp.AvgCPUPercent)
           return nil
       },
   }
   root.AddCommand(perfCmd)
   ```

**Verification:** `go build ./cmd/jailtimed && go build ./cmd/jailtime && go test ./internal/control/...`

**Dependencies:** Units 4 (PerfMetrics/PerfSnapshot types), 6 (Manager.PerfStats method).

---

### Unit 8: Template Pre-Compilation

**Goal:** Pre-compile action templates at JailRuntime init time instead of parsing on every execution.

**Files to modify:**

1. **`internal/action/template.go`** — Add pre-compilation:
   ```go
   // CompileTemplate parses a template string once for reuse.
   func CompileTemplate(name, tmpl string) (*template.Template, error) {
       return template.New(name).Parse(tmpl)
   }

   // RenderCompiled executes a pre-compiled template with the given context.
   func RenderCompiled(t *template.Template, ctx Context) (string, error) {
       var buf bytes.Buffer
       if err := t.Execute(&buf, ctx); err != nil {
           return "", err
       }
       return buf.String(), nil
   }
   ```
   Keep existing `Render()` for backward compatibility.

   Add `RunCompiled` and `RunAllCompiled` variants that accept pre-compiled templates:
   ```go
   func RunCompiled(ctx context.Context, tmpl *template.Template, actCtx Context, timeout time.Duration) (Result, error) {
       // Same as Run but uses RenderCompiled instead of Render
   }

   func RunAllCompiled(ctx context.Context, templates []*template.Template, actCtx Context, timeout time.Duration) ([]Result, error) {
       // Same as RunAll but uses RunCompiled
   }
   ```

2. **`internal/engine/jail_runtime.go`** — Add pre-compiled templates to JailRuntime:
   ```go
   type JailRuntime struct {
       cfg              *config.JailConfig
       includes         []*filter.CompiledFilter
       excludes         []*filter.CompiledFilter
       hits             *HitTracker
       mu               sync.RWMutex
       status           JailStatus
       debugLog         *debugRateLimiter
       inflight         sync.Map
       onMatchTemplates []*template.Template  // NEW: pre-compiled
       queryTemplate    *template.Template     // NEW: pre-compiled (nil if no query)
   }
   ```
   Compile templates in `NewJailRuntime()` and `Reconfigure()`. Use `action.RunAllCompiled()` in `HandleEvent()`.

3. **`internal/action/action_test.go`** — Add tests for `CompileTemplate`, `RenderCompiled`, `RunCompiled`.

**Verification:** `go test ./internal/action/... && go test ./internal/engine/...`

**Dependencies:** None — additive change.

---

### Unit 9: Cleanup — Remove Old CPU Throttle

**Goal:** Remove the old Go-runtime-based CPU throttle from the manager since the timer-based architecture replaces it.

**Files to modify:**

1. **`internal/engine/cpu.go`** — Keep this file (it's still used as fallback by cgroupCPUSampler). No changes needed since `cpuSampler` is referenced by the fallback path in `cpu_cgroup.go`.

2. **`internal/engine/manager.go`** — Should already be clean from Unit 6 (worker pool and checkCPU removed). Verify no remaining references to:
   - `targetCPUFraction`
   - `cpuCheckInterval`
   - `maxDispatchDelay`
   - `eventQueueSize` (should now be the rawLines channel size, e.g. 4096)
   - `taskQueueSize`
   - `eventTask` struct
   - `cpuSampler` (moved to fallback in cpu_cgroup.go)

**Verification:** `go build ./cmd/jailtimed && go test ./internal/engine/...`

**Dependencies:** Unit 6 (manager refactor).

---

### Implementation Order & Dependency Graph

```
Unit 1: Config fields           ─────────────────────────────────┐
Unit 2: HitTracker shards       ─── (independent) ───────────────┤
Unit 3: Cgroup CPU sampler      ─────────────┐                   │
Unit 8: Template pre-compilation ─── (indep) ─┤                   │
                                              │                   │
Unit 4: PerfMetrics collector  ←─── Unit 3 ───┘                   │
Unit 5: Watch backend RawLine  ─── (independent) ─┐               │
                                                   │               │
Unit 6: Manager refactor       ←── Units 1,4,5 ───┼───────────────┘
                                                   │
Unit 7: Performance API        ←── Units 4,6 ──────┘
Unit 9: Cleanup                ←── Unit 6
```

**Parallelizable groups:**
- **Group A (independent):** Units 1, 2, 3, 5, 8 — can all be implemented in parallel
- **Group B (depends on A):** Unit 4 (needs 3), Unit 6 (needs 1, 4, 5)
- **Group C (depends on B):** Unit 7 (needs 4, 6), Unit 9 (needs 6)

**Recommended serial execution order:**
1. Units 1, 2, 3, 5, 8 (parallel)
2. Unit 4
3. Unit 6
4. Units 7, 9 (parallel)

---

### Sub-Agent Assignment Guide

Each unit above is sized for a single AI sub-agent. The agent should:

1. Read the plan section for its unit
2. Read the specific source files listed (use Serena symbolic tools for targeted reading)
3. Make the changes described
4. Run the verification command
5. Fix any compilation or test failures
6. Commit with a descriptive message

**Critical rules for sub-agents:**
- Do NOT modify files outside the listed scope for your unit
- Do NOT change function signatures that other units depend on without coordinating
- All existing tests must pass after your changes
- Use `go vet ./...` before committing
- Follow existing code style (see code conventions in project memories)
- Use the existing test patterns: `t.TempDir()` for fixtures, table-driven tests where appropriate
