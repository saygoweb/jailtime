# jailtimed Configuration Reference

`jailtimed` is a fail2ban-style intrusion-prevention daemon. It watches log files
for patterns, tracks hit counts within a sliding time window, and executes shell
actions (e.g. firewall rules) when a threshold is exceeded. It also supports
static IP/CIDR allow-list files (whitelists) that are kept in memory and can
suppress `on_add` actions automatically.

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
  target_latency: 2s
  read_from_end: true
  perf_window: 3

actions:            # optional daemon-level lifecycle hooks
  on_start: []
  on_stop: []

jails:
  - name: sshd
    # ...

whitelists:         # optional static IP/CIDR set watchers
  - name: trusted
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
| `watcher_mode` | string | `auto` | File watching backend: `auto`, `fsnotify`, `inotify`, `os`, or `poll` |
| `poll_interval` | duration | `2s` | Polling interval when `watcher_mode: poll` |
| `target_latency` | duration | `2s` | Target drain interval for event-driven mode; controls how quickly log lines are processed |
| `read_from_end` | bool | `true` | Start reading existing log files from EOF (ignore history) |
| `perf_window` | int | `3` | Number of drain cycles used for rolling performance averages |

**`watcher_mode`:** `auto` tries fsnotify/inotify first and falls back to poll if
unavailable. `inotify` and `os` are aliases for `fsnotify`. In event-driven mode,
the drain timer is only armed when a file is written to — zero wakeups when idle.
Both modes re-evaluate glob patterns on each tick/event, so new files in
subdirectories are discovered automatically without a restart.

Some engine fields can be updated at runtime without restarting the daemon:

```sh
jailtime config global target_latency 5s
jailtime config global perf_window 5
```

---

## `actions` — global daemon hooks

Global actions run at daemon startup and shutdown, before any jail is started /
after all jails have stopped. They use the same template syntax as jail actions
but only `{{ .Jail }}` is available (it is empty for global actions).

```yaml
actions:
  on_start:
    - 'ipset create jt_global hash:ip -exist'
    - 'logger -t jailtime "daemon started"'
  on_stop:
    - 'logger -t jailtime "daemon stopped"'
```

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
and its `jails:` and/or `whitelists:` lists are merged into the main config.
This lets you keep each jail or whitelist in its own file under `jails.d/`.

```yaml
include:
  - jails.d/*.yaml
```

Fragment files may **only** contain `jails:` and/or `whitelists:` keys. All other
top-level settings (`logging`, `engine`, `control`, `actions`, etc.) must be in
the main config file.

Relative patterns are resolved relative to the directory containing the main
config file. The main config file itself is never re-included.

```sh
# Example layout
/etc/jailtime/
├── jail.yaml          # main config — sets logging, engine, control; includes jails.d/
└── jails.d/
    ├── sshd.yaml
    ├── nginx.yaml
    ├── apache2.yaml
    └── trusted.yaml   # a whitelist fragment
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
    exclude_files:
      - /var/log/auth.log.1
    filters:
      - 'Failed password for .* from (?P<ip>[0-9.]+) port'
    exclude_filters:
      - 'from 192\.168\.'
    hit_count: 5
    find_time: 10m
    jail_time: 1h
    net_type: IP
    query_before_match: true
    query: 'ipset test jt_{{ .Jail }} {{ .IP }} 2>/dev/null'
    action_timeout: 30s
    actions:
      on_start:
        - 'ipset create jt_{{ .Jail }} hash:ip timeout 0 -exist'
      on_add:
        - 'ipset add jt_{{ .Jail }} {{ .IP }} timeout {{ .JailTime }} -exist'
      on_stop:
        - 'ipset destroy jt_{{ .Jail }}'
      on_restart: []
```

