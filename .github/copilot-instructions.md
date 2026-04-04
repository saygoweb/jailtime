# jailtime — Copilot Instructions

## Build & Test

```sh
# Build both binaries
go build ./cmd/jailtimed
go build ./cmd/jailtime

# Run all tests
go test ./...

# Run a single test
go test ./internal/engine/... -run TestJailRuntimeConfigTest
go test ./internal/watch/...  -run TestPollBackendSubdirGlob

# Run tests in a specific package with verbose output
go test -v ./internal/control/...
```

No Makefile — all build/test is via `go` toolchain directly.

## Architecture

The daemon (`jailtimed`) and CLI (`jailtime`) communicate over an **HTTP-over-Unix-socket** API (`/run/jailtime/jailtimed.sock` by default).

```
jailtimed
├── Watch Backend   — fsnotify or poll; re-expands globs each tick → Events
├── Engine/Manager  — routes Events to JailRuntimes; handles jail lifecycle
│   └── JailRuntime — compiled filters + HitTracker + shell action runner
└── Control Server  — HTTP mux over Unix socket
         ▲
jailtime CLI  — control.Client (HTTP over Unix socket)
```

**Event flow:** Watch Backend emits `watch.Event{JailName, FilePath, Line}` → Manager dispatches to the matching `JailRuntime.HandleEvent` → filter match → `HitTracker.Record` sliding window → if threshold hit, run `on_match` shell actions via Go `text/template`.

**Config loading:** `config.Load` parses the main YAML, expands `include:` glob patterns, and merges jails from fragment files. Fragment files may only contain a `jails:` key. Defaults are applied after merging, before validation.

## Adding a New API Endpoint

Follow this chain — every piece is required:

1. **`internal/control/api.go`** — add request/response structs
2. **`internal/engine/jail_runtime.go`** — implement the core logic on `JailRuntime`
3. **`internal/engine/manager.go`** — add a `Manager` method that delegates to `JailRuntime` (acquires `m.mu.RLock`)
4. **`internal/control/server.go`** — add method to `JailController` interface; add HTTP handler; wire into `handleJailAction` (split path with `strings.SplitN(trimmed, "/", 3)`)
5. **`internal/control/client.go`** — add client method using `c.get` or `c.post`; use `url.Values` for query params
6. **`cmd/jailtimed/main.go`** — add method to `JailControllerAdapter` (bridges `engine.Manager` → `control.JailController`)
7. **`cmd/jailtime/main.go`** — add cobra command/subcommand

## Key Conventions

### Filters
- Include-filter patterns **must** contain a named capture group `(?P<ip>...)` — this is what `filter.Extract` pulls out as the IP/CIDR.
- Exclude filters are plain regex (no capture group needed); a match on any exclude filter suppresses the event.
- `filter.Match` returns `nil, nil` (not an error) when a line doesn't match — callers must nil-check the result.

### Config YAML
- Uses pointer booleans (`*bool`) in `rawJailConfig` / `rawEngineConfig` during parsing to distinguish "not set" from `false`. The parsed `JailConfig` / `EngineConfig` use plain values after defaults are applied.
- `enabled` defaults to `true` when omitted; `read_from_end` defaults to `true`.
- Duration fields use a custom `config.Duration` type (wraps `time.Duration`) that unmarshals Go duration strings like `"10m"`, `"1h"`.
- `yaml.KnownFields(true)` is used — unknown YAML keys are a parse error.

### Watch Backends
- Both `PollBackend` and `FsnotifyBackend` call `filepath.Glob` on **every tick/rescan** — new files (including files in new subdirectories matching a `*/access.log` pattern) are discovered automatically without restart.
- `FsnotifyBackend` falls back to `PollBackend` if fsnotify creation fails.
- `WatchSpec.Globs` is a slice — a single jail can watch multiple glob patterns.

### Shell Actions
- Actions are Go `text/template` strings. Available variables: `{{.IP}}`, `{{.Jail}}`, `{{.File}}`, `{{.Line}}`, `{{.JailTime}}` (seconds), `{{.FindTime}}` (seconds), `{{.HitCount}}`, `{{.Timestamp}}` (RFC3339).
- `jails[].query`: a pre-check command; if it exits **0**, the `on_match` action is **skipped** (IP already blocked). Non-zero exit means "not blocked yet — proceed".

### Control Server URL routing
- `/v1/health` — GET
- `/v1/jails` — GET (list all)
- `/v1/jails/{name}/status|start|stop|restart` — GET/POST
- `/v1/jails/{name}/config/files` — GET (`?limit=N&log=true`)
- `/v1/jails/{name}/config/test` — GET (`?file=/path&limit=N&matching=true`)
- Path parsing in `handleJailAction` uses `strings.SplitN(trimmed, "/", 3)` to support two-level sub-actions like `config/files`.

### Testing
- Tests use `t.TempDir()` for all filesystem fixtures — no hardcoded paths.
- Watch backend tests start the backend in a goroutine and use `time.Sleep` + channel drain patterns; allow `150ms` for the backend to initialize before writing files.
- Engine tests (`internal/engine/engine_test.go`) test `JailRuntime` directly without starting the full `Manager`.
- Config fragment tests (`internal/config/config_test.go`) write real YAML files to temp dirs.
