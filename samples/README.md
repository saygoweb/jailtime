# jailtime sample configurations
#
# This directory contains ready-to-use jail definitions for common scenarios.
# Drop the files you need into /etc/jailtime/jails.d/ and add an include to
# your main /etc/jailtime/jail.yaml:
#
#   include:
#     - jails.d/*.yaml

## Samples

### A — `a-nginx-drop.yaml` — Simple iptables DROP via ipset

Monitors the nginx access log for repeated HTTP 4xx/5xx responses and bans
offending IPs on TCP 80/443 using an ipset with a per-entry kernel timeout.

| Event      | Action |
|------------|--------|
| on_start   | `ipset create jt_nginx hash:ip timeout 0` + iptables INPUT DROP rule |
| on_match   | `ipset add jt_nginx <IP> timeout <JailTime>` |
| on_stop    | remove iptables rule + `ipset destroy jt_nginx` |
| on_restart | — |

---

### B — `b-webapp-reroute.yaml` — Transparent reroute via sniproxy/virtualproxy

Instead of dropping traffic, silently redirects suspicious HTTPS (443) traffic
to a local honeypot or captive portal on port 8081 using an iptables NAT
PREROUTING rule.

| Event      | Action |
|------------|--------|
| on_start   | `ipset create jt_webapp …` + iptables nat PREROUTING REDIRECT 443→8081 |
| on_match   | `ipset add jt_webapp <IP> timeout <JailTime>` |
| on_stop    | remove NAT rule + `ipset destroy jt_webapp` |
| on_restart | — |

Requires a service listening on 127.0.0.1:8081 (e.g. sniproxy, nginx tarpit).

---

### C — `c-nginx-drop-idempotent.yaml` — Idempotent DROP using wrapper scripts

Same behaviour as A, but every ipset/iptables operation is wrapped in a shell
script that checks before acting, making each step safe to run multiple times:

- **on_start** never errors if the ipset or rule already exists.
- **on_stop** *flushes* the ipset (removes entries) rather than destroying it,
  so the set persists across jailtime restarts and can be saved by
  `ipset-persistent` / `netfilter-persistent` across reboots.
- **on_match** uses `ipset add -exist` to update the timeout if the IP is
  already present.

Install the wrapper scripts once:

```bash
install -m 755 -D tools/*.sh /usr/local/lib/jailtime/
```

---

## tools/

| Script | Purpose |
|--------|---------|
| `ipset-ensure.sh` | Create an ipset only if it does not already exist |
| `ipset-flush.sh` | Flush entries from an ipset without destroying it |
| `iptables-ensure.sh` | Insert an iptables rule only if not already present (`-C` check before `-I`/`-A`) |
| `iptables-remove.sh` | Delete an iptables rule only if present (`-C` check before `-D`) |

## Template variables

All action command strings are Go `text/template` strings. Available variables:

| Variable | Description |
|----------|-------------|
| `{{ .IP }}` | Extracted IP address |
| `{{ .Jail }}` | Jail name (used for ipset name `jt_{{ .Jail }}`) |
| `{{ .JailTime }}` | Jail duration in seconds |
| `{{ .FindTime }}` | Find-time window in seconds |
| `{{ .HitCount }}` | Configured hit threshold |
| `{{ .File }}` | Source log file |
| `{{ .Line }}` | Matching log line |
| `{{ .Timestamp }}` | RFC3339 timestamp of the match event |
