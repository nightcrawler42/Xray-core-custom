# Xray-core (Patched) — Instant User Disconnect on Removal

> **This fork fixes a critical issue:** when a user is removed or disabled via the panel API, their active connections now terminate **immediately** — no xray restart required. The official xray-core only removes users from the auth table but lets existing connections run indefinitely until they naturally close or the entire service is restarted.

## Quick Install

Works with **Marzban**, **3x-ui**, **x-ui (alireza)**, **PasarGuard**, and standalone xray.

```bash
bash <(curl -sL https://raw.githubusercontent.com/nightcrawler42/Xray-core/main/install.sh)
```

The script auto-detects your panel, builds the patched xray from source, backs up the original binary, and replaces it. Supports `amd64`, `arm64`, and `armv7`.

### What changed

- Added per-user connection tracking to all protocols (VLESS, VMess, Trojan, Shadowsocks, Shadowsocks 2022)
- `RemoveUser` now cancels all active connections for that user via context propagation
- Fixed Shadowsocks 2022 crash when user removed during active handshake
- Fixed silent error swallowing in Shadowsocks 2022 user sync

### Restore original

The installer backs up your original binary with a timestamp. To revert:

```bash
# Find your backup
ls /var/lib/marzban/xray-core/xray.backup.*   # Marzban
ls /usr/local/x-ui/bin/xray-linux-amd64.backup.*  # 3x-ui / x-ui

# Restore
cp /path/to/xray.backup.TIMESTAMP /path/to/xray
systemctl restart x-ui  # or: marzban restart
```

---

## Based on

This is a fork of [XTLS/Xray-core](https://github.com/XTLS/Xray-core). Licensed under [Mozilla Public License Version 2.0](https://github.com/XTLS/Xray-core/blob/main/LICENSE).

## Build from source

```bash
CGO_ENABLED=0 go build -o xray -trimpath -ldflags="-s -w" ./main
```
