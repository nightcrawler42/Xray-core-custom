#!/usr/bin/env bash
set -euo pipefail

# ──────────────────────────────────────────────────────────────────────
# Xray-core Enhanced Installer
# Replaces official xray-core with enhanced version that kills active
# connections on user removal (no restart needed to disconnect users).
#
# Supported panels: Marzban, Marzban Node, 3x-ui, x-ui (alireza), PasarGuard, PasarGuard Node
#
# Usage:
#   sudo bash -c "$(curl -sL https://raw.githubusercontent.com/nightcrawler42/Xray-core-custom/main/install.sh)"
# ──────────────────────────────────────────────────────────────────────

REPO="nightcrawler42/Xray-core-custom"
API_URL="https://api.github.com/repos/${REPO}/releases/latest"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${GREEN}[✓]${NC} $1"; }
warn() { echo -e "${YELLOW}[!]${NC} $1"; }
err()  { echo -e "${RED}[✗]${NC} $1"; exit 1; }
info() { echo -e "${CYAN}[i]${NC} $1"; }

# ── Root check (auto-elevate) ─────────────────────────────────────────
if [[ $EUID -ne 0 ]]; then
    if [[ -f "$0" ]]; then
        exec sudo bash "$0" "$@"
    else
        err "Run as root: sudo bash <(curl -sL ...) or curl ... | sudo bash"
    fi
fi

# ── Detect architecture ──────────────────────────────────────────────
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  ARCH="amd64"; ASSET_NAME="linux-64" ;;
        aarch64|arm64) ARCH="arm64"; ASSET_NAME="linux-arm64-v8a" ;;
        armv7l)        ARCH="arm";   ASSET_NAME="linux-arm32-v7a" ;;
        *)             err "Unsupported architecture: $(uname -m)" ;;
    esac
    log "Architecture: ${ARCH} (asset: Xray-${ASSET_NAME}.zip)"
}

