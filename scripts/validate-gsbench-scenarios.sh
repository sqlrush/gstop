#!/bin/sh

set -u

scenario_codes='101 102 103 201 202 203 204 205 207 208 301 302 303 304 321 322 331 332 333 401 402 403 404 501 502 503 504 505 506 508 509 510 520 521 522 523 524 525 526 527 528 529 530 531 532 533 534 535 536 537 538 539 540 601 602 603 604 605 606 621 622 623 624 625 801'
required_schema=gsbench_e2e_20260801
plan_workers=10
plan_ready_timeout=300

fail() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage:
  validate-gsbench-scenarios.sh --list
  validate-gsbench-scenarios.sh --validate-config FILE
  validate-gsbench-scenarios.sh --gsbench FILE --gstop FILE \
    --config FILE --gstop-config FILE --artifacts DIR

Live mode requires GSBENCH_E2E_SCHEMA=gsbench_e2e_20260801 and a dedicated,
previously unused schema with data.reuse_existing=false.
EOF
}

print_codes() {
	for code in $scenario_codes; do
		printf '%s\n' "$code"
	done
}

ini_value() {
	file=$1
	section_wanted=$2
	key_wanted=$3
	awk -v section_wanted="$section_wanted" -v key_wanted="$key_wanted" '
		function trim(value) {
			sub(/^[[:space:]]+/, "", value)
			sub(/[[:space:]]+$/, "", value)
			return value
		}
		{
			line=$0
			sub(/\r$/, "", line)
			if (line ~ /^[[:space:]]*[#;]/ || line ~ /^[[:space:]]*$/) next
			if (line ~ /^[[:space:]]*\[[^]]+\][[:space:]]*$/) {
				section=line
				sub(/^[[:space:]]*\[/, "", section)
				sub(/\][[:space:]]*$/, "", section)
				section=trim(section)
				next
			}
			equals=index(line, "=")
			if (equals == 0) next
			key=trim(substr(line, 1, equals-1))
			value=trim(substr(line, equals+1))
			if (section == section_wanted && key == key_wanted) {
				count++
				found=value
			}
		}
		END {
			if (count != 1) exit 3
			print found
		}
	' "$file"
}

config_schema() {
	config_file=$1
	[ -f "$config_file" ] || fail "config does not exist: $config_file"
	if ! schema_value=$(ini_value "$config_file" data schema); then
		fail "config must contain exactly one [data] schema value: $config_file"
	fi
	case "$schema_value" in
		gsbench_e2e_*) printf '%s\n' "$schema_value" ;;
		*) fail "unsafe validation schema '$schema_value'; expected gsbench_e2e_*" ;;
	esac
}

absolute_existing_file() {
	input=$1
	[ -f "$input" ] || fail "file does not exist: $input"
	directory=$(dirname -- "$input")
	base=$(basename -- "$input")
	directory=$(CDPATH='' cd -- "$directory" 2>/dev/null && pwd -P) ||
		fail "cannot resolve directory for $input"
	printf '%s/%s\n' "$directory" "$base"
}

resolve_executable() {
	input=$1
	case "$input" in
		*/*) candidate=$input ;;
		*) candidate=$(command -v -- "$input" 2>/dev/null || true) ;;
	esac
	[ -n "$candidate" ] && [ -x "$candidate" ] ||
		fail "executable is unavailable: $input"
	directory=$(dirname -- "$candidate")
	base=$(basename -- "$candidate")
	directory=$(CDPATH='' cd -- "$directory" 2>/dev/null && pwd -P) ||
		fail "cannot resolve executable directory for $input"
	printf '%s/%s\n' "$directory" "$base"
}

safe_field() {
	printf '%s' "$1" | tr '\t\r\n' '   '
}

duration_for() {
	case "$1" in
		101|102|103|401|402|404) printf '%s\n' 60s ;;
		601|602|603|604|605|606|801) printf '%s\n' 20s ;;
		*) printf '%s\n' 10s ;;
	esac
}

plan_name_for() {
	case "$1" in
		601) printf '%s\n' planchange_stats_target ;;
		602) printf '%s\n' planchange_index_unusable ;;
		603) printf '%s\n' planchange_stats_ndistinct ;;
		604) printf '%s\n' planchange_stats_extended ;;
		605) printf '%s\n' planchange_index_drop ;;
		606) printf '%s\n' planchange_index_shape ;;
		*) return 1 ;;
	esac
}

wait_for_plan_baseline() {
	code=$1
	init_log=$2
	init_pid=$3
	status_log=$4
	attempt=0
	while [ "$attempt" -lt "$plan_ready_timeout" ]; do
		workload_run_id=$(sed -n \
			's/.*workload_run_id=\([^ ]*\).*/\1/p' "$init_log" | tail -n 1)
		if [ -n "$workload_run_id" ] &&
			"$gsbench_bin" status --config "$config_file" \
				--run-id "$workload_run_id" >"$status_log" 2>&1 &&
			grep -Fq \
				"run_id=$workload_run_id scenarios=$code phase=plan_baseline status=workload_running" \
				"$status_log"; then
			printf '%s\n' "$workload_run_id"
			return 0
		fi
		kill -0 "$init_pid" 2>/dev/null || return 1
		sleep 1
		attempt=$((attempt + 1))
	done
	return 1
}

