# jailtime

```
 | | |         _  _    _
 | | |        (_)| |  (_)
 | | |  __ _   _ | |_  _  _ __ ___    ___
 | | | / _` | | || __|| || '_ ` _ \  / _ \
 | | || (_| | | || |_ | || | | | | ||  __/
 |_|_| \__,_| |_| \__||_||_| |_| |_| \___|
 | | |
```

A fail2ban-style intrusion-prevention daemon written in Go. `jailtimed` watches
log files for patterns, tracks hit counts within a sliding time window, and
executes shell actions (e.g. firewall rules) when a threshold is exceeded.
It uses an event-driven architecture with a lazy drain timer — truly idle when
no log activity occurs.

## Documentation

- **[jailtime CLI](docs/jailtime.md)** — command reference, flags, examples
- **[jailtimed configuration](docs/jailtimed.md)** — daemon config, jail fields, action templates, sample jails

## Feature Highlights

- Highly Performant (close to zero, even on a loaded server)
- Maintain Jails and Whitelists
- Jails can easily ignore dynamic sets of CIDR
- Super simple configuration of jails, it's all in a per jail yaml file

## Why JailTime

- It's faster than crowdsec
- It's more configurable that Fail2ban, especially in support of whitelists
- It's really very efficient at what it does. Approx 3x more performant that Fail2ban
- Dynamic start, stop, restart (including reload of config) of jails / whitelists

## Architecture

```
  fsnotify / inotify kernel events
         │
         ▼
  ┌──────────────────────────────────────────────────────┐
  │  FsnotifyBackend  (single goroutine, one select loop) │
  │                                                       │
  │  WRITE → mark dirty → arm one-shot drain timer        │
  │  CREATE → discover new files matching globs           │
  │  timer fires → collect lines → processDrain()         │
  │  idle → timer nil, zero wakeups                       │
  └──────────────┬───────────────────────────────────────┘
                 │ DrainFunc (called synchronously)
                 ▼
  ┌──────────────────────────────────────────────────────┐
  │  Manager / Engine                                     │
  │                                                       │
  │  processDrain(lines []RawLine)                        │
  │    └─ for each line:                                  │
  │         ├─ JailRuntime.HandleEvent()                  │
  │         │    ├─ filter.Match (include/exclude regex)  │
  │         │    ├─ whitelist ignore_sets check           │
  │         │    └─ HitTracker.Record (sliding window)    │
  │         │         └─ threshold hit →                  │
  │         │              ActionRunner.Submit(ip, fn)    │
  │         └─ perf.RecordExecution(execTime, interval)   │
  └──────────────────────────────────────────────────────┘
         ▲
         │  HTTP over Unix socket (/run/jailtime/jailtimed.sock)
         ▼
  ┌─────────────────────────────────┐
  │  Control Server  (HTTP mux)     │
  │  /v1/health                     │
  │  /v1/jails[/{name}/…]           │
  │  /v1/whitelists[/{name}/…]      │
  │  /v1/perf                       │
  │  /v1/config/global              │
  └────────────┬────────────────────┘
               │
        jailtime CLI
