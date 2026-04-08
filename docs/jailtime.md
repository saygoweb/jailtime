# jailtime CLI Reference

`jailtime` is the command-line client for the [`jailtimed`](jailtimed.md) daemon.
All commands communicate with the daemon over a Unix domain socket.

## Global flags

These flags are available on every command.

| Flag | Default | Description |
|------|---------|-------------|
| `--socket <path>` | `/run/jailtime/jailtimed.sock` | Path to the jailtimed control socket |

```sh
# Use a non-default socket
jailtime --socket /run/custom/jailtime.sock jail status
```

---

## Commands

### `jail status`

Show the running status of all jails, or a single named jail.

```
jailtime jail status [jail]
```

```sh
# All jails
jailtime jail status

# One jail
jailtime jail status sshd
```

**Output** (tabular):

```
NAME     STATUS
sshd     started
nginx    started
webapp   stopped
```

---

### `jail start`

Start a jail. Runs the jail's `on_start` actions.

```
jailtime jail start <jail>
```

```sh
jailtime jail start sshd
```

---

### `jail stop`

Stop a jail. Runs the jail's `on_stop` actions.

```
jailtime jail stop <jail>
```

```sh
jailtime jail stop sshd
```

---

### `jail restart`

Restart a jail. jailtimed reloads its configuration from disk before restarting,
so any changes to `jail.yaml` or fragment files under `jails.d/` take effect immediately.
Also reconciles new or removed jails found in the updated config.

```
jailtime jail restart <jail>
```

```sh
jailtime jail restart sshd
```

---

### `version`

Print the jailtime version.

```
jailtime version
```

---

### `config files`

Expand the file glob patterns configured for a jail and list every currently
matching file path. Globs are re-evaluated at query time, so files in
subdirectories created after the daemon started will appear.

```
jailtime config files <jail> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--limit <n>` | `10` | Maximum number of file paths to return. `0` = no limit. |
| `--log` | false | Also emit each matched path to the daemon's own logger (useful for system log correlation). |

```sh
# List up to 10 matched files (default)
jailtime config files apache2

# List all matched files
jailtime config files apache2 --limit=0

# List and log via daemon
jailtime config files nginx --log
```

**Output:**

```
/var/log/apache2/site1/access.log
/var/log/apache2/site2/access.log
(2 file(s) matched)
```

---

### `config test`

Read every line of the given log file through the jail's filters without
modifying hit counts or triggering any actions. Useful for verifying that
filter patterns match (or do not match) real log entries before deploying.

```
jailtime config test <jail> <file> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--matching` | false | Print the lines that matched the filters. |
| `--limit <n>` | `10` | Maximum number of matching lines to print when `--matching` is set. `0` = no limit. |

```sh
# Count matches only
jailtime config test sshd /var/log/auth.log

# Show matching lines (up to 10)
jailtime config test sshd /var/log/auth.log --matching

# Show all matching lines
jailtime config test nginx /var/log/nginx/access.log --matching --limit=0

# Show up to 20 matching lines
jailtime config test apache2 /var/log/apache2/access.log --matching --limit=20
```

**Output:**

```
Total lines:    4821
Matching lines: 37
```

With `--matching`:

```
Total lines:    4821
Matching lines: 37

1.2.3.4 - - [04/Apr/2026:03:10:01 +0000] "POST /wp-login.php HTTP/1.1" 200 4823 ...
5.6.7.8 - - [04/Apr/2026:03:11:44 +0000] "GET /xmlrpc.php HTTP/1.1" 200 674 ...
...
```

> **Note:** The file path is resolved on the daemon's host filesystem.
> When running jailtimed in a container or remote host, the path must be
> accessible to the daemon process, not the jailtime client.
