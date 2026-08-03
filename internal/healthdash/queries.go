package healthdash

const statementQuery = `SELECT unique_sql_id, current_database(), user_name,
    SUM(n_calls), SUM(db_time), MAX(query)
FROM dbe_perf.statement
WHERE query !~* '^\s*(start transaction|begin|commit|end)\s*;?\s*$'
GROUP BY unique_sql_id, user_name;`

const activeSQLQuery = `SELECT a.unique_sql_id, a.pid, a.sessionid, a.query,
    EXTRACT(EPOCH FROM (statement_timestamp() - a.query_start)) * 1000000 AS elapsed_us,
    a.datname, a.usename, a.query_start
FROM pg_catalog.pg_stat_activity a
WHERE a.state = 'active' AND a.unique_sql_id IS NOT NULL AND a.unique_sql_id <> 0;`

const waitQuery = `SELECT event, wait, total_wait_time, type
FROM dbe_perf.wait_events
WHERE event != 'none' AND event != 'wait cmd';`

const cpuQuery = `SELECT value FROM GS_INSTANCE_TIME WHERE stat_name = 'CPU_TIME';`

const lockHealthQuery = `SELECT w.pid, w.sessionid, b.pid, t.block_sessionid,
    COALESCE(l.locktype, ''), COALESCE(l.mode, ''), COALESCE(t.locktag, ''),
    w.unique_sql_id, w.query,
    EXTRACT(HOUR FROM (clock_timestamp() - w.query_start)) * 3600 * 1000000 +
    EXTRACT(MINUTE FROM (clock_timestamp() - w.query_start)) * 60 * 1000000 +
    EXTRACT(MICROSECOND FROM (clock_timestamp() - w.query_start)) AS elapsed_us
FROM pg_catalog.pg_thread_wait_status t
JOIN pg_catalog.pg_stat_activity w
  ON w.pid = t.tid AND w.sessionid = t.sessionid
LEFT JOIN pg_catalog.pg_stat_activity b
  ON b.sessionid = t.block_sessionid
LEFT JOIN pg_catalog.pg_lock_status() l
  ON l.pid = t.tid AND l.sessionid = t.sessionid
 AND l.locktag = t.locktag AND l.granted = false
WHERE t.block_sessionid IS NOT NULL;`

const replicationQuery = `SELECT 'LOCAL'::text AS direction,
    CASE WHEN pg_is_in_recovery() THEN 'Standby' ELSE 'Primary' END::text AS local_role,
    ''::text AS peer_role, ''::text AS peer_state, ''::text AS state, ''::text AS channel,
    ''::text AS sender_sent, ''::text AS receiver_received, ''::text AS receiver_write,
    ''::text AS receiver_flush, ''::text AS receiver_replay, '0'::text AS sync_percent,
    ''::text AS sync_state, '0'::text AS sync_priority
UNION ALL
SELECT 'SENDER'::text, local_role::text, peer_role::text, peer_state::text, state::text,
    channel::text, sender_sent_location::text, receiver_received_location::text,
    receiver_write_location::text, receiver_flush_location::text,
    receiver_replay_location::text, sync_percent::text, sync_state::text,
    sync_priority::text
FROM pg_catalog.pg_stat_get_wal_senders()
UNION ALL
SELECT 'RECEIVER'::text, local_role::text, peer_role::text, peer_state::text, state::text,
    channel::text, sender_sent_location::text, receiver_received_location::text,
    receiver_write_location::text, receiver_flush_location::text,
    receiver_replay_location::text, sync_percent::text, ''::text, '0'::text
FROM pg_catalog.pg_stat_get_wal_receiver();`

const replicationFallbackQuery = `SELECT 'LOCAL'::text AS direction,
    CASE WHEN pg_is_in_recovery() THEN 'Standby' ELSE 'Primary' END::text AS local_role,
    ''::text AS peer_role, ''::text AS peer_state, ''::text AS state, ''::text AS channel,
    ''::text AS sender_sent, ''::text AS receiver_received, ''::text AS receiver_write,
    ''::text AS receiver_flush, ''::text AS receiver_replay, '0'::text AS sync_percent,
    ''::text AS sync_state, '0'::text AS sync_priority
UNION ALL
SELECT 'SENDER'::text, 'Primary'::text, 'Standby'::text, ''::text, state::text,
    COALESCE(client_addr::text, client_hostname::text, application_name::text, '') ||
      CASE WHEN client_port IS NULL THEN '' ELSE ':' || client_port::text END,
    sender_sent_location::text, ''::text, receiver_write_location::text,
    receiver_flush_location::text, receiver_replay_location::text, '0'::text,
    sync_state::text, sync_priority::text
FROM pg_catalog.pg_stat_replication;`

const sessionMemoryQuery = `SELECT substring(sessid FROM '\.(.*)$') AS sessid,
    SUM(totalsize)/1024/1024 AS total_mb
FROM gs_session_memory_detail
GROUP BY sessid;`

const analyzeHistoryQuery = `SELECT schemaname, relname, last_analyze, last_autoanalyze
FROM pg_stat_all_tables
WHERE schemaname NOT IN (SELECT nspname FROM pg_namespace WHERE nspowner = 10)
  AND (last_analyze IS NOT NULL OR last_autoanalyze IS NOT NULL)
ORDER BY GREATEST(COALESCE(last_analyze, last_autoanalyze), COALESCE(last_autoanalyze, last_analyze)) DESC
LIMIT 20;`

const invalidIndexQuery = `SELECT a.schemaname, a.relname, a.indexrelname,
    b.indisusable, b.indisready, b.indisvalid
FROM pg_stat_user_indexes a
LEFT JOIN pg_index b ON a.indexrelid = b.indexrelid
WHERE (b.indisusable = 'f' OR b.indisready = 'f' OR b.indisvalid = 'f')
  AND a.schemaname NOT IN (SELECT nspname FROM pg_namespace WHERE nspowner = 10);`

const clusterTopologyQuery = `SELECT node_name, node_type::text, node_host, node_port,
    node_host1, node_port1, hostis_primary, nodeis_primary, nodeis_preferred,
    nodeis_central, nodeis_active, node_id
FROM pg_catalog.pgxc_node
ORDER BY node_type, node_name;`

const clusterCMCommand = `[ ! -f ~/gauss_env_file ] || . ~/gauss_env_file; command -v cm_ctl >/dev/null 2>&1 && cm_ctl query -Cvi`