```

| Component | Description |
|-----------|-------------|
| **FsnotifyBackend** | Event-driven log watching via inotify/fsnotify. Lazy one-shot drain timer — zero wakeups when idle. Re-expands globs on each CREATE event to pick up new files without restart. |
| **PollBackend** | Polling fallback (or explicit `watcher_mode: poll`). Re-expands globs each tick. |
| **Manager** | Owns all JailRuntimes. Receives `[]RawLine` batches synchronously from the backend via `processDrain`. No internal goroutine or timer. |
| **JailRuntime** | Compiles include/exclude filter regexes. Routes `RawLine` events to `HitTracker` and `ActionRunner`. Supports `watch_mode: tail` (log files) and `watch_mode: static` (IP/CIDR list files). |
| **HitTracker** | Sliding-window hit counter per IP/CIDR. |
| **ActionRunner** | Executes `on_add` shell actions asynchronously per IP. Drops duplicate in-flight actions for the same IP. |
| **Whitelists** | Special jail entries using `watch_mode: static` that maintain an in-memory IP/CIDR set. Jails reference them via `ignore_sets` to suppress `on_add` for known-safe addresses. |
| **Control Server** | HTTP-over-Unix-socket API. Handles jail/whitelist lifecycle, config inspection, perf metrics, and runtime config updates. |
| **jailtime CLI** | Client for the control socket (`jail`, `whitelist`, `config`, `perf`, `version` commands). |

## Quick start

### Build

```sh
git clone https://github.com/saygoweb/jailtime
cd jailtime
go build -o jailtimed ./cmd/jailtimed
go build -o jailtime  ./cmd/jailtime
sudo install -m 755 jailtimed /usr/sbin/jailtimed
sudo install -m 755 jailtime  /usr/bin/jailtime
```

### Automated install (Debian/Ubuntu)

```sh
sudo bash deploy/setup.sh
```

Builds, installs binaries, creates `/etc/jailtime/`, installs the systemd unit,
and starts the daemon. See `deploy/setup.sh --help` for options.

### Configure

```sh
sudo mkdir -p /etc/jailtime/jails.d
# Copy a sample jail and edit it
sudo cp /usr/share/doc/jailtime/jails.d/a-nginx-drop.yaml /etc/jailtime/jails.d/
sudo $EDITOR /etc/jailtime/jails.d/a-nginx-drop.yaml
```

`/etc/jailtime/jail.yaml` is the main config file. See
[jailtimed configuration](docs/jailtimed.md) for all options.

### Systemd

```sh
sudo systemctl enable --now jailtimed
sudo systemctl status jailtimed
journalctl -u jailtimed -f
```

### Verify

```sh
# Jail and whitelist status
jailtime jail status
jailtime whitelist status

# Inspect matched log files and test filters
jailtime config files nginx
jailtime config test nginx /var/log/nginx/access.log --matching

# Performance metrics
jailtime perf

# Live runtime config (no restart needed)
jailtime config global
jailtime config global target_latency 5s
```

## Configuration overview

```yaml
version: 1

include:
  - jails.d/*.yaml          # merge fragment files

logging:
  target: journal           # or "file"
  level: info

control:
  socket: /run/jailtime/jailtimed.sock

engine:
  watcher_mode: auto        # auto | fsnotify | poll
  target_latency: 2s        # drain timer interval
  read_from_end: true       # skip history on startup
  perf_window: 3            # cycles for perf averaging

actions:                    # global daemon lifecycle hooks
  on_start:
    - 'logger -t jailtime "daemon started"'
  on_stop:
    - 'logger -t jailtime "daemon stopped"'

jails:
  - name: nginx
    files:
      - /var/log/nginx/*/access.log
    exclude_files:
      - /var/log/nginx/internal/access.log
    filters:
      - '^(?P<ip>[0-9]{1,3}(?:\.[0-9]{1,3}){3}) .* " [45][0-9][0-9] '
    exclude_filters:
      - '" [23][0-9][0-9] '
    hit_count: 10
    find_time: 1m
    jail_time: 1h
    net_type: IP
    query_before_match: true
    query: 'ipset test jt_nginx {{ .IP }} 2>/dev/null'
    action_timeout: 30s
    actions:
      on_start:
        - 'ipset create jt_nginx hash:ip timeout 0 -exist'
      on_add:
        - 'ipset add jt_nginx {{ .IP }} timeout {{ .JailTime }} -exist'
      on_stop:
        - 'ipset destroy jt_nginx'

whitelists:
  - name: trusted
    watch_mode: static
    files:
      - /etc/jailtime/trusted.txt
    net_type: CIDR
    actions:
      on_add:
        - 'logger -t jailtime "trusted {{ .IP }} added"'
      on_remove:
        - 'logger -t jailtime "trusted {{ .IP }} removed"'
```

See [jailtimed configuration](docs/jailtimed.md) for the complete field reference.

## Sample jails

Ready-to-use configurations in `samples/jails.d/`:

| File | Scenario |
|------|---------|
| `a-nginx-drop.yaml` | Nginx — iptables DROP via ipset |
| `b-webapp-reroute.yaml` | Web app — redirect attackers to a honeypot |
| `c-nginx-drop-idempotent.yaml` | Nginx — idempotent DROP using wrapper scripts |
| `d-nginx-drop-cidr.yaml` | Nginx — ban entire CIDR blocks, survives reboot |
| `e-apache-wordpress.yaml` | Apache — WordPress login and xmlrpc brute-force |
