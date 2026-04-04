# jailtime: fail2ban-style service in Go

## 1. Scope and goals

`jailtime` is a fail2ban-like system composed of:

- A long-running daemon: `jailtimed` (managed by `systemd`)
- A control CLI: `jailtime`
- YAML-based configuration in `/etc/jailtime/jail.yaml`

Primary behavior:

- Monitor log files selected by per-jail glob patterns
- Parse new lines with include/exclude regex filters
- Extract an IP/CIDR token from matching lines
- Track hit counts in a moving window (`HitCount` + `FindTime`)
- Trigger configurable actions when threshold is reached
- Keep state externalized (query/action layer), not persisted by `jailtime`

Non-goals (v1):

- Kernel packet filtering implementation details (iptables/nft/pf wrappers can be external actions)
- Stateful database inside daemon
- Distributed clustering

---

## 2. High-level architecture

### 2.1 Components

- `cmd/jailtimed`
  - Starts daemon
  - Loads config
  - Starts watchers + jail runtime manager
  - Exposes control API over local Unix socket
  - Writes logs to journald/stdout or `/var/log/jailtime.log`

- `cmd/jailtime`
  - CLI control app
  - Sends commands to daemon: `start`, `stop`, `restart`, `status`
  - Displays jail status and error output

- `internal/config`
  - YAML schema, defaults, validation
  - Duration parser supporting `s|m|h|d|w`

- `internal/engine`
  - Jail lifecycle manager
  - Event ingestion pipeline
  - Match and threshold logic

- `internal/watch`
  - File change backend abstraction
  - Backends: OS notify (preferred), FS notify, polling fallback

- `internal/filter`
  - Compiled regex matchers
  - Include-first semantics with optional exclude override
  - IP/CIDR extraction helpers

- `internal/action`
  - Command action executor
  - Templating (`{{ .IP }}`, `{{ .Jail }}`, `{{ .Match }}`, etc.)

- `internal/control`
  - Local RPC/HTTP-over-unix API definitions and handlers

- `internal/logging`
  - Structured logger adapter (journal/syslog/file)

### 2.2 Data flow

1. `jailtimed` loads `/etc/jailtime/jail.yaml`.
2. For each enabled jail, daemon expands file globs.
3. Watch backend emits line-oriented events from changed files.
4. Jail pipeline applies `Filters`, then `ExcludeFilters` (if present) as an override.
5. If include-match is not excluded and yields IP/CIDR capture, hit-window state updates.
6. When threshold reached, execute jail `OnMatch` actions with context.
7. Control API exposes runtime status and jail lifecycle operations.

---

## 3. Configuration specification (`/etc/jailtime/jail.yaml`)

## 3.1 Top-level schema

```yaml
version: 1
logging:
  target: journal # journal | file
  file: /var/log/jailtime.log
  level: info # debug | info | warn | error
control:
  socket: /run/jailtime/jailtimed.sock
  timeout: 5s
engine:
  watcher_mode: auto # auto | os | fsnotify | poll
  poll_interval: 2s
  read_from_end: true
jails:
  - name: sshd
    enabled: true
    files:
      - /var/log/auth.log
      - /var/log/secure
    exclude_filters:
      - 'Accepted\s+publickey'
      - 'Accepted\s+password'
    filters:
      - 'Failed password for .* from (?P<ip>[0-9a-fA-F:\.]+)'
      - 'Invalid user .* from (?P<ip>[0-9a-fA-F:\.]+)'
    actions:
      on_match:
        - 'nft add element inet filter blacklist { {{ .IP }} timeout {{ .JailTime }}s }'
      on_start:
        - 'logger -t jailtime "sshd jail started"'
      on_stop:
        - 'logger -t jailtime "sshd jail stopped"'
      on_restart:
        - 'logger -t jailtime "sshd jail warm restart"'
    hit_count: 5
    find_time: 10m
    jail_time: 1h
    net_type: IP # IP | CIDR
    query: 'nft list set inet filter blacklist | grep -q -- "{{ .IP }}"'
```

## 3.2 Jail fields (required semantics)

- `name` (string, required, unique)
- `enabled` (bool, default `true`)
- `files` (array of glob strings, required)
- `filters` (array regex, required)
- `exclude_filters` (array regex, optional; evaluated after `filters` and suppresses matched includes)
- `actions.on_match` (array shell commands, required)
- `actions.on_start` (array shell commands, optional)
- `actions.on_stop` (array shell commands, optional)
- `actions.on_restart` (array shell commands, optional)
- `hit_count` (int >= 1, required)
- `find_time` (duration, required)
- `jail_time` (duration, required)
- `net_type` (`IP` or `CIDR`, required)
- `query` (command template string, required)

