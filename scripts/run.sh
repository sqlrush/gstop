#!/bin/bash
# Launch gstop from its install directory, resolving symlinks so the binary finds
# its adjacent configs. Unlike the Python tool, no LD_LIBRARY_PATH is required —
# the binary is statically linked and the driver is pure Go.
SOURCE="${BASH_SOURCE[0]}"
while [ -L "$SOURCE" ]; do
    DIR="$(cd -P "$(dirname "$SOURCE")" && pwd)"
    SOURCE="$(readlink "$SOURCE")"
    [[ "$SOURCE" != /* ]] && SOURCE="$DIR/$SOURCE"
done
SCRIPT_DIR="$(cd -P "$(dirname "$SOURCE")" && pwd)"
cd "$SCRIPT_DIR"

exec ./gstop "$@"