### Jail fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | — | **Required.** Unique jail name |
| `enabled` | bool | `true` | Set to `false` to disable without removing the config |
| `files` | []string | — | **Required.** Glob patterns for log files to watch |
| `exclude_files` | []string | — | Glob patterns to exclude from `files` matches |
| `filters` | []string | — | Include regex patterns; **required** for `watch_mode: tail` (see below) |
| `exclude_filters` | []string | — | Exclude regex patterns — a match suppresses the event |
| `hit_count` | int | `1` | Number of matches within `find_time` before actions fire |
| `find_time` | duration | — | **Required.** Sliding window for hit counting |
| `jail_time` | duration | — | **Required.** Ban duration, passed as `{{ .JailTime }}` in seconds to actions |
| `net_type` | string | `IP` | `IP` — validate as a plain IP; `CIDR` — validate as a CIDR block |
| `watch_mode` | string | `tail` | `tail` — watch log files for new lines; `static` — watch a file of IP/CIDR entries (see Whitelists) |
| `query` | string | — | Pre-check command; see `query_before_match` |
| `query_before_match` | bool | `false` | When `true`, run `query` before `on_add`; if `query` exits **0**, `on_add` is skipped |
| `tags_from` | []string | `[]` | Ordered list of tag sources; resolved values are joined with `,` and exposed as `{{ .Tags }}` — see [Tags](#tags) |
| `action_timeout` | duration | `30s` | Maximum time allowed for each individual action command |
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

### `exclude_files` — file exclusions

`exclude_files` takes the same glob syntax as `files`. Any path that matches
an exclude glob is ignored, even if it also matches a `files` glob.

```yaml
files:
  - /var/log/apache2/*/access.log
exclude_files:
  - /var/log/apache2/internal/access.log
```

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
filter also matched. Useful for suppressing known-good sources or response codes.

```yaml
exclude_filters:
  - '" [23][0-9][0-9] '      # ignore 2xx/3xx responses
  - 'from 192\.168\.'        # ignore RFC-1918 sources
```

### Tags

The `tags_from` field is an ordered array of tag source names. Each source is resolved to a string; empty values are omitted. The remaining values are joined with `,` to form the `{{ .Tags }}` template variable and the `tags` structured log field.

| Source | Produces |
|---|---|
| `parent_dir` | Base name of the directory containing the matched log file |
| `match_tag1`…`match_tag9` | Text captured by `(?P<tag1>…)`…`(?P<tag9>…)` named groups in the matched filter |

`tags_from` defaults to `[]` (empty — no tags computed or logged).

**Example — log the virtual-host name:**

```yaml
jails:
  - name: apache-vhosts
    files:
      - /var/log/apache2/*/access.log
    tags_from:
      - parent_dir
    filters:
      - '(?P<ip>[0-9.]+) .* " [45][0-9][0-9] '
    actions:
      on_add:
        - 'nft add element inet filter blacklist { {{ .IP }} } comment "{{ .Tags }}"'
```

If the matched file is `/var/log/apache2/site.example/access.log` then `{{ .Tags }}` evaluates to `site.example`.

**Example — combine parent directory and a filter capture group:**

```yaml
tags_from:
  - parent_dir
  - match_tag1
filters:
  - '(?P<ip>[0-9.]+).*service=(?P<tag1>\w+)'
# Tags = "site.example,webapp"
```

---

### `query` — pre-check

When `query_before_match: true` and `query` is set, jailtimed runs the query as
a shell command before executing `on_add` actions. If the command exits **0**,
the IP is considered already blocked and `on_add` is skipped. Any non-zero exit
code (including errors) means "not yet blocked — proceed".

```yaml
query_before_match: true
query: 'ipset test jt_{{ .Jail }} {{ .IP }} 2>/dev/null'
```

---

## Actions

Actions are lists of shell command strings, executed in order. Each string is
a Go `text/template` — template variables are substituted before the command
is run. Each action command runs under `/bin/sh -c`.

### Action hooks

| Hook | When it runs |
|------|-------------|
| `on_start` | When the jail starts (daemon startup or `jailtime jail start`) |
| `on_add` | When `hit_count` is reached within `find_time`; skipped if `query` exits 0 (when `query_before_match: true`) |
| `on_remove` | When an entry is removed from a `watch_mode: static` file |
| `on_stop` | When the jail stops (`jailtime jail stop` or daemon shutdown) |
| `on_restart` | When `jailtime jail restart` is issued (runs after `on_stop`/`on_start` for the named jail) |

`on_add` is **required** for `watch_mode: tail` jails — a jail with no `on_add`
actions fails validation. (`on_match` is accepted as a deprecated alias for `on_add`.)

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
| `{{ .Tags }}` | Comma-joined tag values from `tags_from` (empty string when no tags configured) |

---

## Whitelists

Whitelists are entries under the top-level `whitelists:` key. They use the same
`JailConfig` structure as jails but with `watch_mode: static`. jailtimed reads
the listed files (one IP or CIDR per line) and maintains an in-memory set. When
a line is added to or removed from the file, `on_add` / `on_remove` actions fire.

```yaml
whitelists:
  - name: trusted
    watch_mode: static
    files:
      - /etc/jailtime/trusted.txt
    net_type: CIDR
    action_timeout: 10s
    actions:
      on_add:
        - 'logger -t jailtime "trusted CIDR {{ .IP }} loaded"'
      on_remove:
        - 'logger -t jailtime "trusted CIDR {{ .IP }} removed"'
```

Manage whitelists at runtime using `jailtime whitelist status|start|stop|restart`.

---

## Validation rules

jailtimed validates the config at startup and on every `restart`. Errors are
reported with the jail name and field:

- `name` must be non-empty and unique across all jails and whitelists (including merged fragments)
- `files` must contain at least one entry
- `filters` must contain at least one entry (for `watch_mode: tail` jails)
- `actions.on_add` (or `actions.on_match`) must contain at least one entry for `watch_mode: tail` jails
- `find_time` must be > 0
- `jail_time` must be > 0
- `hit_count` must be ≥ 1
- `net_type` must be `IP` or `CIDR`
- All `filters` and `exclude_filters` must be valid Go regular expressions
- Each entry in `tags_from` must be `"parent_dir"` or `"match_tag1"`…`"match_tag9"`

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
It keeps `ProtectSystem=strict` enabled while allowing writes only to the
systemd-managed directories `/var/cache/whois_cache` and `/var/log/intrusion`,
which are used by the bundled RDAP/cache tooling and intrusion log actions.
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

Wrapper scripts for idempotent ipset/iptables operations are in `tools/`
and are installed to `/usr/local/lib/jailtime/` by `deploy/setup.sh`.

```sh
# Use a sample jail
cp /usr/share/doc/jailtime/jails.d/a-nginx-drop.yaml /etc/jailtime/jails.d/
$EDITOR /etc/jailtime/jails.d/a-nginx-drop.yaml
jailtime jail restart nginx
```
