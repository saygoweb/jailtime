# jailtime — Project Overview

## Purpose
`jailtime` is a fail2ban-like daemon written in Go that watches log files for suspicious patterns and runs shell actions (e.g., blocking IPs via firewall rules) when a threshold of matching events is reached within a sliding time window.

## Binaries
- `jailtimed` — the daemon that watches log files and runs actions
- `jailtime` — the CLI client that communicates with the daemon over HTTP-over-Unix-socket

## Tech Stack
- **Language:** Go (1.26+)
- **Key dependencies:**
  - `github.com/fsnotify/fsnotify` — inotify-based file watching
  - `github.com/spf13/cobra` — CLI framework
  - `gopkg.in/yaml.v3` — YAML config parsing
- **Module:** `github.com/sgw/jailtime`

## Architecture
```
jailtimed
├── Watch Backend   — fsnotify or poll; re-expands globs each tick → Events
├── Engine/Manager  — routes Events to JailRuntimes; handles jail lifecycle
│   └── JailRuntime — compiled filters + HitTracker + shell action runner
└── Control Server  — HTTP mux over Unix socket (/run/jailtime/jailtimed.sock)
         ▲
jailtime CLI  — control.Client (HTTP over Unix socket)
```

**Event flow:** Watch Backend emits `watch.Event{JailName, FilePath, Line}` → Manager dispatches to matching `JailRuntime.HandleEvent` → filter match → `HitTracker.Record` sliding window → if threshold hit, run `on_match` shell actions via Go `text/template`.

## Directory Structure
```
cmd/
  jailtime/      — CLI client binary
  jailtimed/     — daemon binary
internal/
  config/        — YAML config loading, types, validation
  control/       — HTTP API (api.go, server.go, client.go)
  engine/        — JailRuntime, Manager, HitTracker
  action/        — shell action runner and template rendering
  watch/         — file watching backends (poll, fsnotify)
  filter/        — regex filter compilation and matching
  logging/       — logger
pkg/             — (shared packages if any)
samples/         — example config files
deploy/          — deployment files
docs/            — documentation
```
