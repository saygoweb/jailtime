# Code Conventions & Key Patterns

## Config YAML
- Pointer booleans (`*bool`) in `rawJailConfig`/`rawEngineConfig` to distinguish "not set" from `false`
- Parsed `JailConfig`/`EngineConfig` use plain values after defaults are applied
- `enabled` defaults to `true`; `read_from_end` defaults to `true`
- Duration fields use custom `config.Duration` type (wraps `time.Duration`) for Go duration strings
- `yaml.KnownFields(true)` — unknown YAML keys are a parse error

## Filters
- Include-filter patterns **must** contain named capture group `(?P<ip>...)` for `filter.Extract`
- Exclude filters are plain regex (no capture group needed)
- `filter.Match` returns `nil, nil` on no match — callers must nil-check the result

## Watch Backends
- Both `PollBackend` and `FsnotifyBackend` call `filepath.Glob` on every tick/rescan
- `FsnotifyBackend` falls back to `PollBackend` if fsnotify creation fails
- `WatchSpec.Globs` is a slice — a single jail can watch multiple glob patterns

## Shell Actions (text/template)
Available variables: `{{.IP}}`, `{{.Jail}}`, `{{.File}}`, `{{.Line}}`, `{{.JailTime}}` (seconds), `{{.FindTime}}` (seconds), `{{.HitCount}}`, `{{.Timestamp}}` (RFC3339)
- `query` field: pre-check command; exit 0 → skip `on_match` (already blocked); non-zero → proceed

## Control Server URL Routing
- `/v1/health` — GET
- `/v1/jails` — GET
- `/v1/jails/{name}/status|start|stop|restart` — GET/POST
- `/v1/jails/{name}/config/files` — GET (`?limit=N&log=true`)
- `/v1/jails/{name}/config/test` — GET (`?file=/path&limit=N&matching=true`)
- Path parsing uses `strings.SplitN(trimmed, "/", 3)` for two-level sub-actions like `config/files`

## Adding a New API Endpoint (required chain)
1. `internal/control/api.go` — add request/response structs
2. `internal/engine/jail_runtime.go` — implement core logic on `JailRuntime`
3. `internal/engine/manager.go` — add `Manager` method delegating to `JailRuntime` (use `m.mu.RLock`)
4. `internal/control/server.go` — add method to `JailController` interface; add HTTP handler; wire into `handleJailAction`
5. `internal/control/client.go` — add client method using `c.get` or `c.post`; use `url.Values` for query params
6. `cmd/jailtimed/main.go` — add method to `JailControllerAdapter`
7. `cmd/jailtime/main.go` — add cobra command/subcommand

## Testing
- Use `t.TempDir()` for all filesystem fixtures — no hardcoded paths
- Watch backend tests: goroutine + `time.Sleep` + channel drain; allow 150ms for init before writing files
- Engine tests: test `JailRuntime` directly without starting full `Manager`
- Config fragment tests: write real YAML files to temp dirs
