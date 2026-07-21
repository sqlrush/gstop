package healthdash

const statementQuery = `SELECT unique_sql_id, SUM(n_calls), SUM(db_time), MAX(query)
FROM dbe_perf.statement
WHERE query !~* '^\s*(start transaction|begin|commit|end)\s*;?\s*$'
GROUP BY unique_sql_id;`

const activeSQLQuery = `SELECT a.unique_sql_id, a.pid, a.sessionid, a.query,
    EXTRACT(HOUR FROM (clock_timestamp() - a.query_start)) * 3600 * 1000000 +
    EXTRACT(MINUTE FROM (clock_timestamp() - a.query_start)) * 60 * 1000000 +
    EXTRACT(MICROSECOND FROM (clock_timestamp() - a.query_start)) AS elapsed_us
FROM pg_catalog.pg_stat_activity a
WHERE a.state = 'active' AND a.unique_sql_id IS NOT NULL AND a.unique_sql_id <> 0;`

const waitQuery = `SELECT event, wait, total_wait_time, type
FROM dbe_perf.wait_events
WHERE event != 'none' AND event != 'wait cmd';`

const cpuQuery = `SELECT value FROM GS_INSTANCE_TIME WHERE stat_name = 'CPU_TIME';`

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
