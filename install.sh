#!/usr/bin/env bash
set -euo pipefail

# ──────────────────────────────────────────────────────────────────────
# Xray-core Patched Installer
# Replaces official xray-core with patched version that kills active
# connections on user removal (no restart needed to disconnect users).
#
# Supported panels: Marzban, 3x-ui, x-ui (alireza), PasarGuard
#
# Usage:
#   bash <(curl -sL https://raw.githubusercontent.com/nightcrawler42/Xray-core/main/install.sh)
# ──────────────────────────────────────────────────────────────────────

REPO="https://github.com/nightcrawler42/Xray-core.git"
BRANCH="main"
BUILD_DIR="/tmp/xray-core-build"
GO_VERSION="1.23.4"
GO_MIN_VERSION="1.22"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${GREEN}[✓]${NC} $1"; }
warn() { echo -e "${YELLOW}[!]${NC} $1"; }
err()  { echo -e "${RED}[✗]${NC} $1"; exit 1; }
info() { echo -e "${CYAN}[i]${NC} $1"; }

# ── Root check ────────────────────────────────────────────────────────
[[ $EUID -ne 0 ]] && err "Run as root"

# ── Detect architecture ──────────────────────────────────────────────
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        armv7l)        ARCH="arm" ;;
        *)             err "Unsupported architecture: $(uname -m)" ;;
    esac
    OS="linux"
    log "Architecture: ${OS}/${ARCH}"
}

# ── Detect panel ─────────────────────────────────────────────────────
detect_panel() {
    PANEL=""
    XRAY_BIN=""
    RESTART_CMD=""

    # Marzban (Docker)
    if [[ -f /opt/marzban/docker-compose.yml ]] || docker ps 2>/dev/null | grep -q marzban; then
        PANEL="marzban"
        if [[ -f /opt/marzban/.env ]]; then
            XRAY_BIN=$(grep -oP 'XRAY_EXECUTABLE_PATH\s*=\s*"\K[^"]+' /opt/marzban/.env 2>/dev/null || true)
        fi
        [[ -z "$XRAY_BIN" ]] && XRAY_BIN="/var/lib/marzban/xray-core/xray"
        RESTART_CMD="marzban_restart"
        log "Detected: Marzban"
        log "Xray binary: $XRAY_BIN"
        return
    fi

    # 3x-ui / x-ui (alireza) — both use same paths
    if systemctl list-units --type=service --all 2>/dev/null | grep -q 'x-ui'; then
        if [[ -d /usr/local/x-ui ]]; then
            XRAY_BIN="/usr/local/x-ui/bin/xray-linux-${ARCH}"
            RESTART_CMD="systemctl restart x-ui"

            # Distinguish 3x-ui vs x-ui
            if [[ -f /usr/local/x-ui/x-ui ]] && /usr/local/x-ui/x-ui --version 2>&1 | grep -qi "3x-ui"; then
                PANEL="3x-ui"
                log "Detected: 3x-ui"
            else
                PANEL="x-ui"
                log "Detected: x-ui (alireza)"
            fi
            log "Xray binary: $XRAY_BIN"
            return
        fi
    fi

    # PasarGuard (Docker node)
    if docker ps 2>/dev/null | grep -q pasarguard; then
        PANEL="pasarguard"
        XRAY_BIN="/var/lib/pg-node/xray"
        RESTART_CMD="pasarguard_restart"
        log "Detected: PasarGuard"
        log "Xray binary: $XRAY_BIN"
        return
    fi

    # Fallback: check for standalone xray
    if command -v xray &>/dev/null; then
        PANEL="standalone"
        XRAY_BIN=$(command -v xray)
        RESTART_CMD="systemctl restart xray 2>/dev/null || true"
        warn "No panel detected, found standalone xray at: $XRAY_BIN"
        return
    fi

    err "No supported panel or xray installation found"
}

# ── Install Go if needed ─────────────────────────────────────────────
ensure_go() {
    if command -v go &>/dev/null; then
        local ver
        ver=$(go version | grep -oP '\d+\.\d+' | head -1)
        if printf '%s\n%s\n' "$GO_MIN_VERSION" "$ver" | sort -V | head -1 | grep -q "$GO_MIN_VERSION"; then
            log "Go $ver found"
            return
        fi
    fi

    info "Installing Go ${GO_VERSION}..."
    local go_tar="go${GO_VERSION}.${OS}-${ARCH}.tar.gz"
    wget -q "https://go.dev/dl/${go_tar}" -O "/tmp/${go_tar}" || err "Failed to download Go"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${go_tar}"
    rm -f "/tmp/${go_tar}"
    export PATH="/usr/local/go/bin:$PATH"
    log "Go ${GO_VERSION} installed"
}

