#!/usr/bin/env bash
# deploy/setup.sh — Install jailtime on a Debian/Ubuntu system.
#
# Usage (from the repository root):
#   sudo bash deploy/setup.sh [--no-start] [--no-enable] [--sample-config]
#
# Options:
#   --no-start        Install and enable the service but do not start it.
#   --no-enable       Install but do not enable or start the service.
#   --sample-config   Install the minimal sample jail.yaml even if a config
#                     already exists (backs up the existing file first).
#
# What this script does:
#   1.  Verify prerequisites (root, OS, required tools).
#   2.  Optionally install Go if not found.
#   3.  Build jailtimed and jailtime from source.
#   4.  Install binaries → /usr/sbin/
#   5.  Install wrapper tools → /usr/local/lib/jailtime/
#   6.  Create /etc/jailtime/ and /etc/jailtime/jails.d/
#   7.  Install a starter jail.yaml (skipped if one already exists, unless
#       --sample-config is given).
#   8.  Install the systemd unit and reload systemd.
#   9.  Enable and/or start the service (respecting --no-start/--no-enable).

set -euo pipefail

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${BOLD}[setup]${NC} $*"; }
success() { echo -e "${GREEN}[setup]${NC} $*"; }
warn()    { echo -e "${YELLOW}[setup]${NC} $*"; }
die()     { echo -e "${RED}[setup] ERROR:${NC} $*" >&2; exit 1; }

# ── Option parsing ────────────────────────────────────────────────────────────
OPT_NO_START=false
OPT_NO_ENABLE=false
OPT_SAMPLE_CONFIG=false

for arg in "$@"; do
    case "$arg" in
        --no-start)       OPT_NO_START=true ;;
        --no-enable)      OPT_NO_ENABLE=true ;;
        --sample-config)  OPT_SAMPLE_CONFIG=true ;;
        -h|--help)
            sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) die "Unknown option: $arg (use --help for usage)" ;;
    esac
done

# ── Locate the repository root ────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# ── Prerequisite checks ───────────────────────────────────────────────────────
[[ "$EUID" -eq 0 ]] || die "This script must be run as root (use sudo)."

# Debian/Ubuntu detection
if [[ ! -f /etc/debian_version ]]; then
    warn "This script targets Debian/Ubuntu; continuing anyway on $(uname -s)."
fi

# ── Install system packages ───────────────────────────────────────────────────
REQUIRED_PKGS=(ipset iptables)
MISSING_PKGS=()

info "Checking system packages..."
for pkg in "${REQUIRED_PKGS[@]}"; do
    if ! dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q "install ok installed"; then
        MISSING_PKGS+=("$pkg")
    fi
done

if [[ "${#MISSING_PKGS[@]}" -gt 0 ]]; then
    info "Installing missing packages: ${MISSING_PKGS[*]}"
    apt-get update -qq
    apt-get install -y -qq "${MISSING_PKGS[@]}"
fi

# ── Go toolchain ──────────────────────────────────────────────────────────────
GO_MIN_MAJOR=1
GO_MIN_MINOR=21

ensure_go() {
    if command -v go &>/dev/null; then
        GO_VER=$(go version | awk '{print $3}' | sed 's/go//')
        GO_MAJOR=$(echo "$GO_VER" | cut -d. -f1)
        GO_MINOR=$(echo "$GO_VER" | cut -d. -f2)
        if [[ "$GO_MAJOR" -gt "$GO_MIN_MAJOR" ]] || \
           { [[ "$GO_MAJOR" -eq "$GO_MIN_MAJOR" ]] && [[ "$GO_MINOR" -ge "$GO_MIN_MINOR" ]]; }; then
            info "Go $GO_VER found at $(command -v go)"
            return
        else
            warn "Go $GO_VER is too old (need >= $GO_MIN_MAJOR.$GO_MIN_MINOR); will install a newer version."
        fi
    else
        info "Go not found; installing via apt..."
    fi

    apt-get update -qq
    # golang-go in Debian bookworm / Ubuntu 22.04+ is recent enough
    if apt-get install -y -qq golang-go 2>/dev/null && command -v go &>/dev/null; then
        info "Installed Go $(go version | awk '{print $3}')"
        return
    fi

    # Fallback: download the official tarball for the current stable release
    local GOTAR_URL
    local GOARCH
    GOARCH=$(dpkg --print-architecture)
    [[ "$GOARCH" == "amd64" ]] || [[ "$GOARCH" == "arm64" ]] || \
        die "Unsupported architecture for automatic Go install: $GOARCH"
    GOTAR_URL="https://dl.google.com/go/go1.22.4.linux-${GOARCH}.tar.gz"
    info "Downloading Go from $GOTAR_URL ..."
    curl -fsSL "$GOTAR_URL" -o /tmp/go.tar.gz
    rm -rf /usr/local/go
    tar -C /usr/local -xzf /tmp/go.tar.gz
    rm /tmp/go.tar.gz
    export PATH="/usr/local/go/bin:$PATH"
    info "Installed Go $(/usr/local/go/bin/go version | awk '{print $3}')"
}

ensure_go

# ── Build ─────────────────────────────────────────────────────────────────────
info "Building jailtime binaries from $REPO_ROOT ..."
cd "$REPO_ROOT"

