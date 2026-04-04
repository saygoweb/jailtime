# jailtime

A fail2ban-style intrusion-prevention daemon written in Go. `jailtime` watches log files for patterns, tracks hit counts within a configurable time window, and executes shell actions (e.g. firewall rules) when a threshold is exceeded.

## Architecture

```
┌─────────────────────────────────────────────┐
│                  jailtimed                  │
│                                             │
│  ┌──────────┐   events   ┌───────────────┐  │
│  │  Watch   │──────────▶│ Engine/Manager│  │
│  │ Backend  │            │  JailRuntime  │  │
│  └──────────┘            │  HitTracker   │  │
│                          └───────────────┘  │
│                                             │
│  ┌──────────────────────────────────────┐   │
│  │  Control Server (Unix socket HTTP)   │   │
│  └──────────────────────────────────────┘   │
└─────────────────────────────────────────────┘
         ▲
         │ jailtime CLI
```

| Component | Description |
|-----------|-------------|
| **Watch Backend** | Watches log files via fsnotify, OS inotify, or polling |
| **Engine/Manager** | Manages jail lifecycles; routes file events to JailRuntimes |
| **JailRuntime** | Compiles filters, tracks hits, runs shell actions |
| **HitTracker** | Sliding-window counter per IP address |
| **Control Server** | HTTP-over-Unix-socket API for runtime control |
| **jailtime CLI** | Client for the control socket |

## Quick Start

### Build

```sh
git clone https://github.com/sgw/jailtime
cd jailtime
go build -o jailtimed ./cmd/jailtimed
go build -o jailtime  ./cmd/jailtime
sudo install -m 755 jailtimed /usr/sbin/jailtimed
sudo install -m 755 jailtime  /usr/bin/jailtime
```

### Configure

```sh
sudo mkdir -p /etc/jailtime
sudo install -m 640 deploy/jail.yaml.example /etc/jailtime/jail.yaml
# Edit /etc/jailtime/jail.yaml for your environment
```

### Install systemd unit

```sh
sudo install -m 644 deploy/jailtimed.service /etc/systemd/system/jailtimed.service
sudo systemctl daemon-reload
sudo systemctl enable --now jailtimed
```

## Example minimal config

```yaml
version: 1

logging:
  target: journal
  level: info

control:
  socket: /run/jailtime/jailtimed.sock

engine:
  watcher_mode: auto
  poll_interval: 2s
  read_from_end: true

jails:
  - name: sshd
    enabled: true
    files:
      - /var/log/auth.log
    filters:
      - 'Failed password for .* from (?P<ip>[0-9.]+) port'
    hit_count: 5
    find_time: 10m
    jail_time: 1h
    net_type: IP
    actions:
      on_start:
        - 'logger -t jailtime "jail sshd started"'
      on_match:
        - 'nft add element inet filter blocklist { {{.IP}} }'
      on_stop:
        - 'logger -t jailtime "jail sshd stopped"'
```

## CLI Usage

```sh
# Show status of all jails
jailtime status

# Show status of a specific jail
jailtime status sshd

# Start / stop / restart a jail
jailtime start   sshd
jailtime stop    sshd
jailtime restart sshd

# Print version
jailtime version

# Use a custom socket
jailtime --socket /run/jailtime/jailtimed.sock status
```

## Configuration Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `version` | int | — | Config schema version (use `1`) |
| `logging.target` | string | `journal` | `journal` (stdout) or `file` |
| `logging.file` | string | — | Log file path (when target=file) |
| `logging.level` | string | `info` | `debug`, `info`, `warn`, `error` |
| `control.socket` | string | `/run/jailtime/jailtimed.sock` | Unix socket path |
| `control.timeout` | duration | — | Request timeout |
| `engine.watcher_mode` | string | `auto` | `auto`, `os`, `fsnotify`, `poll` |
| `engine.poll_interval` | duration | `2s` | Poll interval (poll mode) |
| `engine.read_from_end` | bool | `true` | Start reading files from EOF |
| `jails[].name` | string | — | Unique jail name |
| `jails[].enabled` | bool | `true` | Enable/disable jail |
| `jails[].files` | []string | — | Glob patterns for log files |
| `jails[].filters` | []string | — | Include regex patterns (named capture `ip`) |
| `jails[].exclude_filters` | []string | — | Exclude regex patterns |
| `jails[].hit_count` | int | `1` | Hits before action fires |
| `jails[].find_time` | duration | `1m` | Sliding window for hit counting |
| `jails[].jail_time` | duration | — | Ban duration (passed as `{{.JailTime}}` in seconds) |
| `jails[].net_type` | string | `IP` | `IP` or `CIDR` |
| `jails[].query` | string | — | Pre-check command; skip action if exit 0 |
| `jails[].actions.on_start` | []string | — | Commands run when jail starts |
| `jails[].actions.on_match` | []string | — | Commands run on threshold hit |
| `jails[].actions.on_stop` | []string | — | Commands run when jail stops |
| `jails[].actions.on_restart` | []string | — | Commands run on restart |

### Action template variables

Actions are Go `text/template` strings with these variables:

| Variable | Description |
|----------|-------------|
| `{{.IP}}` | Matched IP address |
| `{{.Jail}}` | Jail name |
| `{{.File}}` | Log file that triggered the match |
| `{{.Line}}` | Log line that matched |
| `{{.JailTime}}` | `jail_time` in seconds |
| `{{.FindTime}}` | `find_time` in seconds |
| `{{.HitCount}}` | Current hit count |
| `{{.Timestamp}}` | RFC3339 timestamp |

## Systemd Setup

```sh
sudo install -m 644 deploy/jailtimed.service /etc/systemd/system/jailtimed.service
sudo systemctl daemon-reload
sudo systemctl enable jailtimed
sudo systemctl start  jailtimed
sudo systemctl status jailtimed
journalctl -u jailtimed -f
```
