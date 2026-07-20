#!/bin/bash
# gstop daemon manager: start/stop/status the daemon and, via a cron-registered
# `check`, keep it alive, restart it if its data log goes stale, cap its resource
# use, and prune old emergency logs. Adapted from gstop_manage.sh for the Go
# binary (a `gstop -d` process launched through run.sh).

ALARM_FILE="/var/log/abcsys.log"
LOG_THRESHOLD=1        # minutes without a fresh data log before restart
CPU_THRESHOLD=90       # percent
MEM_THRESHOLD=1024     # MB
EMERGENCY_LOGS_SAVE_DAYS=7

HOSTNAME=$(hostname)
INSTALL_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
GSTOP_RUN_SCRIPT="${INSTALL_DIR}/run.sh"
GSTOP_MANAGE_SCRIPT="${INSTALL_DIR}/gstop-manage.sh"
GSTOP_MANAGE_LOG_FILE="${INSTALL_DIR}/gstop_manage.log"
GSTOP_CONFIG_FILE="${INSTALL_DIR}/configs/gstop.cfg"
GAUSS_ENV_FILE="/home/Ruby/gauss_env_file"

# Daemon processes: the gstop binary running with -d.
GSTOP_PIDS=$(ps aux | grep -w gstop | grep -v grep | grep -E -- '-d|--daemon' | awk '{print $2}')
PIDS=($GSTOP_PIDS)
PIDS_NUM=${#PIDS[@]}

log() {
    echo "[$(date "+%Y-%m-%d %H:%M:%S")] $1" | tee -a "${GSTOP_MANAGE_LOG_FILE}"
}

send_alarm() {
    echo "[ALARM][$(date "+%Y-%m-%d %H:%M:%S")][${HOSTNAME}]$1" >> "${ALARM_FILE}"
}

show_gstop_daemon() {
    if [ "${PIDS_NUM}" -eq 0 ]; then
        log "the gstop daemon has exited"
        return
    fi
    for PID in "${PIDS[@]}"; do
        log "find gstop daemon, pid: ${PID}"
    done
}

start_gstop_daemon() {
    if [ "${PIDS_NUM}" -ne 0 ]; then
        log "the gstop daemon has been started"
        return
    fi
    if [ -f "${GAUSS_ENV_FILE}" ]; then
        log "start gstop daemon"
        source "${GAUSS_ENV_FILE}"
        nohup "${GSTOP_RUN_SCRIPT}" -d &>/dev/null &
    fi
}

stop_gstop_daemon() {
    if [ "${PIDS_NUM}" -eq 0 ]; then
        log "the gstop daemon has exited"
        return
    fi
    for PID in "${PIDS[@]}"; do
        log "stop gstop daemon, pid: ${PID}"
        kill -9 "${PID}"
    done
}

restart_gstop_daemon() {
    log "restart gstop daemon"
    for PID in "${PIDS[@]}"; do
        log "stop gstop daemon, pid: ${PID}"
        kill -9 "${PID}"
    done
    if [ -f "${GAUSS_ENV_FILE}" ]; then
        source "${GAUSS_ENV_FILE}"
        nohup "${GSTOP_RUN_SCRIPT}" -d &>/dev/null &
    fi
}

check_pids() {
    if [ "${PIDS_NUM}" -eq 0 ]; then
        send_alarm "Unable to find the PID of the gstop daemon process"
        return 1
    elif [ "${PIDS_NUM}" -gt 1 ]; then
        send_alarm "Too many gstop daemon processes: ${PIDS_NUM}"
        return 2
    fi
    return 0
}

check_log_modification() {
    local log_dir
    log_dir=$(grep "persist_file_base_dir" "${GSTOP_CONFIG_FILE}" | awk -F'"' '{print $2}')
    local log_abs_dir="${INSTALL_DIR}/${log_dir}"

    local latest_log_file
    latest_log_file=$(find "${log_abs_dir}" -name "*.log" -type f -printf "%T@ %p\n" 2>/dev/null | sort -nr | head -1 | awk '{print $2}')
    if [ -z "$latest_log_file" ]; then
        return 0
    fi

    if [ -f "$latest_log_file" ]; then
        local latest_mod_time current_time time_diff_seconds
        latest_mod_time=$(stat -c %Y "$latest_log_file" 2>/dev/null)
        current_time=$(date +%s)
        time_diff_seconds=$((current_time - latest_mod_time))
        local threshold_seconds=$((LOG_THRESHOLD * 60))
        if [ "$time_diff_seconds" -gt "$threshold_seconds" ]; then
            send_alarm "gstop's log file has not been updated for more than $((time_diff_seconds / 60)) minutes (threshold: ${LOG_THRESHOLD})"
            return 1
        fi
    fi
    return 0
}

delete_logs_of_emergency() {
    local emergency_logs_dir
    emergency_logs_dir=$(grep "emergency_log_base_dir" "${GSTOP_CONFIG_FILE}" | awk -F'"' '{print $2}')
    [ -z "$emergency_logs_dir" ] && return
    find "${INSTALL_DIR}/${emergency_logs_dir}" -type f -mtime +"${EMERGENCY_LOGS_SAVE_DAYS}" -delete 2>/dev/null
}

check_resource_usage() {
    for PID in "${PIDS[@]}"; do
        local TOP_DATA cpu mem_mb
        TOP_DATA=$(top -b -n 1 -p "${PID}" 2>/dev/null)
        cpu=$(echo "${TOP_DATA}" | awk -v pid="${PID}" 'NR>7 && $1 == pid { printf "%s", $9 }')
        if [ -n "$cpu" ] && [ "$(echo "$cpu > ${CPU_THRESHOLD}" | bc)" = "1" ]; then
            send_alarm "gstop's CPU usage is too high: PID $PID - CPU: ${cpu}%"
            kill -9 "${PID}"
            log "stop gstop daemon because of high CPU usage, pid: ${PID}"
            return
        fi
        mem_mb=$(echo "${TOP_DATA}" | awk -v pid="${PID}" 'NR>7 && $1 == pid {
            m=$6; if (m ~ /[tT]$/) m=m*1024*1024*1024; else if (m ~ /[gG]$/) m=m*1024*1024; else if (m ~ /[mM]$/) m=m*1024; printf "%.2f", m/1024 }')
        if [ -n "$mem_mb" ] && [ "$(echo "$mem_mb > ${MEM_THRESHOLD}" | bc)" = "1" ]; then
            send_alarm "gstop's MEM usage is too high: PID $PID - MEM: ${mem_mb}MB"
            kill -9 "${PID}"
            log "stop gstop daemon because of high MEM usage, pid: ${PID}"
            return
        fi
    done
}

check_gstop_daemon() {
    check_pids
    if [[ $? -eq 1 ]]; then
        start_gstop_daemon
        return
    fi
    check_log_modification
    if [[ $? -eq 1 ]]; then
        restart_gstop_daemon
        return
    fi
    check_resource_usage
}

register_gstop_monitor() {
    if [[ $EUID -ne 0 ]]; then
        echo "need root privilege"
        return
    fi
    if crontab -u Ruby -l 2>/dev/null | grep -qw "${GSTOP_MANAGE_SCRIPT}"; then
        log "gstop monitor has been registered"
        return
    fi
    (crontab -u Ruby -l 2>/dev/null; echo "*/10 * * * * ${GSTOP_MANAGE_SCRIPT} check") | crontab -u Ruby -
    log "gstop monitor registered successfully"
}

unregister_gstop_monitor() {
    if [[ $EUID -ne 0 ]]; then
        echo "need root privilege"
        return
    fi
    if ! crontab -u Ruby -l 2>/dev/null | grep -qw "${GSTOP_MANAGE_SCRIPT}"; then
        log "gstop monitor has not been registered"
        return
    fi
    crontab -u Ruby -l 2>/dev/null | grep -v "${GSTOP_MANAGE_SCRIPT}" | crontab -u Ruby -
    log "gstop monitor unregistered successfully"
}

show_usage() {
    cat <<EOF
Usage: $0 <command>

command:
    status         show gstop daemon pid
    start          start gstop daemon
    stop           stop gstop daemon
    register       register the daemon monitor cron job (needs root)
    unregister     unregister the daemon monitor cron job (needs root)
    check          health-check the daemon and prune old emergency logs
    help           show help
EOF
}

case "${1:-help}" in
    status)     show_gstop_daemon ;;
    start)      start_gstop_daemon ;;
    stop)       stop_gstop_daemon ;;
    register)   register_gstop_monitor ;;
    unregister) unregister_gstop_monitor ;;
    check)      check_gstop_daemon; delete_logs_of_emergency ;;
    -h|--help|help) show_usage ;;
    *)          echo "Unknown command: $1"; show_usage; exit 1 ;;
esac