# ── Detect panel ─────────────────────────────────────────────────────
detect_panel() {
    PANEL=""
    XRAY_BIN=""
    RESTART_CMD=""

    # Marzban Node (Docker) — check before main Marzban
    if [[ -f /opt/marzban-node/docker-compose.yml ]] || docker ps 2>/dev/null | grep -q marzban-node; then
        PANEL="marzban-node"
        if [[ -f /opt/marzban-node/.env ]]; then
            XRAY_BIN=$(grep -oP 'XRAY_EXECUTABLE_PATH\s*=\s*"\K[^"]+' /opt/marzban-node/.env 2>/dev/null || true)
        fi
        [[ -z "$XRAY_BIN" ]] && XRAY_BIN="/var/lib/marzban-node/xray-core/xray"
        RESTART_CMD="marzban_node_restart"
        log "Detected: Marzban Node"
        log "Xray binary: $XRAY_BIN"
        return
    fi

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

    # PasarGuard Node
    if docker ps 2>/dev/null | grep -q pg-node || [[ -f /opt/pg-node/docker-compose.yml ]]; then
        PANEL="pasarguard-node"
        XRAY_BIN="/var/lib/pg-node/xray"
        RESTART_CMD="pasarguard_restart"
        log "Detected: PasarGuard Node"
        log "Xray binary: $XRAY_BIN"
        return
    fi

    # PasarGuard (main panel or generic)
    if docker ps 2>/dev/null | grep -q pasarguard || [[ -f /opt/pasarguard/docker-compose.yml ]]; then
        PANEL="pasarguard"
        XRAY_BIN="/var/lib/pasarguard/xray"
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

# ── Download pre-built binary ───────────────────────────────────────
download_xray() {
    info "Fetching latest release info..."
    RELEASE_JSON=$(curl -sL "$API_URL") || err "Failed to fetch release info"

    TAG=$(echo "$RELEASE_JSON" | grep -oP '"tag_name"\s*:\s*"\K[^"]+') || err "Failed to parse release tag"
    log "Latest release: $TAG"

    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TAG}/Xray-${ASSET_NAME}.zip"
    TMPDIR=$(mktemp -d)
    ZIPFILE="${TMPDIR}/xray.zip"

    info "Downloading Xray-${ASSET_NAME}.zip..."
    curl -sL "$DOWNLOAD_URL" -o "$ZIPFILE" || err "Failed to download release binary"

    if command -v file &>/dev/null && ! file "$ZIPFILE" | grep -q "Zip archive"; then
        err "Downloaded file is not a valid ZIP archive. Release may not have binaries yet."
    fi

    if ! command -v unzip &>/dev/null; then
        info "Installing unzip..."
        if command -v apt-get &>/dev/null; then
            apt-get update -qq >/dev/null 2>&1
            apt-get install -y unzip || err "Failed to install unzip via apt"
        elif command -v yum &>/dev/null; then
            yum install -y unzip || err "Failed to install unzip via yum"
        elif command -v apk &>/dev/null; then
            apk add unzip || err "Failed to install unzip via apk"
        else
            err "unzip not found. Install it manually: apt install unzip"
        fi
    fi

    info "Extracting..."
    unzip -qo "$ZIPFILE" -d "$TMPDIR" || err "Failed to extract archive"

    NEW_XRAY="${TMPDIR}/xray"
    [[ -f "$NEW_XRAY" ]] || err "xray binary not found in archive"
    chmod +x "$NEW_XRAY"
    log "Download and extraction complete"
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

# ── Ensure Marzban uses our binary path ──────────────────────────────
ensure_marzban_env() {
    local panel_dir="$1"
    local env_file="${panel_dir}/.env"
    local compose_file="${panel_dir}/docker-compose.yml"

    # Try .env file first
    if [[ -f "$env_file" ]]; then
        if grep -q 'XRAY_EXECUTABLE_PATH' "$env_file"; then
            sed -i "s|^XRAY_EXECUTABLE_PATH=.*|XRAY_EXECUTABLE_PATH=\"$XRAY_BIN\"|" "$env_file"
            log "Updated XRAY_EXECUTABLE_PATH in $env_file"
        else
            echo "XRAY_EXECUTABLE_PATH=\"$XRAY_BIN\"" >> "$env_file"
            log "Added XRAY_EXECUTABLE_PATH to $env_file"
        fi
        NEED_RECREATE=true
        return
    fi

    # No .env — try adding to docker-compose.yml environment section
    if [[ -f "$compose_file" ]]; then
        if grep -q 'XRAY_EXECUTABLE_PATH' "$compose_file"; then
            sed -i "s|XRAY_EXECUTABLE_PATH:.*|XRAY_EXECUTABLE_PATH: \"$XRAY_BIN\"|" "$compose_file"
            log "Updated XRAY_EXECUTABLE_PATH in $compose_file"
        else
            # Add after existing environment entries
            sed -i "/environment:/a\\      XRAY_EXECUTABLE_PATH: \"$XRAY_BIN\"" "$compose_file"
            log "Added XRAY_EXECUTABLE_PATH to $compose_file"
        fi
        NEED_RECREATE=true
        return
    fi

    warn "Could not find .env or docker-compose.yml in $panel_dir"
}

# ── Panel-specific restart functions ─────────────────────────────────
marzban_restart() {
    if [[ "${NEED_RECREATE:-}" == "true" ]] && [[ -f /opt/marzban/docker-compose.yml ]]; then
        info "Env changed, recreating Marzban container..."
        cd /opt/marzban && docker compose up -d --force-recreate marzban
    elif command -v marzban &>/dev/null; then
        marzban restart
    elif [[ -f /opt/marzban/docker-compose.yml ]]; then
        cd /opt/marzban && docker compose restart
    else
        warn "Could not restart Marzban automatically"
        return
    fi
    log "Marzban restarted"
}

marzban_node_restart() {
    if [[ "${NEED_RECREATE:-}" == "true" ]] && [[ -f /opt/marzban-node/docker-compose.yml ]]; then
        info "Env changed, recreating Marzban Node container..."
        cd /opt/marzban-node && docker compose up -d --force-recreate
    elif command -v marzban-node &>/dev/null; then
        marzban-node restart
    elif [[ -f /opt/marzban-node/docker-compose.yml ]]; then
        cd /opt/marzban-node && docker compose restart
    else
        warn "Could not restart Marzban Node automatically"
        return
    fi
    log "Marzban Node restarted"
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
    elif [[ "$RESTART_CMD" == "marzban_node_restart" ]]; then
        marzban_node_restart
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
    [[ -n "${TMPDIR:-}" ]] && rm -rf "$TMPDIR"
    log "Cleaned up temporary files"
}

# ── Main ─────────────────────────────────────────────────────────────
main() {
    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║  Xray-core Enhanced Installer                      ║${NC}"
    echo -e "${CYAN}║  github.com/nightcrawler42/Xray-core-custom               ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════╝${NC}"
    echo ""

    detect_arch
    detect_panel

    echo ""
    echo -e "Panel:    ${GREEN}${PANEL}${NC}"
    echo -e "Binary:   ${GREEN}${XRAY_BIN}${NC}"
    echo -e "Arch:     ${GREEN}${ARCH}${NC}"
    echo ""
    if [[ -t 0 ]]; then
        read -rp "Continue? [y/N] " confirm
        [[ "$confirm" =~ ^[Yy]$ ]] || { warn "Aborted"; exit 0; }
    else
        info "Non-interactive mode, proceeding automatically"
    fi
    echo ""

    NEED_RECREATE=false
    download_xray
    backup_xray
    replace_xray

    # Ensure Marzban/Marzban-node config points to our binary
    if [[ "$PANEL" == "marzban" ]]; then
        ensure_marzban_env "/opt/marzban"
    elif [[ "$PANEL" == "marzban-node" ]]; then
        ensure_marzban_env "/opt/marzban-node"
    fi

    verify

    echo ""
    if [[ -t 0 ]]; then
        read -rp "Restart panel now? [y/N] " restart_confirm
        if [[ "$restart_confirm" =~ ^[Yy]$ ]]; then
            restart_panel
        else
            warn "Remember to restart your panel to apply changes"
            info "Restart command: ${RESTART_CMD}"
        fi
    else
        restart_panel
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
