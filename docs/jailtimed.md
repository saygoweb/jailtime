# jailtimed Configuration Reference

`jailtimed` is a fail2ban-style intrusion-prevention daemon. It watches log files
for patterns, tracks hit counts within a configurable time window, and executes
shell actions (e.g. firewall rules) when a threshold is exceeded.

For daemon installation and the `jailtime` CLI, see:
- [jailtime CLI](jailtime.md)

---

## Configuration file

By default jailtimed loads `/etc/jailtime/jail.yaml`. Pass `--config <path>` to
use a different file.

```sh
jailtimed --config /etc/jailtime/jail.yaml
```

The file format is YAML. **Unknown fields cause a parse error** (strict parsing),
so typos in field names are caught immediately.

---

## Top-level fields

```yaml
version: 1          # required; must be 1

include: []         # optional glob patterns — see "Fragment files" below

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
    # ...
```

---

## `logging`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `target` | string | `journal` | `journal` (stdout/systemd journal) or `file` |
| `file` | string | — | Log file path; required when `target: file` |
| `level` | string | `info` | `debug`, `info`, `warn`, or `error` |

---

## `control`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `socket` | string | `/run/jailtime/jailtimed.sock` | Unix domain socket path for the control API |
| `timeout` | duration | — | Request timeout for control connections |

---

## `engine`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `watcher_mode` | string | `auto` | File watching backend: `auto`, `fsnotify`, or `poll` |
| `poll_interval` | duration | `2s` | How often to poll for new log lines or new glob matches |
| `read_from_end` | bool | `true` | Start reading existing log files from EOF (ignore history) |

**`watcher_mode`:** `auto` tries fsnotify first and falls back to poll if unavailable.
Both modes re-evaluate glob patterns on every tick, so files in new subdirectories
are discovered automatically without a restart.

---

## Duration format

All duration fields accept Go duration strings plus two extra suffixes:

| Suffix | Meaning | Example |
|--------|---------|---------|
| `s` | seconds | `30s` |
| `m` | minutes | `10m` |
| `h` | hours | `1h` |
| `d` | days | `7d` |
| `w` | weeks | `2w` |

Fractional values are supported: `1.5h`, `0.5d`.

---

## Fragment files (`include`)

The `include` key takes a list of glob patterns. Each matching file is loaded
and its `jails:` list is merged into the main config. This lets you keep each
jail in its own file under `jails.d/`.

```yaml
include:
  - jails.d/*.yaml
```

Fragment files may **only** contain a `jails:` key. All other top-level settings
(`logging`, `engine`, `control`, etc.) must be in the main config file.

Relative patterns are resolved relative to the directory containing the main
config file. The main config file itself is never re-included.

```sh
# Example layout
/etc/jailtime/
├── jail.yaml          # main config — sets logging, engine, control; includes jails.d/
└── jails.d/
    ├── sshd.yaml
    ├── nginx.yaml
    └── apache2.yaml
```

---

## Jail configuration

Each entry in the `jails:` list defines one jail.

```yaml
jails:
  - name: sshd
    enabled: true
    files:
      - /var/log/auth.log
    filters:
      - 'Failed password for .* from (?P<ip>[0-9.]+) port'
    exclude_filters:
      - 'from 192\.168\.'
    hit_count: 5
    find_time: 10m
    jail_time: 1h
    net_type: IP
    query: 'ipset test jt_{{ .Jail }} {{ .IP }} 2>/dev/null'
    actions:
      on_start:
        - 'logger -t jailtime "jail {{ .Jail }} started"'
      on_match:
        - 'nft add element inet filter blocklist { {{ .IP }} }'
      on_stop:
        - 'logger -t jailtime "jail {{ .Jail }} stopped"'
      on_restart: []
```

### Jail fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | — | **Required.** Unique jail name |
| `enabled` | bool | `true` | Set to `false` to disable without removing the config |
| `files` | []string | — | **Required.** Glob patterns for log files to watch |
| `filters` | []string | — | **Required.** Include regex patterns (see below) |
| `exclude_filters` | []string | — | Exclude regex patterns — a match suppresses the event |
| `hit_count` | int | `1` | Number of matches within `find_time` before actions fire |
| `find_time` | duration | — | **Required.** Sliding window for hit counting |
| `jail_time` | duration | — | **Required.** Ban duration, passed as `{{ .JailTime }}` in seconds to actions |
| `net_type` | string | `IP` | `IP` — validate capture as a plain IP; `CIDR` — validate as a CIDR block |
| `query` | string | — | Pre-check command; if it exits **0** (already blocked), `on_match` is skipped |
| `actions` | object | — | Shell commands for lifecycle events (see below) |

### `files` — glob patterns

`files` accepts shell glob patterns. Both single-directory and multi-level
wildcards work:

```yaml
files:
  - /var/log/nginx/access.log          # exact path
  - /var/log/apache2/*/access.log      # one wildcard subdirectory
  - /var/log/app/*.log                 # any .log in a directory
```

Globs are re-expanded on every watch tick, so files created in new
subdirectories after the daemon starts are picked up automatically.