gsbench_arg=
gstop_arg=
config_arg=
gstop_config_arg=
artifacts_arg=

if [ "${1-}" = "--list" ]; then
	[ "$#" -eq 1 ] || fail "--list takes no other arguments"
	print_codes
	exit 0
fi
if [ "${1-}" = "--validate-config" ]; then
	[ "$#" -eq 2 ] || fail "--validate-config requires one file"
	config_schema "$2"
	exit 0
fi

while [ "$#" -gt 0 ]; do
	case "$1" in
		--gsbench)
			[ "$#" -ge 2 ] || fail "--gsbench requires a value"
			gsbench_arg=$2
			shift 2
			;;
		--gstop)
			[ "$#" -ge 2 ] || fail "--gstop requires a value"
			gstop_arg=$2
			shift 2
			;;
		--config)
			[ "$#" -ge 2 ] || fail "--config requires a value"
			config_arg=$2
			shift 2
			;;
		--gstop-config)
			[ "$#" -ge 2 ] || fail "--gstop-config requires a value"
			gstop_config_arg=$2
			shift 2
			;;
		--artifacts)
			[ "$#" -ge 2 ] || fail "--artifacts requires a value"
			artifacts_arg=$2
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*) fail "unknown argument: $1" ;;
	esac
done

[ -n "$gsbench_arg" ] || fail "--gsbench is required"
[ -n "$gstop_arg" ] || fail "--gstop is required"
[ -n "$config_arg" ] || fail "--config is required"
[ -n "$gstop_config_arg" ] || fail "--gstop-config is required"
[ -n "$artifacts_arg" ] || fail "--artifacts is required"
command -v jq >/dev/null 2>&1 || fail "jq is required in live mode"

gsbench_bin=$(resolve_executable "$gsbench_arg")
gstop_bin=$(resolve_executable "$gstop_arg")
config_file=$(absolute_existing_file "$config_arg")
gstop_config=$(absolute_existing_file "$gstop_config_arg")
schema=$(config_schema "$config_file")

[ "$schema" = "$required_schema" ] ||
	fail "live test config schema must be exactly $required_schema"
[ "${GSBENCH_E2E_SCHEMA-}" = "$required_schema" ] ||
	fail "set GSBENCH_E2E_SCHEMA=$required_schema to acknowledge live initialization"

if ! reuse_existing=$(ini_value "$config_file" data reuse_existing); then
	fail "config must set [data] reuse_existing=false"
fi
if ! validation_enabled=$(ini_value "$config_file" run validation_enabled); then
	fail "config must set [run] validation_enabled=true for the live matrix"
fi
if ! dry_run=$(ini_value "$config_file" run dry_run); then
	fail "config must set [run] dry_run=false"
fi
if ! profile_cap=$(ini_value "$config_file" safety profile_cap_gb); then
	fail "config must set [safety] profile_cap_gb"
fi
[ "$reuse_existing" = false ] || fail "data.reuse_existing must be false"
[ "$validation_enabled" = true ] || fail "run.validation_enabled must be true"
[ "$dry_run" = false ] || fail "run.dry_run must be false"
case "$profile_cap" in
	*[!0-9]*|'') fail "safety.profile_cap_gb must be an integer >=20" ;;
esac
[ "$profile_cap" -ge 20 ] || fail "safety.profile_cap_gb must be >=20"

database_host=$(ini_value "$config_file" database host 2>/dev/null || printf '%s' unknown)
database_name=$(ini_value "$config_file" database database 2>/dev/null || printf '%s' unknown)