BUILD_DIR="$REPO_ROOT/.build"
mkdir -p "$BUILD_DIR"

go build -o "$BUILD_DIR/jailtimed" ./cmd/jailtimed
go build -o "$BUILD_DIR/jailtime"  ./cmd/jailtime

success "Build complete."

# ── Install binaries ──────────────────────────────────────────────────────────
info "Installing binaries to /usr/sbin/ ..."
install -m 755 "$BUILD_DIR/jailtimed" /usr/sbin/jailtimed
install -m 755 "$BUILD_DIR/jailtime"  /usr/sbin/jailtime
rm -rf "$BUILD_DIR"
success "Installed /usr/sbin/jailtimed and /usr/sbin/jailtime."

# ── Install wrapper tools ─────────────────────────────────────────────────────
TOOLS_DIR=/usr/local/lib/jailtime
info "Installing wrapper tools to $TOOLS_DIR ..."
mkdir -p "$TOOLS_DIR"
install -m 755 "$REPO_ROOT"/tools/*.sh "$TOOLS_DIR/"
success "Installed wrapper scripts to $TOOLS_DIR."

# ── Config directory ──────────────────────────────────────────────────────────
info "Creating config directories ..."
mkdir -p /etc/jailtime/jails.d
chmod 750 /etc/jailtime
success "Config directory: /etc/jailtime/ (with jails.d/)"

# ── Starter jail.yaml ─────────────────────────────────────────────────────────
JAIL_YAML=/etc/jailtime/jail.yaml

install_sample_config() {
    info "Installing starter jail.yaml → $JAIL_YAML ..."
    cat > "$JAIL_YAML" <<'EOF'
# /etc/jailtime/jail.yaml — jailtime main configuration
#
# Drop per-jail configs into /etc/jailtime/jails.d/*.yaml and they will
# be merged automatically at startup.
#
# See /usr/share/doc/jailtime/samples/ for example jail files.

version: 1

logging:
  target: journal      # journal | file
  # file: /var/log/jailtime.log
  level: info          # debug | info | warn | error

control:
  socket: /run/jailtime/jailtimed.sock
  timeout: 5s

engine:
  watcher_mode: auto   # auto | fsnotify | poll
  poll_interval: 2s
  read_from_end: true

include:
  - jails.d/*.yaml

# Inline jails can also be defined here:
jails: []
EOF
    chmod 640 "$JAIL_YAML"
    chown root:root "$JAIL_YAML"
    success "Installed starter config at $JAIL_YAML."
}

if [[ -f "$JAIL_YAML" ]]; then
    if $OPT_SAMPLE_CONFIG; then
        BACKUP="${JAIL_YAML}.bak.$(date +%Y%m%dT%H%M%S)"
        warn "Backing up existing $JAIL_YAML → $BACKUP"
        cp "$JAIL_YAML" "$BACKUP"
        install_sample_config
    else
        info "Config $JAIL_YAML already exists — skipping (use --sample-config to overwrite)."
    fi
else
    install_sample_config
fi

# ── Sample docs ───────────────────────────────────────────────────────────────
info "Installing sample configs to /usr/share/doc/jailtime/ ..."
mkdir -p /usr/share/doc/jailtime
cp -r "$REPO_ROOT/samples/"* /usr/share/doc/jailtime/
success "Samples available at /usr/share/doc/jailtime/."

# ── Systemd unit ──────────────────────────────────────────────────────────────
UNIT_FILE=/etc/systemd/system/jailtimed.service
info "Installing systemd unit → $UNIT_FILE ..."
install -m 644 "$REPO_ROOT/deploy/jailtimed.service" "$UNIT_FILE"
systemctl daemon-reload
success "Systemd unit installed."

# ── Enable / start ────────────────────────────────────────────────────────────
if $OPT_NO_ENABLE; then
    warn "Skipping enable and start (--no-enable)."
elif $OPT_NO_START; then
    systemctl enable jailtimed
    success "Service enabled (not started — use: systemctl start jailtimed)."
else
    systemctl enable jailtimed
    systemctl restart jailtimed
    sleep 1
    if systemctl is-active --quiet jailtimed; then
        success "jailtimed is running."
    else
        warn "jailtimed failed to start. Check logs with: journalctl -u jailtimed -n 50"
        # Non-fatal — config might be intentionally empty at this point.
    fi
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo
echo -e "${BOLD}══════════════════════════════════════════${NC}"
echo -e "${GREEN}  jailtime installation complete${NC}"
echo -e "${BOLD}══════════════════════════════════════════${NC}"
echo
echo "  Binaries:   /usr/sbin/jailtimed   /usr/sbin/jailtime"
echo "  Config:     /etc/jailtime/jail.yaml"
echo "  Jails dir:  /etc/jailtime/jails.d/"
echo "  Tools:      /usr/local/lib/jailtime/"
echo "  Samples:    /usr/share/doc/jailtime/"
echo
echo "  Quick start:"
echo "    1. Copy a sample jail:  cp /usr/share/doc/jailtime/jails.d/a-nginx-drop.yaml \\"
echo "                               /etc/jailtime/jails.d/"
echo "    2. Edit as needed:      \$EDITOR /etc/jailtime/jails.d/a-nginx-drop.yaml"
echo "    3. Start the daemon:    systemctl start jailtimed"
echo "    4. Check status:        jailtime jail status"
echo