Use `jailtime config files <jail>` to inspect which files currently match.

### `filters` — include patterns

Filters are Go regular expressions. The first filter that matches a log line
wins; subsequent filters are not checked.

**The regex must contain a named capture group `(?P<ip>...)`.** This group
provides the IP address (or CIDR when `net_type: CIDR`) used for hit tracking
and action templates.

```yaml
filters:
  # SSH failed password — captures the source IP
  - 'Failed password for .* from (?P<ip>[0-9.]+) port'

  # Nginx combined log — captures first field
  - '^(?P<ip>[0-9]{1,3}(?:\.[0-9]{1,3}){3}) .* " [45][0-9][0-9] '
```

Use `jailtime config test <jail> <logfile> --matching` to verify your patterns
against real log data.

### `exclude_filters` — suppress patterns

If any exclude filter matches a line, that line is ignored even if an include
filter also matched. Useful for whitelisting known-good sources or response
codes.

```yaml
exclude_filters:
  - '" [23][0-9][0-9] '      # ignore 2xx/3xx responses
  - 'from 192\.168\.'        # ignore RFC-1918 sources
  - 'whitelist'
```

### `query` — pre-check

When `query` is set, jailtimed runs it as a shell command before executing
`on_match` actions. If the command exits **0**, the IP is considered already
blocked and `on_match` is skipped. Any non-zero exit code (including errors)
means "not yet blocked — proceed".

```yaml
query: 'ipset test jt_{{ .Jail }} {{ .IP }} 2>/dev/null'
```

---

## Actions

Actions are lists of shell command strings, executed in order. Each string is
a Go `text/template` — template variables are substituted before the command
is run.

### Action hooks

| Hook | When it runs |
|------|-------------|
| `on_start` | When the jail starts (daemon startup or `jailtime jail start`) |
| `on_match` | When `hit_count` is reached within `find_time`; skipped if `query` exits 0 |
| `on_stop` | When the jail stops (`jailtime jail stop` or daemon shutdown) |
| `on_restart` | When `jailtime jail restart` is issued (runs after `on_stop`/`on_start` for the named jail) |

`on_match` is **required** — a jail with no `on_match` actions fails validation.

### Template variables

| Variable | Description |
|----------|-------------|
| `{{ .IP }}` | Matched IP address (or CIDR when `net_type: CIDR`) |
| `{{ .Jail }}` | Jail name |
| `{{ .File }}` | Log file path that triggered the match |
| `{{ .Line }}` | The log line that matched |
| `{{ .JailTime }}` | `jail_time` expressed in seconds (integer) |
| `{{ .FindTime }}` | `find_time` expressed in seconds (integer) |
| `{{ .HitCount }}` | Hit count at the moment the threshold was crossed |
| `{{ .Timestamp }}` | RFC3339 timestamp of the match event |

---

## Validation rules

jailtimed validates the config at startup and on every `restart`. Errors are
reported with the jail name and field:

- `name` must be non-empty and unique across all jails (including merged fragments)
- `files` must contain at least one entry
- `filters` must contain at least one entry
- `actions.on_match` must contain at least one entry
- `find_time` must be > 0
- `jail_time` must be > 0
- `hit_count` must be ≥ 1
- `net_type` must be `IP` or `CIDR`
- All `filters` and `exclude_filters` must be valid Go regular expressions

---

## Systemd

Install the provided unit file and manage jailtimed with systemctl:

```sh
sudo install -m 644 deploy/jailtimed.service /etc/systemd/system/jailtimed.service
sudo systemctl daemon-reload
sudo systemctl enable --now jailtimed

# Check status
sudo systemctl status jailtimed
journalctl -u jailtimed -f
```

The unit file runs jailtimed as root (required for iptables/nft/ipset operations).
Adjust `deploy/jailtimed.service` if you use a capability-based setup instead.

---

## Sample jails

Ready-to-use jail definitions are installed to `/usr/share/doc/jailtime/jails.d/`
by `deploy/setup.sh`, and are also available in the repository under `samples/jails.d/`.

| File | Scenario |
|------|---------|
| `a-nginx-drop.yaml` | Nginx — iptables DROP via ipset |
| `b-webapp-reroute.yaml` | Web app — redirect attackers to a honeypot |
| `c-nginx-drop-idempotent.yaml` | Nginx — idempotent DROP using wrapper scripts |
| `d-nginx-drop-cidr.yaml` | Nginx — ban entire CIDR blocks, survives reboot |
| `e-apache-wordpress.yaml` | Apache — WordPress login and xmlrpc brute-force |

Wrapper scripts for idempotent ipset/iptables operations are in `samples/tools/`
and are installed to `/usr/local/lib/jailtime/` by `deploy/setup.sh`.

```sh
# Use a sample jail
cp /usr/share/doc/jailtime/jails.d/a-nginx-drop.yaml /etc/jailtime/jails.d/
$EDITOR /etc/jailtime/jails.d/a-nginx-drop.yaml
jailtime jail restart nginx
```