umask 077
case "$artifacts_arg" in
	/*) artifacts=$artifacts_arg ;;
	*) artifacts=$(pwd -P)/$artifacts_arg ;;
esac
mkdir -p -- "$artifacts/scenarios" "$artifacts/status" "$artifacts/restore" ||
	fail "cannot create artifact directory: $artifacts"
chmod 700 "$artifacts" 2>/dev/null || true
cd -- "$artifacts" || fail "cannot enter artifact directory"

printf 'database=%s host=%s schema=%s config=%s\n' \
	"$database_name" "$database_host" "$schema" "$config_file"
printf 'code\tname\ttarget\tobserved\tceiling\toperations\terrors\toutcome\tapplicability\tevidence_path\n' >results.tsv
printf 'code\trun_id\texit_code\tduration\toutcome\trestore_state\tstatus_path\n' >runs.tsv

gstop_pid=
current_run_id=
current_plan_code=
plan_init_pid=

stop_gstop() {
	if [ -n "$gstop_pid" ] && kill -0 "$gstop_pid" 2>/dev/null; then
		kill "$gstop_pid" 2>/dev/null || true
		wait "$gstop_pid" 2>/dev/null || true
	fi
	gstop_pid=
}

stop_plan_init() {
	if [ -n "$plan_init_pid" ]; then
		if kill -0 "$plan_init_pid" 2>/dev/null; then
			kill "$plan_init_pid" 2>/dev/null || true
		fi
		wait "$plan_init_pid" 2>/dev/null || true
	fi
	plan_init_pid=
}

recover_current_plan() {
	[ -n "$current_plan_code" ] || return 0
	"$gsbench_bin" run "$current_plan_code" recover --config "$config_file" \
		>"restore/$current_plan_code-exit.log" 2>&1 || true
}

on_exit() {
	stop_plan_init
	recover_current_plan
	stop_gstop
}

on_signal() {
	signal_name=$1
	if [ -n "$current_plan_code" ]; then
		stop_plan_init
		recover_current_plan
		current_plan_code=
	elif [ -n "$current_run_id" ] && [ -n "$gsbench_bin" ]; then
		"$gsbench_bin" restore --config "$config_file" --run-id "$current_run_id" \
			>"restore/interrupted-$current_run_id.log" 2>&1 || true
	fi
	stop_gstop
	printf 'interrupted by %s; dataset was not cleaned up\n' "$signal_name" >&2
	exit 130
}

trap on_exit EXIT
trap 'on_signal HUP' HUP
trap 'on_signal INT' INT
trap 'on_signal TERM' TERM

"$gsbench_bin" doctor --config "$config_file" >doctor.log 2>&1 ||
	fail "doctor failed; see $artifacts/doctor.log"

"$gsbench_bin" init --config "$config_file" --size 20GB >init.log 2>&1 ||
	fail "20GB initialization failed; see $artifacts/init.log"
grep -q 'target_bytes=21474836480' init.log ||
	fail "init log does not confirm the 20GB target"
final_size=$(sed -n 's/.*final_size_bytes=\([0-9][0-9]*\).*/\1/p' init.log | tail -n 1)
[ -n "$final_size" ] || fail "init log has no final physical size"
min_size=20401094656
max_size=22548578304
[ "$final_size" -ge "$min_size" ] && [ "$final_size" -le "$max_size" ] ||
	fail "initialized physical size $final_size is outside 19-21GiB"

"$gstop_bin" -d -c "$gstop_config" -i 1 -l 1 >gstop.log 2>&1 &
gstop_pid=$!
sleep 1
kill -0 "$gstop_pid" 2>/dev/null || fail "gstop exited before scenario execution"

