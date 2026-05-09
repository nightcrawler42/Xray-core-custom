# Xray-core Enhanced

A custom distribution of Xray-core with improved user management, connection handling, and stability fixes.

## Quick Install

Works with **Marzban**, **Marzban Node**, **3x-ui**, **x-ui (alireza)**, **PasarGuard**, **PasarGuard Node**, and standalone xray.

```bash
sudo bash -c "$(curl -sL https://raw.githubusercontent.com/nightcrawler42/Xray-core-custom/main/install.sh)"
```

The script auto-detects your panel, downloads a pre-built binary, backs up the original, and replaces it. Supports `amd64`, `arm64`, and `armv7`.

## Features

- Immediate connection termination on user removal across all protocols (VLESS, VMess, Trojan, Shadowsocks, Shadowsocks 2022)
- Per-user connection tracking with context-based cancellation
- Improved error handling in Shadowsocks 2022 multi-user mode
- Race condition fixes in user management

## Restore original

The installer backs up your original binary with a timestamp. To revert:

```bash
cp /path/to/xray.backup.TIMESTAMP /path/to/xray
systemctl restart x-ui  # or: marzban restart
```

## Build from source

```bash
CGO_ENABLED=0 go build -o xray -trimpath -ldflags="-s -w" ./main
```

## License

[Mozilla Public License Version 2.0](LICENSE)

Based on [XTLS/Xray-core](https://github.com/XTLS/Xray-core).
