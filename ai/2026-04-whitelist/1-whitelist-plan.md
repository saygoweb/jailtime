# Whitelist Feature Plan

## Problem Statement

jailtime currently supports **jails** — watch growing log files line-by-line and
ban IPs that exceed a hit threshold. This plan adds four related capabilities:

1. **Static-file watching** — some files (e.g. a Cloudflare CIDR list updated by
   a cron job or API call) are replaced wholesale. The engine must diff the old and
   new contents, running actions for added and removed entries.

2. **whitelist.d** — a parallel concept to jails.d. Whitelists are functionally
   identical to jails (same config shape, same action hooks) but the typical
   action is an iptables ACCEPT rule rather than DROP. Critically, **whitelist
   on_start / on_add actions must run before jail on_start / on_add** so that
   ACCEPT rules are inserted above DROP rules in iptables. Whitelists, like
   jails, have a `net_type` field which can be `IP` or `CIDR`.

3. **First-class whitelist-ignore in jails** — a new `ignore_sets` field on a
   jail names one or more loaded whitelists whose in-memory IP/CIDR set suppresses
   `on_add` for a matched IP, without hand-writing a `query` template. Membership
   is checked in-memory in Go (ip-vs-ip-list and ip-vs-CIDR), not via
   external ipset calls.

4. **File-glob exclusions** — a new `exclude_files` field on jails/whitelists so
   that specific files matched by a broad glob can be silently skipped (e.g.
   apache access logs behind Cloudflare that carry only proxy IPs).

---

## Design

### Phase 1 — File-glob exclusions (lowest risk, standalone)

**Config change:**
```yaml
jails:
  - name: apache
    files:
      - /var/log/apache2/*/access.log
    exclude_files:                        # NEW
      - /var/log/apache2/cloudflare*/access.log
```

**Code changes:**

- Add `ExcludeFiles []string \`yaml:"exclude_files"\`` to both `rawJailConfig`
  and `JailConfig` in `internal/config/types.go` and `load.go`.
- Add `ExcludeGlobs []string` to `watch.WatchSpec` in `internal/watch/backend.go`.
- In `buildSpecs` (manager) copy `cfg.ExcludeFiles` → `spec.ExcludeGlobs`.
- In `PollBackend.Start`: after building `pathInfos`, expand each `ExcludeGlobs`
  pattern and delete matching keys from `pathInfos` before opening tailers.
- In `FsnotifyBackend.Start`: same exclusion in `initialScan` and `handleCreate`.
- Validation: warn (not error) if an `exclude_files` entry matches no file at startup.

### Phase 2 — Static-file watching

**The core problem:**  
A static file (e.g. `/etc/jailtime/whitelists/cloudflare.txt`) is atomically
replaced. We need to diff the previous set of lines against the new set and fire
actions for added and removed entries.

#### 2a — Config shape

A new field `watch_mode` on `JailConfig` (default `"tail"`):

```yaml
whitelists:
  - name: cloudflare
    watch_mode: static          # NEW — "tail" (default) | "static"
    files:
      - /etc/jailtime/whitelists/cloudflare.txt
    net_type: CIDR
    filters:
      - '^(?P<ip>[0-9./]+)\s*$'
    actions:
      on_start:   [...]
      on_add:     ['ipset add jt_whitelist {{ .IP }}']   # IP newly present
      on_remove:  ['ipset del jt_whitelist {{ .IP }}']   # IP gone
      on_stop:    [...]
```

- `on_add` = IP **appeared** (new since last scan, or first scan). `on_match` is a
  deprecated alias: if `on_add` is empty and `on_match` is set, use `on_match`.
- `on_remove` = IP **disappeared**.

**Config struct changes** in `internal/config/types.go` / `load.go`:

```go
// JailConfig gains:
WatchMode    string   `yaml:"watch_mode"`    // "tail" | "static"; default "tail"
ExcludeFiles []string `yaml:"exclude_files"` // Phase 1

// JailActions gains:
OnAdd    []string `yaml:"on_add"`    // replaces on_match for static mode
OnRemove []string `yaml:"on_remove"` // static: IP removed from file
// OnMatch []string kept as deprecated alias
```

