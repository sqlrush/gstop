#!/bin/bash
# Cross-compile gstop for the target Linux platforms and assemble a distributable
# package per architecture. The result is a self-contained tarball (static binary
# + configs + install/run/manage scripts) — no Python, rpm, or libpq needed, since
# the openGauss Go driver is pure Go.
set -euo pipefail

VERSION="${1:-v1.4.0}"
DATE="$(date +%Y%m%d)"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

rm -rf dist
for arch in arm64 amd64; do
    out="dist/gstop_${arch}"
    mkdir -p "$out"
    echo "Building linux/${arch} ..."
    GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w" -o "$out/gstop" ./cmd/gstop
    cp -r configs "$out/"
    cp scripts/run.sh scripts/install.sh scripts/gstop-manage.sh "$out/"
    chmod +x "$out/gstop" "$out/run.sh" "$out/install.sh" "$out/gstop-manage.sh"
    (cd dist && tar czf "gstop_${VERSION}_linux_${arch}_${DATE}.tar.gz" "gstop_${arch}")
    echo "  -> dist/gstop_${VERSION}_linux_${arch}_${DATE}.tar.gz"
done
echo "Done."
