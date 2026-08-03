#!/bin/bash
# Launch gstop from a validated single-architecture v1.6.1 package. Resolve
# symlinks first so ~/.local/bin/gstop still finds the package root.
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
GSTOP_CONFIG="$PACKAGE_ROOT/configs/gstop.cfg"

if [[ ! -x "$GSTOP_BIN" ]]; then
    echo "安装包版本/目录结构不匹配: 文件不存在或不可执行: $GSTOP_BIN" >&2
    exit 1
fi
if [[ ! -f "$GSTOP_CONFIG" ]]; then
    echo "安装包版本/目录结构不匹配: 缺少配置文件: $GSTOP_CONFIG" >&2
    exit 1
fi

cd "$PACKAGE_ROOT"
exec "$GSTOP_BIN" "$@"