`rawJailConfig` mirrors these additions. In `Load`, after building `JailConfig`,
merge: if `OnAdd` is empty and `OnMatch` is non-empty, log a deprecation warning
and copy `OnMatch` → `OnAdd`.

`applyDefaults` sets `WatchMode = "tail"` when unset, for all jails **and**
whitelists.

**Validation changes** in `internal/config/validate.go`:

- For `watch_mode: tail` jails: existing rules unchanged (`on_match`/`on_add`
  required, `find_time`/`jail_time`/`hit_count` required).
- For `watch_mode: static` jails: `on_add` required; `find_time`, `jail_time`,
  and `hit_count` must **not** be set (error if they are).
- Validate `watch_mode` is one of `"tail"`, `"static"`.

#### 2b — Watch layer changes

The watch layer communicates with the engine via two types:

```go
// RawLine is the unit emitted by backends via DrainFunc — gains Kind field:
type RawLine struct {
    Kind      EventKind  // NEW — "line" (default), "added", "removed"
    FilePath  string
    Line      string
    Jails     []string
    EnqueueAt time.Time
}

// EventKind distinguishes tail vs static diff events:
type EventKind string
const (
    EventLine    EventKind = "line"    // existing tail event (zero value for compat)
    EventAdded   EventKind = "added"   // static: line newly present
    EventRemoved EventKind = "removed" // static: line no longer present
)

// Event (constructed in manager.processBatch from RawLine) also gains Kind:
type Event struct {
    Kind     EventKind
    JailName string
    FilePath string
    Line     string
    Time     time.Time
    // Offset field retained but unused
}

// WatchSpec gains two new fields:
type WatchSpec struct {
    JailName     string
    Globs        []string
    ExcludeGlobs []string // Phase 1
    ReadFromEnd  bool
    WatchMode    string   // "tail" | "static"
}
```

**`PollBackend`** gains a static snapshot map:

```go
staticSnapshots map[string]map[string]bool  // file path → set of raw lines
```

(keyed by file path string, same as the existing `tailers` map)

On each tick for specs where `WatchMode == "static"`:
1. Open and read the entire file (no `FileTailer` / offset tracking).
2. Collect all lines into a `map[string]bool`.
3. Compare against `staticSnapshots[path]`:
   - `added = current − previous` → emit `RawLine{Kind: EventAdded}`
   - `removed = previous − current` → emit `RawLine{Kind: EventRemoved}`
4. Replace `staticSnapshots[path]` with current set.
5. On first scan (no previous snapshot): treat all lines as added.

Snapshots track **raw lines** (not extracted IPs) so the engine applies its
normal `filter.Match` pipeline consistently, just as for tail events.

**`FsnotifyBackend`** mirrors the same static snapshot logic: on
`fsnotify.Write` or `fsnotify.Create` for a static-mode file, re-read and diff
the snapshot. `fsnotify.Rename` (atomic replace) must also trigger a re-read
and re-watch of the new inode (see Open Questions).

**`manager.processBatch`** must propagate `RawLine.Kind` to `watch.Event.Kind`:

```go
evt := watch.Event{
    Kind:     line.Kind,    // propagate
    JailName: jailName,
    FilePath: line.FilePath,
    Line:     line.Line,
    Time:     line.EnqueueAt,
}
```

#### 2c — Engine changes

**`JailRuntime` gains new compiled template fields:**

```go
type JailRuntime struct {
    // existing:
    onMatchTmpls []*template.Template  // renamed/repurposed as onAddTmpls
    queryTmpl    *template.Template

    // new:
    onAddTmpls    []*template.Template  // compiled from OnAdd (or OnMatch fallback)
    onRemoveTmpls []*template.Template  // compiled from OnRemove
}
```

`compileTemplates` and `Reconfigure` compile both `OnAdd` and `OnRemove`
template slices. The `onMatchTmpls` field is replaced by `onAddTmpls` internally.

**`HandleEvent` dispatches on `evt.Kind`:**

