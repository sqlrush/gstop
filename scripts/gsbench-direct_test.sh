#!/bin/sh
set -eu

workspace_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
launcher="$workspace_dir/gstop-og/gsbench-direct"
fixture_dir=$(mktemp -d "${TMPDIR:-/tmp}/gsbench-direct-test.XXXXXX")
trap 'rm -rf "$fixture_dir"' EXIT HUP INT TERM

mkdir -p "$fixture_dir/configs"
printf '%s\n' \
  '[main]' \
  'db_password = "fixture-secret"' \
  > "$fixture_dir/configs/gstop.cfg"
printf '%s\n' '[database]' > "$fixture_dir/configs/gsbench.cfg"

# The next single-quoted line is the literal body of the fake executable.
# shellcheck disable=SC2016
printf '%s\n' \
  '#!/bin/sh' \
  'printf "password=%s\\n" "$GSBENCH_PASSWORD"' \
  'printf "args=%s\\n" "$*"' \
  > "$fixture_dir/gsbench"
chmod +x "$fixture_dir/gsbench"

output=$(
  GSTOP_CONFIG="$fixture_dir/configs/gstop.cfg" \
  GSBENCH_CONFIG="$fixture_dir/configs/gsbench.cfg" \
  GSBENCH_BIN="$fixture_dir/gsbench" \
  "$launcher" doctor
)

expected=$(printf '%s\n' \
  'password=fixture-secret' \
  "args=doctor -c $fixture_dir/configs/gsbench.cfg")

if [ "$output" != "$expected" ]; then
  printf 'unexpected launcher output:\n%s\n' "$output" >&2
  exit 1
fi

printf 'PASS: gsbench-direct loads the existing gstop password and forwards arguments\n'
