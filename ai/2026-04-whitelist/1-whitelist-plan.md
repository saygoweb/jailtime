# Whitelist Feature Plan

## Problem Statement

jailtime currently supports **jails** — watch growing log files line-by-line and
ban IPs that exceed a hit threshold. This plan adds four related capabilities:

1. **Static-file watching** — some files (e.g. a Cloudflare CIDR list updated by
   a cron job or API call) are replaced wholesale. The engine must diff the old and
   new contents, running actions for added, removed, and (optionally) unchanged
   entries.

2. **Whitelist.d** — a parallel concept to jails.d. Whitelists are functionally
   identical to jails (same config shape, same action hooks) but the typical
   action is an iptables ACCEPT rule rather than DROP. Critically, **whitelist
   on_start / on_add actions must run before jail on_start / on_add** so that
   ACCEPT rules are inserted above DROP rules in iptables. Whitelists, like
   jails, have a `type` field which can be `cidr` or `ip`.

3. **First-class whitelist-ignore in jails** — a new `whitelist_sets` field on a
   jail lets operators name one or more whitelist sets whose membership suppresses
   `on_add` for that IP, without hand-writing a `query` template. Membership is
   checked in-memory in Go (supporting ip-vs-ip-list and ip-vs-CIDR), not via
   external ipset calls.

4. **File-glob exclusions** — a new `exclude_files` field on jails/whitelists so
   that specific files matched by a broad glob can be silently skipped (e.g.
   apache access logs that are behind Cloudflare and carry proxy IPs only).

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

- Add `ExcludeFiles []string \`yaml:"exclude_files"\`` to `rawJailConfig` and
  `JailConfig`.
- Add `ExcludeGlobs []string` to `watch.WatchSpec`.
- In `buildSpecs` (manager) copy the new field through.
- In `PollBackend.Start` and `FsnotifyBackend.Start`, after expanding `Globs`,
  expand each `ExcludeGlobs` pattern and subtract matches from the result set
  before opening / tailing files.
- Validation: warn (not error) if an exclude_files entry matches no file at
  startup.

### Phase 2 — Static-file watching

**The core problem:**  
A static file (e.g. `/etc/jailtime/whitelists/cloudflare.txt`) is atomically
replaced. We need to diff the previous set of matching lines against the new set
and fire different actions for added, removed, and persisting entries.

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
      on_add:     ['ipset add jt_whitelist {{ .IP }}']   # IP newly present (replaces on_match)
      on_remove:  ['ipset del jt_whitelist {{ .IP }}']   # NEW — IP gone
      on_stop:    [...]
```

- `on_add` = IP **appeared** (new since last scan, or first scan). `on_match` remains as a deprecated alias for `on_add`.
- `on_remove` = IP **disappeared** (was in previous scan, not in current).

#### 2b — Watch layer changes

```go
// watch/backend.go
type EventKind string

const (
    EventLine    EventKind = "line"     // existing tail event
    EventAdded   EventKind = "added"    // static: IP newly present
    EventRemoved EventKind = "removed"  // static: IP no longer present
)

type Event struct {
    Kind     EventKind   // NEW (defaults to EventLine for backward compat)
    JailName string
    FilePath string
    Offset   int64
    Line     string
    Time     time.Time
}

type WatchSpec struct {
    JailName     string
    Globs        []string
    ExcludeGlobs []string   // Phase 1
    ReadFromEnd  bool
    WatchMode    string     // "tail" | "static"
}
```

`PollBackend` gains a separate map of **static file snapshots**:
```
staticSnapshots map[tailerKey]map[string]bool  // path → set of matching lines
```

On each tick for a static-mode spec:
1. Re-read the entire file (no offset tracking).
2. Run each line through the jail's include/exclude filter chain **in the watch
   layer** — only store lines that match (or store raw lines and let the engine
   filter; see trade-off note below).
3. Compute `added = current − previous`, `removed = previous − current`.
4. Emit `EventAdded` / `EventRemoved` events accordingly.

> **Trade-off:** Filtering in the watch layer couples the backend to the filter
> package. The simpler approach is to emit raw lines and let `JailRuntime`
> handle filtering, just as it does today for tail mode. Static snapshots should
> therefore track **raw lines** (the full line, not the extracted IP) so the
> engine can re-run filters consistently.

`FsnotifyBackend` mirrors the same static snapshot logic, triggered on
`fsnotify.Write` or `fsnotify.Create` events for watched static files.

#### 2c — Engine changes

`JailRuntime.HandleEvent` dispatches on `evt.Kind`:

- `EventLine` (tail) — existing logic unchanged.
- `EventAdded` — run include/exclude filters, extract IP, skip HitTracker
  entirely, run `on_add` actions directly (treat as immediate threshold hit
  with count=1). `on_match` is a deprecated alias for `on_add`.
- `EventRemoved` — extract IP from line (same filter), run `on_remove` actions.
  No HitTracker involvement.

The `query` pre-check still applies to `EventAdded` (suppresses `on_add` if
the IP is already in the target ipset). It does **not** apply to `EventRemoved`.

`HitTracker` is not involved in static mode. `hit_count`, `find_time`, and
`jail_time` have no meaning for static entries (the file itself determines
membership duration). Validation should warn if those fields are set alongside
`watch_mode: static`.

### Phase 3 — Whitelist.d support

#### 3a — Config shape

Add `Whitelists []JailConfig` to `Config`. Fragment files in `whitelist.d/` use
a `whitelists:` key (not `jails:`):

```yaml
# /etc/jailtime/whitelist.d/cloudflare.yaml
whitelists:
  - name: cloudflare
    watch_mode: static
    ...
