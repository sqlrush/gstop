#!/bin/bash
# Install the complete gstop v1.6.3 single-architecture package for the current
# user. Validate the package before creating any links or modifying shell files.
set -euo pipefail

SOURCE="${BASH_SOURCE[0]}"
while [ -L "$SOURCE" ]; do
    DIR="$(cd -P "$(dirname "$SOURCE")" && pwd)"
    SOURCE="$(readlink "$SOURCE")"
    [[ "$SOURCE" != /* ]] && SOURCE="$DIR/$SOURCE"
done
SCRIPT_DIR="$(cd -P "$(dirname "$SOURCE")" && pwd)"
PACKAGE_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
GSTOP_BIN="$PACKAGE_ROOT/bin/gstop"
RUN_SCRIPT="$PACKAGE_ROOT/scripts/run.sh"
GSTOP_CONFIG="$PACKAGE_ROOT/configs/gstop.cfg"
VERSION_FILE="$PACKAGE_ROOT/VERSION"
CHECKSUM_FILE="$PACKAGE_ROOT/SHA256SUMS"

fail_layout() {
    echo "安装包版本/目录结构不匹配，请重新解压 v1.6.3 完整包。" >&2
    echo "包根目录: $PACKAGE_ROOT" >&2
    echo "问题: $1" >&2
    exit 1
}

require_file() {
    [[ -f "$1" ]] || fail_layout "缺少文件: $1"
}

require_executable() {
    [[ -x "$1" ]] || fail_layout "文件不存在或不可执行: $1"
}

require_executable "$GSTOP_BIN"
require_executable "$RUN_SCRIPT"
require_file "$GSTOP_CONFIG"
require_file "$VERSION_FILE"
require_file "$CHECKSUM_FILE"

if ! grep -Fqx "gstop v1.6.3" "$VERSION_FILE"; then
    fail_layout "VERSION 未声明 gstop v1.6.3: $VERSION_FILE"
fi

if command -v sha256sum >/dev/null 2>&1; then
    if ! (cd "$PACKAGE_ROOT" && sha256sum -c SHA256SUMS); then
        fail_layout "SHA-256 校验失败: $CHECKSUM_FILE"
    fi
elif command -v shasum >/dev/null 2>&1; then
    if ! (cd "$PACKAGE_ROOT" && shasum -a 256 -c SHA256SUMS); then
        fail_layout "SHA-256 校验失败: $CHECKSUM_FILE"
    fi
else
    echo "Warning: sha256sum/shasum unavailable; package checksum skipped." >&2
fi

commands=("pidstat" "iostat" "nproc" "uptime" "lsblk")
for cmd in "${commands[@]}"; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Command '$cmd' not found. Install sysstat and util-linux, then retry." >&2
        exit 1
    fi
done

TARGET_DIR="$HOME/.local/bin"
mkdir -p "$TARGET_DIR"
ln -sf "$RUN_SCRIPT" "$TARGET_DIR/gstop"
chmod +x "$RUN_SCRIPT" "$GSTOP_BIN"
if [[ -f "$SCRIPT_DIR/gstop-manage.sh" ]]; then
    chmod +x "$SCRIPT_DIR/gstop-manage.sh"
fi

if ! grep -q "$TARGET_DIR" "$HOME/.bashrc" 2>/dev/null; then
    echo "export PATH=\$PATH:$TARGET_DIR" >> "$HOME/.bashrc"
fi
export PATH="$PATH:$TARGET_DIR"

echo "Install finished. Start with: gstop"
