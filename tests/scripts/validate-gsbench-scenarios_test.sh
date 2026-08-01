#!/bin/sh

set -eu

test_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
repo_root=$(CDPATH='' cd -- "$test_dir/../.." && pwd -P)
harness=$repo_root/scripts/validate-gsbench-scenarios.sh
tmp_dir=${TMPDIR:-/tmp}/gsbench-harness-test-$$

cleanup_test() {
	rm -rf -- "$tmp_dir"
}
trap cleanup_test EXIT HUP INT TERM

mkdir -p -- "$tmp_dir"

expected='101
102
103
201
202
203
204
205
207
208
301
302
303
304
321
322
331
332
333
401
402
403
404
501
502
503
504
505
506
508
509
510
520
521
522
523
524
525
526
527
528
529
530
531
532
533
534
535
536
537
538
539
540
601
602
603
604
605
606
621
622
623
624
625
801'

actual=$($harness --list)
if [ "$actual" != "$expected" ]; then
	printf '%s\n' "unexpected scenario list" >&2
	printf '%s\n' "$expected" >"$tmp_dir/expected"
	printf '%s\n' "$actual" >"$tmp_dir/actual"
	diff -u "$tmp_dir/expected" "$tmp_dir/actual" 2>/dev/null || true
	exit 1
fi

count=$(printf '%s\n' "$actual" | awk 'NF { count++; seen[$1]++ } END {
	for (code in seen) if (seen[code] != 1) exit 2
	print count
}')
if [ "$count" -ne 65 ]; then
	printf 'scenario count=%s want=65\n' "$count" >&2
	exit 1
fi

valid_cfg=$tmp_dir/valid.cfg
invalid_cfg=$tmp_dir/invalid.cfg
duplicate_cfg=$tmp_dir/duplicate.cfg

printf '%s\n' '[data]' 'schema = gsbench_e2e_test' >"$valid_cfg"
printf '%s\n' '[data]' 'schema = gsbench' >"$invalid_cfg"
printf '%s\n' '[data]' 'schema = gsbench_e2e_test' 'schema = gsbench_e2e_other' >"$duplicate_cfg"

$harness --validate-config "$valid_cfg" >/dev/null
if $harness --validate-config "$invalid_cfg" >/dev/null 2>&1; then
	printf '%s\n' "unsafe schema was accepted" >&2
	exit 1
fi
if $harness --validate-config "$duplicate_cfg" >/dev/null 2>&1; then
	printf '%s\n' "duplicate schema was accepted" >&2
	exit 1
fi

printf '%s\n' "validate-gsbench-scenarios self-tests passed"