- `EventLine` (tail, default) — existing logic entirely unchanged.
- `EventAdded` — run `filter.Match`, validate IP (same `net.ParseIP`/`net.ParseCIDR`
  switch as today), skip `HitTracker`, run `onAddTmpls` directly.
  The `query_before_match` gate still applies: if `cfg.QueryBeforeMatch && queryTmpl != nil`,
  run the query; exit 0 suppresses `on_add`. No `ActionRunner.Submit` dedup is
  needed since the watch layer guarantees each added line fires only once per scan.
- `EventRemoved` — run `filter.Match`, validate IP, run `onRemoveTmpls`.
  No query check, no HitTracker.

### Phase 3 — Whitelist.d support

#### 3a — Config shape

Add `Whitelists []JailConfig` to `Config`. Fragment files may contain either
key (or both):

```yaml
# /etc/jailtime/whitelist.d/cloudflare.yaml
whitelists:
  - name: cloudflare
    watch_mode: static
    ...
```

The existing `include:` glob list already handles fragment-file discovery.
Fragment files need not be segregated by directory — any included file may
contain both `jails:` and `whitelists:` keys.

**`rawJailsFile` → `rawFragmentFile`** in `internal/config/load.go`:

```go
type rawFragmentFile struct {
    Jails      []rawJailConfig `yaml:"jails"`
    Whitelists []rawJailConfig `yaml:"whitelists"`
}
```

`loadJailsFile` is renamed `loadFragmentFile`, returns
`(jails []rawJailConfig, whitelists []rawJailConfig, error)`.

**`rawConfig`** gains a `Whitelists` field:
```go
type rawConfig struct {
    ...
    Jails      []rawJailConfig `yaml:"jails"`
    Whitelists []rawJailConfig `yaml:"whitelists"` // NEW
}
```

The main `Load` function builds `config.Config.Whitelists` from `raw.Whitelists`
using the same `rawJailConfig → JailConfig` conversion as for jails.

**`applyDefaults`** must loop over `c.Whitelists` in addition to `c.Jails`.

**`Validate`** must:
- Check `c.Whitelists` with the same per-entry rules as `c.Jails`.
- Error on name collisions between jails and whitelists.

#### 3b — Startup ordering (iptables rule order)

`Manager.Run` starts whitelist runtimes **before** jail runtimes. Both complete
`on_start` before any jail action is triggered.

```go
// manager.go — Run()
// 1. Start all enabled whitelists (on_start + watchers)
// 2. Start all enabled jails (on_start + watchers)
```

**`Manager` struct** gains:

```go
type Manager struct {
    cfg             *config.Config
    configPath      string
    jails           map[string]*JailRuntime
    whitelists      map[string]*JailRuntime  // NEW
    backend         watch.Backend
    mu              sync.RWMutex
    perf            *PerfMetrics
    currentInterval time.Duration
    lastDrainAt     time.Time
}
```

`NewManager` initialises `m.whitelists` from `cfg.Whitelists`, same pattern as
`m.jails`.

**`buildSpecs`** is updated to include specs from `m.whitelists` as well as
`m.jails` (all share the same backend).

**`processBatch`** looks up runtimes in a combined map:

```go
// snapshot from both maps under RLock
allRuntimes := make(map[string]*JailRuntime, len(m.jails)+len(m.whitelists))
for name, jr := range m.jails      { allRuntimes[name] = jr }
for name, jr := range m.whitelists { allRuntimes[name] = jr }
```

**`RestartJail`** already reloads and reconciles jails. A parallel
`RestartWhitelist` method handles the whitelist map. Both must also call
`m.backend.UpdateSpecs` with the merged spec list.

#### 3c — Control API & CLI

