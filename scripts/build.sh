#!/bin/bash
# Cross-compile gstop and gsbench for the target Linux platforms and assemble a distributable
# package per architecture. The result is a self-contained tarball (static binary
# + configs + install/run/manage scripts) — no Python, rpm, or libpq needed, since
# the openGauss Go driver is pure Go.
set -euo pipefail

VERSION="${1:-v1.6.3}"
DATE="$(date +%Y%m%d)"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

stage="$(mktemp -d "$ROOT/.gstop-build.XXXXXX")"
cleanup() {
    rm -rf -- "$stage"
}
trap cleanup EXIT

mkdir -p dist
for arch in arm64 amd64; do
    package_name="gstop_${arch}"
    out="$stage/$package_name"
    mkdir -p "$out/bin" "$out/configs" "$out/docs/gsbench" "$out/scripts"
    echo "Building linux/${arch} ..."
    GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w" -o "$out/bin/gstop" ./cmd/gstop
    GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w" -o "$out/bin/gsbench" ./cmd/gsbench
    cp -R configs/. "$out/configs/"
    cp README.md "$out/docs/README.md"
    cp docs/gsbench/README.md "$out/docs/gsbench/README.md"
    cp scripts/run.sh scripts/install.sh scripts/gstop-manage.sh "$out/scripts/"
    printf 'gstop %s\ngsbench v1.1.7\n' "$VERSION" > "$out/VERSION"
    chmod +x "$out/bin/gstop" "$out/bin/gsbench" \
        "$out/scripts/run.sh" "$out/scripts/install.sh" "$out/scripts/gstop-manage.sh"
    (
        cd "$out"
        find . -type f ! -name SHA256SUMS -print | LC_ALL=C sort |
            while IFS= read -r file; do
                shasum -a 256 "$file"
            done > SHA256SUMS
        shasum -a 256 -c SHA256SUMS
    )

    archive="$stage/gstop_${VERSION}_linux_${arch}_${DATE}.tar.gz"
    COPYFILE_DISABLE=1 tar --no-xattrs -C "$stage" -czf "$archive" "$package_name"

    final_package="dist/$package_name"
    final_archive="dist/$(basename "$archive")"
    if [[ -e "$final_package" ]]; then
        mv "$final_package" "$stage/previous_${package_name}"
    fi
    if [[ -e "$final_archive" ]]; then
        mv "$final_archive" "$stage/previous_$(basename "$archive")"
    fi
    mv "$out" "$final_package"
    mv "$archive" "$final_archive"
    echo "  -> dist/gstop_${VERSION}_linux_${arch}_${DATE}.tar.gz"
done
echo "Done."