# ── Build patched xray ───────────────────────────────────────────────
build_xray() {
    info "Cloning patched Xray-core..."
    rm -rf "$BUILD_DIR"
    git clone --depth 1 -b "$BRANCH" "$REPO" "$BUILD_DIR" 2>/dev/null || err "Failed to clone repo"

    info "Building xray-core (this may take a few minutes)..."
    cd "$BUILD_DIR"
    CGO_ENABLED=0 go build -o xray -trimpath -ldflags "-s -w" ./main || err "Build failed"
    log "Build successful"

    NEW_XRAY="${BUILD_DIR}/xray"
    chmod +x "$NEW_XRAY"
}

# ── Backup original binary ───────────────────────────────────────────
backup_xray() {
    if [[ -f "$XRAY_BIN" ]]; then
        local backup="${XRAY_BIN}.backup.$(date +%Y%m%d%H%M%S)"
        cp "$XRAY_BIN" "$backup"
        log "Backed up original: $backup"
    fi
}

# ── Replace binary ───────────────────────────────────────────────────
replace_xray() {
    mkdir -p "$(dirname "$XRAY_BIN")"
    cp "$NEW_XRAY" "$XRAY_BIN"
    chmod +x "$XRAY_BIN"
    log "Replaced xray binary at: $XRAY_BIN"
}

# ── Panel-specific restart functions ─────────────────────────────────
marzban_restart() {
    if command -v marzban &>/dev/null; then
        marzban restart
    elif [[ -f /opt/marzban/docker-compose.yml ]]; then
        cd /opt/marzban && docker compose restart
    else
        warn "Could not restart Marzban automatically"
        return
    fi
    log "Marzban restarted"
}

pasarguard_restart() {
    local compose_dir=""
    for d in /opt/pasarguard /opt/pg-node /root/pasarguard; do
        if [[ -f "$d/docker-compose.yml" ]]; then
            compose_dir="$d"
            break
        fi
    done

    if [[ -n "$compose_dir" ]]; then
        # Mount patched binary into container
        local compose_file="${compose_dir}/docker-compose.yml"
        if ! grep -q "$XRAY_BIN:/usr/local/bin/xray" "$compose_file" 2>/dev/null; then
            warn "Add this volume to your node service in ${compose_file}:"
            echo "      - ${XRAY_BIN}:/usr/local/bin/xray"
            echo ""
            info "Then run: cd ${compose_dir} && docker compose restart node"
        else
            cd "$compose_dir" && docker compose restart node
            log "PasarGuard node restarted"
        fi
    else
        warn "PasarGuard compose dir not found. Manually mount ${XRAY_BIN} into node container"
    fi
}

# ── Restart panel ────────────────────────────────────────────────────
restart_panel() {
    info "Restarting panel..."
    if [[ "$RESTART_CMD" == "marzban_restart" ]]; then
        marzban_restart
    elif [[ "$RESTART_CMD" == "pasarguard_restart" ]]; then
        pasarguard_restart
    else
        eval "$RESTART_CMD"
        log "Panel restarted"
    fi
}

# ── Verify ───────────────────────────────────────────────────────────
verify() {
    if [[ -x "$XRAY_BIN" ]]; then
        local ver
        ver=$("$XRAY_BIN" version 2>&1 | head -1 || true)
        log "Installed version: $ver"
    fi
}

# ── Cleanup ──────────────────────────────────────────────────────────
cleanup() {
    rm -rf "$BUILD_DIR"
    log "Cleaned up build files"
}

# ── Main ─────────────────────────────────────────────────────────────
main() {
    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║  Xray-core Patched Installer                       ║${NC}"
    echo -e "${CYAN}║  Fix: Kill connections on user removal              ║${NC}"
    echo -e "${CYAN}║  github.com/nightcrawler42/Xray-core               ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════╝${NC}"
    echo ""

    detect_arch
    detect_panel

    echo ""
    echo -e "Panel:    ${GREEN}${PANEL}${NC}"
    echo -e "Binary:   ${GREEN}${XRAY_BIN}${NC}"
    echo -e "Arch:     ${GREEN}${OS}/${ARCH}${NC}"
    echo ""
    read -rp "Continue? [y/N] " confirm
    [[ "$confirm" =~ ^[Yy]$ ]] || { warn "Aborted"; exit 0; }
    echo ""

    ensure_go
    build_xray
    backup_xray
    replace_xray
    verify

    echo ""
    read -rp "Restart panel now? [y/N] " restart_confirm
    if [[ "$restart_confirm" =~ ^[Yy]$ ]]; then
        restart_panel
    else
        warn "Remember to restart your panel to apply changes"
        info "Restart command: ${RESTART_CMD}"
    fi

    cleanup

    echo ""
    echo -e "${GREEN}╔══════════════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║  Installation complete!                             ║${NC}"
    echo -e "${GREEN}║  Users will now be disconnected immediately when    ║${NC}"
    echo -e "${GREEN}║  removed or disabled — no restart required.         ║${NC}"
    echo -e "${GREEN}╚══════════════════════════════════════════════════════╝${NC}"
    echo ""
}

main "$@"