Mirror the existing jail endpoints for whitelists:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/whitelists` | list all whitelist statuses |
| GET | `/v1/whitelists/{name}/status` | status of one whitelist |
| POST | `/v1/whitelists/{name}/start` | start |
| POST | `/v1/whitelists/{name}/stop` | stop |
| POST | `/v1/whitelists/{name}/restart` | restart |
| GET | `/v1/whitelists/{name}/config/files` | matched files |
| GET | `/v1/whitelists/{name}/config/test` | test filters |

**`control/server.go`**: register `/v1/whitelists` and `/v1/whitelists/` handlers,
mirroring `handleJails` / `handleJailAction`. Path parsing via
`strings.SplitN(trimmed, "/", 3)` (same pattern as existing).

**`JailController` interface** gains whitelist equivalents of every jail method:
`StartWhitelist`, `StopWhitelist`, `RestartWhitelist`, `WhitelistStatus`,
`AllWhitelistStatuses`, `WhitelistConfigFiles`, `WhitelistConfigTest`.

**`control/api.go`** adds `ListWhitelistsResponse`, reusing `JailStatusResponse`.

**`control/client.go`** adds client methods for each new endpoint using the
existing `c.get` / `c.post` helpers.

**`cmd/jailtimed/main.go`** — `JailControllerAdapter` gains whitelist methods
bridging to `Manager`.

**`cmd/jailtime/main.go`** — add `whitelist` cobra subcommand tree mirroring
the existing `status`/`start`/`stop`/`restart`/`config` commands, with a
`whitelist` parent command.

### Phase 4 — First-class whitelist-ignore in jails

Add `ignore_sets` to `JailConfig`:

```yaml
jails:
  - name: nginx
    ignore_sets:
      - cloudflare      # name of a loaded whitelist runtime
      - trusted_nets
```

**In-memory membership on `JailRuntime`:**

Each static whitelist runtime maintains two in-memory sets, updated on every
`HandleEvent` call:

```go
type JailRuntime struct {
    ...
    memberIPs   map[string]struct{}  // for net_type: IP  (keyed by normalised IP string)
    memberCIDRs []*net.IPNet         // for net_type: CIDR
}

func (jr *JailRuntime) IsMember(ip string) bool
```

`EventAdded` adds to the set; `EventRemoved` removes from it. The sets are
guarded by `jr.mu`.

**Injection at construction time:**

`JailRuntime` holds a membership-checker function injected by the manager after
all runtimes are created:

```go
type MembershipChecker func(ip string) bool

type JailRuntime struct {
    ...
    ignoreSetsChecker MembershipChecker  // nil if ignore_sets not configured
}
```

In `Manager.NewManager` (and `RestartJail` after reconciliation), after
constructing all runtimes, inject:

```go
func (m *Manager) injectIgnoreSets() {
    for _, jr := range m.jails {
        if len(jr.cfg.IgnoreSets) > 0 {
            jr.ignoreSetsChecker = m.buildIgnoreChecker(jr.cfg.IgnoreSets)
        }
    }
}