for code in $scenario_codes; do
	kill -0 "$gstop_pid" 2>/dev/null || fail "gstop exited before scenario $code"
	duration=$(duration_for "$code")
	run_log=$artifacts/scenarios/$code.log
	evidence_lines=$artifacts/scenarios/$code.evidence.jsonl
	evidence_file=$artifacts/scenarios/$code.json
	started_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
	current_run_id=
	case "$code" in
		601|602|603|604|605|606)
			current_plan_code=$code
			init_log=$run_log
			fault_log=$artifacts/scenarios/$code-fault.log
			recover_log=$artifacts/scenarios/$code-recover.log
			ready_status=$artifacts/status/$code-baseline.log
			"$gsbench_bin" run "$code" init --worker "$plan_workers" \
				--duration "$duration" --config "$config_file" \
				>"$init_log" 2>&1 &
			plan_init_pid=$!
			if ! current_run_id=$(wait_for_plan_baseline \
				"$code" "$init_log" "$plan_init_pid" "$ready_status"); then
				fail "scenario $code init did not reach plan_baseline; see $init_log"
			fi
			if ! "$gsbench_bin" run "$code" fault --config "$config_file" \
				>"$fault_log" 2>&1; then
				fail "scenario $code fault failed; see $fault_log"
			fi
			if ! "$gsbench_bin" run "$code" recover --config "$config_file" \
				>"$recover_log" 2>&1; then
				fail "scenario $code recover failed; see $recover_log"
			fi
			if wait "$plan_init_pid"; then
				run_rc=0
			else
				run_rc=$?
			fi
			plan_init_pid=
			[ "$run_rc" -eq 0 ] ||
				fail "scenario $code init failed with exit code $run_rc; see $init_log"
			status_path=$artifacts/status/$code.log
			"$gsbench_bin" status --config "$config_file" \
				>"$status_path" 2>&1 || fail "status failed after scenario $code"
			operations=$(sed -n \
				's/.*plan init SUCCESS.*operations=\([0-9][0-9]*\).*/\1/p' \
				"$init_log" | tail -n 1)
			[ -n "$operations" ] || operations=0
			name=$(plan_name_for "$code")
			printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
				"$code" "$name" "workers=$plan_workers" \
				'phases=plan_baseline,hold,restore' '' \
				"$operations" 0 SUCCESS APPLICABLE "$init_log" >>results.tsv
			printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
				"$code" "$current_run_id" "$run_rc" "$duration" SUCCESS \
				PLAN_RECOVERED "$status_path" >>runs.tsv
			printf 'scenario=%s name=%s started=%s outcome=SUCCESS rc=%s restore=PLAN_RECOVERED\n' \
				"$code" "$name" "$started_at" "$run_rc"
			current_plan_code=
			current_run_id=
			continue
			;;
	esac
	if "$gsbench_bin" run --config "$config_file" --scenario "$code" \
		--duration "$duration" >"$run_log" 2>&1; then
		run_rc=0
	else
		run_rc=$?
	fi
	sed -n 's/^[^ ]* EVIDENCE //p' "$run_log" >"$evidence_lines"
	if ! jq -s --argjson code "$code" \
		'[.[] | select(.scenario_code == $code)] |
		 if length == 1 then .[0] else error("expected one scenario evidence envelope") end' \
		"$evidence_lines" >"$evidence_file" 2>"$evidence_file.error"; then
		current_run_id=$(sed -n 's/.*run_id=\([^ ]*\).*/\1/p' "$run_log" | head -n 1)
		if [ -n "$current_run_id" ]; then
			"$gsbench_bin" restore --config "$config_file" --run-id "$current_run_id" \
				>"restore/$code-missing-evidence.log" 2>&1 ||
				fail "scenario $code has no evidence and fallback restore failed"
		fi
		fail "scenario $code emitted invalid or missing evidence"
	fi
	current_run_id=$(jq -r '.run_id' "$evidence_file")
	name=$(jq -r '.scenario' "$evidence_file")
	outcome=$(jq -r '.outcome' "$evidence_file")
	restore_failed=$(jq -r '.restore.failed' "$evidence_file")
	restore_state=$(jq -r '.restore.state' "$evidence_file")
	if [ "$restore_failed" = true ]; then
		"$gsbench_bin" restore --config "$config_file" --run-id "$current_run_id" \
			>"restore/$code-retry.log" 2>&1 ||
			fail "scenario $code restore failed twice; dataset retained"
		restore_state=RETRIED_SUCCESS
	fi
	status_path=$artifacts/status/$code.log
	"$gsbench_bin" status --config "$config_file" --run-id "$current_run_id" \
		>"$status_path" 2>&1 || fail "status failed after scenario $code"
	target=$(jq -r '[.evidence[]? | select(.target != 0) |
		(.metric + "=" + (.target|tostring))] | join(";")' "$evidence_file")
	observed=$(jq -r '[.evidence[]? | select(.available == true) |
		(.metric + "=" + (.actual|tostring))] | join(";")' "$evidence_file")
	ceiling=$(jq -r '[.evidence[]?.details? |
		to_entries[]? | select(.key | test("ceiling|reachable|max_reachable|capacity")) |
		(.key + "=" + (.value|tostring))] | unique | join(";")' "$evidence_file")
	operations=$(jq -r '[.evidence[]? | select(.metric == "operations") | .actual][0] // 0' "$evidence_file")
	errors=$(jq -r '[.evidence[]? | select(.metric == "errors") | .actual][0] // 0' "$evidence_file")
	if [ "$outcome" = NOT_APPLICABLE ]; then
		applicability=NOT_APPLICABLE
	else
		applicability=APPLICABLE
	fi
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$code" "$(safe_field "$name")" "$(safe_field "$target")" \
		"$(safe_field "$observed")" "$(safe_field "$ceiling")" \
		"$operations" "$errors" "$outcome" "$applicability" "$evidence_file" \
		>>results.tsv
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$code" "$current_run_id" "$run_rc" "$duration" "$outcome" \
		"$restore_state" "$status_path" >>runs.tsv
	printf 'scenario=%s name=%s started=%s outcome=%s rc=%s restore=%s\n' \
		"$code" "$name" "$started_at" "$outcome" "$run_rc" "$restore_state"
	current_run_id=
done

stop_gstop
"$gsbench_bin" restore --config "$config_file" >restore/final.log 2>&1 ||
	fail "final restore failed; dataset retained"
"$gsbench_bin" status --config "$config_file" >status/final.log 2>&1 ||
	fail "final status failed; dataset retained"
grep -q 'stale recovery runs=0 database_runs=0 local_actions=0' status/final.log ||
	fail "residual recovery actions remain; dataset retained"

printf 'full scenario matrix complete; dataset retained: results=%s/results.tsv\n' \
	"$artifacts"
