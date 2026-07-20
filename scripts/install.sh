#!/bin/bash
# Install gstop for the current user: verify the OS command dependencies the OS
# monitor relies on, then expose a `gstop` launcher on PATH. The Go build is a
# single static binary, so there are no rpm or Python package steps.
set -euo pipefail

commands=("pidstat" "iostat" "nproc" "uptime" "lsblk")
for cmd in "${commands[@]}"; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Command '$cmd' not found. Install sysstat and util-linux, then retry."
        exit 1
    fi
done

INSTALL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ ! -x "$INSTALL_DIR/gstop" ]]; then
    echo "gstop binary not found next to this script ($INSTALL_DIR)."
    exit 1
fi

TARGET_DIR="$HOME/.local/bin"
mkdir -p "$TARGET_DIR"
ln -sf "$INSTALL_DIR/run.sh" "$TARGET_DIR/gstop"
chmod +x "$INSTALL_DIR/run.sh" "$INSTALL_DIR/gstop" "$INSTALL_DIR/gstop-manage.sh"

if ! grep -q "$TARGET_DIR" "$HOME/.bashrc" 2>/dev/null; then
    echo "export PATH=\$PATH:$TARGET_DIR" >> "$HOME/.bashrc"
fi
export PATH="$PATH:$TARGET_DIR"

echo "Install finished. Start with: gstop"