func (m *Manager) buildIgnoreChecker(sets []string) MembershipChecker {
    return func(ip string) bool {
        m.mu.RLock()
        defer m.mu.RUnlock()
        for _, name := range sets {
            if wl, ok := m.whitelists[name]; ok && wl.IsMember(ip) {
                return true
            }
        }
        return false
    }
}
```

In `HandleEvent`, after a threshold hit for `EventLine` (or for `EventAdded`),
call `ignoreSetsChecker` before executing `onAddTmpls`. If it returns `true`,
suppress `on_add` and log.

**Validation**: error if both `query` and `ignore_sets` are set simultaneously
(require the operator to choose one approach).

---

## File change inventory

| File | Change |
|------|--------|
| `internal/config/types.go` | Add `WatchMode`, `ExcludeFiles`, `IgnoreSets []string` to `JailConfig`; add `OnAdd`, `OnRemove []string` to `JailActions` (keep `OnMatch` as deprecated alias); add `Whitelists []JailConfig` to `Config` |
| `internal/config/load.go` | Rename `rawJailsFile` → `rawFragmentFile` (adds `Whitelists` key); rename `loadJailsFile` → `loadFragmentFile`; add `WatchMode`, `ExcludeFiles`, `IgnoreSets` to `rawJailConfig`; build `Config.Whitelists` from `raw.Whitelists`; merge `OnMatch` → `OnAdd` with deprecation warning |
| `internal/config/validate.go` | Branch on `watch_mode`: relax `find_time`/`jail_time`/`hit_count` for static; require `on_add`; validate `watch_mode` values; error on `ignore_sets`+`query` conflict; error on whitelist/jail name collision; validate `c.Whitelists` entries |
| `internal/watch/backend.go` | Add `EventKind` type + `EventLine`/`EventAdded`/`EventRemoved` consts; add `Kind` to `RawLine`; add `Kind` to `Event`; add `ExcludeGlobs`, `WatchMode` to `WatchSpec` |
| `internal/watch/poll.go` | Add `staticSnapshots map[string]map[string]bool`; on each tick: for static specs, read whole file, diff against snapshot, emit `RawLine{Kind: EventAdded/EventRemoved}`; apply `ExcludeGlobs` before opening tailers |
| `internal/watch/fsnotify.go` | Same static snapshot diffing triggered on `Write`/`Create`/`Rename` events; apply `ExcludeGlobs` in `initialScan` and `handleCreate` |
| `internal/engine/jail_runtime.go` | Add `onAddTmpls`, `onRemoveTmpls []*template.Template`, `memberIPs map[string]struct{}`, `memberCIDRs []*net.IPNet`, `ignoreSetsChecker MembershipChecker`; update `compileTemplates`/`Reconfigure` for new templates; dispatch in `HandleEvent` on `evt.Kind`; add `IsMember(ip string) bool` method |
| `internal/engine/manager.go` | Add `whitelists map[string]*JailRuntime`; start whitelists before jails in `Run`; add `injectIgnoreSets`/`buildIgnoreChecker`; update `buildSpecs`, `processBatch` to include whitelists; add `StartWhitelist`/`StopWhitelist`/`RestartWhitelist`/`WhitelistStatus`/`AllWhitelistStatuses`/`WhitelistConfigFiles`/`WhitelistConfigTest` methods |
| `internal/control/api.go` | Add `ListWhitelistsResponse` (reuses `JailStatusResponse`) |
| `internal/control/server.go` | Add whitelist methods to `JailController` interface; register `/v1/whitelists` and `/v1/whitelists/` handlers; add `handleWhitelists` and `handleWhitelistAction` (mirror of `handleJails`/`handleJailAction`) |
| `internal/control/client.go` | Add `ListWhitelists`, `WhitelistStatus`, `StartWhitelist`, `StopWhitelist`, `RestartWhitelist`, `WhitelistConfigFiles`, `WhitelistConfigTest` client methods |
| `cmd/jailtimed/main.go` | Extend `JailControllerAdapter` with whitelist methods delegating to `Manager` |
| `cmd/jailtime/main.go` | Add `whitelist` cobra command with `status`/`start`/`stop`/`restart`/`config files`/`config test` subcommands mirroring the jail commands |
| `samples/whitelist.d/` | Add sample configs: cloudflare CIDR static whitelist, trusted-nets IP static whitelist |

---

## Implementation Order

The phases are independently mergeable:

1. **Phase 1** (exclude_files) — config + watch layer only, no engine changes.
2. **Phase 2** (static files) — watch layer (`RawLine.Kind`, static snapshots) +
   engine (`HandleEvent` dispatch, `onAddTmpls`/`onRemoveTmpls`). Can be validated
   with a regular jail using `watch_mode: static` before any whitelist concept exists.
3. **Phase 3** (whitelist.d) — config schema + manager + full API + CLI chain;
   builds on Phase 2 for static whitelists but also works with `watch_mode: tail`.
4. **Phase 4** (ignore_sets) — adds `IsMember`, `MembershipChecker` injection, and
   the `ignore_sets` config field; can be deferred if operators use `query` directly.

---

## Open Questions

- **Fsnotify static files:** Atomically replaced files (write to temp + rename)
  trigger `fsnotify.Rename` + `fsnotify.Create`. The fsnotify backend must
  re-watch the new inode after a rename. This is already a known complication
  for log rotation; the same fix applies here.

- **Whitelist/jail name collision:** Error at config-load time. *(Confirmed)*

- **API versioning:** The new `/v1/whitelists` endpoints fit naturally into v1.
  No version bump needed.