```

Main config gains a parallel include mechanism:

```yaml
# /etc/jailtime/jail.yaml
include:
  - jails.d/*.yaml
  - whitelist.d/*.yaml    # fragment files may contain jails: or whitelists:
```

OR add a dedicated `include_whitelists` key. The simpler approach is to allow
fragment files to contain **either** `jails:` or `whitelists:` (or both), and
extend `loadJailsFile` → `loadFragmentFile` to parse both keys. This avoids a
second include directive.

`rawJailsFile` becomes `rawFragmentFile`:
```go
type rawFragmentFile struct {
    Jails      []rawJailConfig `yaml:"jails"`
    Whitelists []rawJailConfig `yaml:"whitelists"`
}
```

#### 3b — Startup ordering (iptables rule order)

The `Manager` must start whitelist runtimes and wait for their `on_start` actions
to complete **before** starting any jail runtimes. This ensures whitelist ACCEPT
rules exist in iptables before any DROP rules are inserted.

```go
// manager.go — Run()
// 1. Start whitelists (on_start + set up watchers)
// 2. Start jails (on_start + set up watchers)
```

Both share the same watch backend and event channel. The backend receives specs
from both pools. The event router resolves `evt.JailName` in a combined map
`allRuntimes = whitelists ∪ jails`.

The `Manager` struct gains:
```go
type Manager struct {
    cfg        *config.Config
    jails      map[string]*JailRuntime
    whitelists map[string]*JailRuntime   // NEW
    backend    watch.Backend
    mu         sync.RWMutex
}
```

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

CLI: `jailtime whitelist [list|status|start|stop|restart]` mirroring `jailtime jail`.

Implementing the full chain per the architecture guide:
`api.go` → `jail_runtime.go` (no change; shared) → `manager.go` → `server.go` →
`client.go` → `jailtimed/main.go` (adapter) → `jailtime/main.go` (cobra).

### Phase 4 — First-class whitelist-ignore in jails

Add a convenience field `whitelist_sets` to `JailConfig`:

```yaml
jails:
  - name: nginx
    whitelist_sets:
      - cloudflare        # NEW — name of a whitelist defined in whitelists:/whitelist.d/
      - trusted_nets
```

When `whitelist_sets` is non-empty, the engine performs an **in-memory Go
membership check** against the named whitelists before running `on_add`. This
check handles both `type: ip` whitelists (exact IP match against a set of IPs)
and `type: cidr` whitelists (IP contained within any CIDR range). The check is
not delegated to ipset or any external tool — it is a pure Go comparison.

If the candidate IP is a member of any named whitelist set, `on_add` is
suppressed. This replaces the need to hand-write a `query` template.

Validation: error if both `query` and `whitelist_sets` are set simultaneously
(ambiguous semantics) — require the operator to merge them manually or leave
`query` empty.

---

## File change inventory

| File | Change |
|------|--------|
| `internal/config/types.go` | Add `ExcludeFiles`, `WatchMode`, `WhitelistSets`, `Type` to `JailConfig`; add `OnAdd`, `OnRemove` to `JailActions` (keep `OnMatch` as deprecated alias for `OnAdd`); add `Whitelists []JailConfig` to `Config` |
| `internal/config/load.go` | Extend `rawFragmentFile` to parse `whitelists:`; load whitelist fragments; build `Config.Whitelists` |
| `internal/config/validate.go` | Validate `watch_mode` values; warn on `hit_count`/`find_time` with static mode; validate `whitelist_sets`/`query` conflict; error on whitelist/jail name collision |
| `internal/watch/backend.go` | Add `EventKind`, `Kind` to `Event`; add `ExcludeGlobs`, `WatchMode` to `WatchSpec` |
| `internal/watch/poll.go` | Implement static snapshot diffing; apply `ExcludeGlobs` |
| `internal/watch/fsnotify.go` | Implement static snapshot diffing; apply `ExcludeGlobs` |
| `internal/engine/jail_runtime.go` | Dispatch on `evt.Kind`; handle `EventAdded`/`EventRemoved`; implement `whitelist_sets` in-memory membership check (ip and CIDR) |
| `internal/engine/manager.go` | Add `whitelists map`; start whitelists before jails; expose whitelist management methods; update `buildSpecs` |
| `internal/control/api.go` | Add whitelist request/response structs |
| `internal/control/server.go` | Add whitelist routes and `JailController` interface methods |
| `internal/control/client.go` | Add whitelist client methods |
| `cmd/jailtimed/main.go` | Extend `JailControllerAdapter` with whitelist methods |
| `cmd/jailtime/main.go` | Add `whitelist` cobra subcommand tree |
| `samples/whitelist.d/` | Add sample whitelist configs (cloudflare CIDR, trusted-nets) |

---

## Implementation Order

The phases are designed to be independently mergeable:

1. **Phase 1** (exclude_files) — pure config + watch layer, no engine changes.
2. **Phase 2** (static files) — watch layer + engine; no whitelist concept yet.
   Can be validated with a regular jail using `watch_mode: static`.
3. **Phase 3** (whitelist.d) — config + manager + API + CLI; builds on Phase 2
   for the common case of static whitelists, but works with `watch_mode: tail`
   too.
4. **Phase 4** (whitelist_sets) — small engine change; can be deferred if the
   `query` field is sufficient for early adopters.

---

## Open Questions

- **Fsnotify static files:** Atomically replaced files (write to temp + rename)
  trigger `fsnotify.Rename` + `fsnotify.Create`. The fsnotify backend must
  re-watch the new inode after a rename. This is already a known complication
  for log rotation; the same fix applies here.

- **Whitelist/jail name collision:** Error at config-load time. *(Confirmed)*

- **API versioning:** The new `/v1/whitelists` endpoints fit naturally into v1.
  No version bump needed.