Duration format:

- Accept Go-style (`300ms`, `10s`, `5m`, `2h`)
- Also accept `d` and `w` suffixes:
  - `1d = 24h`
  - `1w = 7d`

Validation rules:

- At least one `files` glob and one `filters` regex
- `actions.on_match` cannot be empty
- `find_time > 0`, `jail_time > 0`, `hit_count > 0`
- `net_type` governs extraction validation:
  - `IP`: must parse as `netip.Addr`
  - `CIDR`: must parse as prefix (`netip.ParsePrefix`)

Filter evaluation order:

- First evaluate `filters` (include list); if none match, line is ignored.
- Then evaluate `exclude_filters` only for include-matched lines.
- If any exclude filter matches, drop the line (no hit update, no action).
- If `exclude_filters` is absent or empty, include matches proceed normally.

---

## 4. Matching and threshold algorithm

Pre-step per line before hit accounting:

- Include-first: line must match at least one `filters` regex.
- Optional excludes: if `exclude_filters` exists and any exclude matches, skip line.
- Extraction: derive IP/CIDR from surviving include match context.

Per jail, for each extracted network key `k` (IP or CIDR):

- Keep volatile in-memory counters only:
  - `count`
  - `window_expiry`
- On each matching event at time `t`:
  - If `t > window_expiry`, reset `count = 0`
  - Increment `count += 1`
  - Set `window_expiry = t + FindTime` (reset/extend each hit)
  - If `count >= HitCount`:
    - Optionally run `query` for `k`
    - If query says already known/blocked, skip action
    - Else run `on_match` actions with `JailTime`
    - Reset `count = 0` to avoid immediate retrigger storm

Notes:

- This satisfies: timer reset on each match, reset hits when timer expires.
- External source of truth remains outside daemon (query/action subsystem).

---

## 5. Watch backend requirements

Backend priority (`watcher_mode: auto`):

1. Native OS/file notifications (inotify/kqueue/etc via `fsnotify` or OS abstraction)
2. Generic FS notifications backend
3. Polling fallback (stat + incremental tail read)

### 5.1 Watch abstraction

```go
type Event struct {
    JailName string
    FilePath string
    Offset   int64
    Line     string
    Time     time.Time
}

type Backend interface {
    Name() string
    Start(ctx context.Context, specs []WatchSpec, out chan<- Event) error
}
```

`WatchSpec` includes glob list and per-file cursor behavior.

### 5.2 File handling details

- Track inode + offset per file to handle rotation/truncation.
- On rotate:
  - continue reading old file to EOF if still accessible
  - open new file path and begin at `0` or `end` based on `read_from_end`
- Debounce duplicate notify bursts.
- Poll mode scans globs each interval and tails appended bytes.

---

## 6. Action execution model

Command templates are rendered with context:

- `.IP` extracted IP/CIDR text
- `.Jail` jail name
- `.File` source file
- `.Line` matching log line
- `.JailTime` jail time in seconds
- `.FindTime` find time in seconds
- `.HitCount` configured threshold
- `.Timestamp` RFC3339

Execution requirements:

- Run with `/bin/sh -c` in v1 (later: direct argv mode for safer quoting)
- Per-command timeout (default 10s)
- Capture stdout/stderr and exit code in logs
- Execute action lists sequentially; stop on first failure (configurable later)

Security notes:

- `jail.yaml` must be root-owned and not group/world writable
- Commands run as daemon user (typically root for firewall actions)
- Reject unsafe config permissions at startup with explicit error

---

## 7. Daemon and control plane

## 7.1 `jailtimed` systemd unit

`/etc/systemd/system/jailtimed.service`:

