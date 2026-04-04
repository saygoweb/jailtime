# jailtime

A fail2ban-style intrusion-prevention daemon written in Go. `jailtime` watches log files for patterns, tracks hit counts within a configurable time window, and executes shell actions (e.g. firewall rules) when a threshold is exceeded.

## Documentation

- **[jailtime CLI](docs/jailtime.md)** — command reference, flags, examples
- **[jailtimed configuration](docs/jailtimed.md)** — daemon config, jail fields, action templates, sample jails

## Architecture

```
┌─────────────────────────────────────────────┐
│                  jailtimed                  │
│                                             │
│  ┌──────────┐   events   ┌───────────────┐  │
│  │  Watch   │──────────▶ │ Engine/Manager│  │
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
| **Watch Backend** | Watches log files via fsnotify or polling; re-expands globs each tick |
| **Engine/Manager** | Manages jail lifecycles; routes file events to JailRuntimes |
| **JailRuntime** | Compiles filters, tracks hits per IP, runs shell actions |
| **HitTracker** | Sliding-window counter per IP address |
| **Control Server** | HTTP-over-Unix-socket API for runtime control |
| **jailtime CLI** | Client for the control socket |

## Quick start

### Build

```sh
git clone https://github.com/sgw/jailtime
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
jailtime status
jailtime config files nginx
jailtime config test nginx /var/log/nginx/access.log --matching
```