```ini
[Unit]
Description=Jailtime daemon
After=network.target

[Service]
Type=simple
ExecStart=/usr/sbin/jailtimed --config /etc/jailtime/jail.yaml
Restart=on-failure
RestartSec=2s
RuntimeDirectory=jailtime
RuntimeDirectoryMode=0755

# Hardening (tune for firewall backend needs)
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

## 7.2 Control API

Transport:

- Unix domain socket: `/run/jailtime/jailtimed.sock`
- Local-only access

Operations:

- `GET /v1/jails` -> list all jails and statuses
- `POST /v1/jails/{name}/start`
- `POST /v1/jails/{name}/stop`
- `POST /v1/jails/{name}/restart`
- `GET /v1/jails/{name}/status`
- `GET /v1/health`

Status values:

- `started`
- `stopped`

CLI examples:

```bash
jailtime status
jailtime status sshd
jailtime start sshd
jailtime stop sshd
jailtime restart sshd
```

---

## 8. Logging

Default behavior:

- Log to journald via stdout/stderr when run under systemd

Alternative:

- File logger at `/var/log/jailtime.log`

Log events:

- daemon startup/shutdown
- config parse/validation errors
- backend selected (`os`, `fsnotify`, `poll`)
- jail lifecycle transitions
- regex match decisions (debug)
- hit window updates and trigger events
- action command invocation results

---

## 9. Suggested Go module layout

```text
jailtime/
  cmd/
    jailtimed/
      main.go
    jailtime/
      main.go
  internal/
    config/
      types.go
      load.go
      validate.go
      duration.go
    engine/
      manager.go
      jail_runtime.go
      hits.go
    watch/
      backend.go
      fsnotify.go
      poll.go
      tail.go
    filter/
      compile.go
      match.go
      extract.go
    action/
      runner.go
      template.go
    control/
      api.go
      server.go
      client.go
    logging/
      logger.go
  pkg/
    version/
      version.go
```

Dependencies (initial):

- `gopkg.in/yaml.v3` for YAML
- `github.com/fsnotify/fsnotify` for notifications
- `github.com/spf13/cobra` (optional) for CLI
- Standard library `net/netip`, `regexp`, `os/exec`, `time`, `log/slog`

---

## 10. Agentic implementation plan (AI coding workflow)

This section defines implementation steps suitable for autonomous coding agents.

## 10.1 Milestone plan

1. Bootstrap repo and config parsing
- Create module, command entrypoints, base package layout
- Implement YAML load and strict validation
- Add test fixtures for valid/invalid jail configs

2. Duration and schema validation
- Implement custom duration parser with `d`/`w`
- Compile regex at startup; fail-fast on invalid patterns
- Validate action/query fields and net type

3. Watch subsystem
- Implement backend interface and event type
- Implement `fsnotify` backend with rotation-safe tailing
- Implement polling backend fallback
- Add auto-selection logic and backend health logs

4. Filter/match pipeline
- Implement include-first matching with optional exclude override
- Implement named capture extraction (`ip`) and fallback heuristics
- Validate `IP` vs `CIDR` by `net_type`

5. Hit window engine
- Implement per-jail per-key counters + expiry reset semantics
- Add query pre-check hook
- Trigger `on_match` actions on threshold

6. Lifecycle hooks
- Wire `on_start`, `on_stop`, `on_restart` action lists
- Add runtime jail state machine (`started`, `stopped`)

7. Control API and CLI
- Implement UDS server endpoints
- Implement CLI commands mapping to endpoints
- Add human-readable and JSON status output

8. Logging and ops polish
- Support journal/file targets
- Emit structured logs for decisions and failures
- Add systemd unit and packaging docs

9. Hardening and tests
- Add integration tests with temp files + synthetic log appends
- Add race tests for concurrency correctness
- Add permission checks for config file safety

## 10.2 Coding-agent task breakdown

Parallelizable tasks for agents:

- Agent A: `internal/config` + validation + parser tests
- Agent B: `internal/watch` backends + file rotation behavior tests
- Agent C: `internal/filter` and extraction logic tests
- Agent D: `internal/action` runner and template context
- Agent E: control API server/client + CLI wiring

Integration agent:

- Merge all components in `internal/engine`
- Add end-to-end tests from synthetic logs to action invocation

## 10.3 Definition of done (v1)

- `jailtimed` starts from `/etc/jailtime/jail.yaml` and validates config
- At least one notification backend and polling fallback work
- All jail lifecycle actions execute as specified
- Match/exclude/hit-window semantics conform to requirements
- `jailtime` CLI can start/stop/restart/status per jail
- Logging works to journal or file target
- Unit + integration tests pass in CI

---

## 11. Example minimal config

```yaml
version: 1
jails:
  - name: sshd
    files: ["/var/log/auth.log"]
    exclude_filters: []
    filters:
      - 'Failed password for .* from (?P<ip>[0-9\.]+)'
    actions:
      on_match:
        - 'iptables -I INPUT -s {{ .IP }} -j DROP'
      on_start: []
      on_stop: []
      on_restart: []
    hit_count: 5
    find_time: 10m
    jail_time: 1h
    net_type: IP
    query: 'iptables -C INPUT -s {{ .IP }} -j DROP >/dev/null 2>&1'
```

## 12. Future extensions

- Native firewall providers (`nftables`, `iptables`, `pf`) instead of shell commands
- Metrics endpoint (`/metrics`) for Prometheus
- Config hot reload (`SIGHUP`) with per-jail diff application
- Action retry policies and circuit breaker behavior
- Pluggable parser chain for JSON logs and structured events
