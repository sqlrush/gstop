package monitor

import (
	"strings"

	"gstop/internal/dbcompat"
)

// sessionQueryFor returns the session query adapted to the connected database.
// The GaussDB form is the production-validated original; openGauss enforces CASE
// branch type unity, so the BLOCKER pid branch (which shares a CASE with the ”
// text branch) is cast to text there.
func sessionQueryFor(kind dbcompat.Kind) string {
	if kind.IsOpenGauss() {
		return strings.Replace(sessionQueryGaussDB,
			"SELECT pid FROM pg_catalog.pg_stat_get_activity_with_conninfo",
			"SELECT pid::text FROM pg_catalog.pg_stat_get_activity_with_conninfo", 1)
	}
	return sessionQueryGaussDB
}

// The three queries the session panel runs each cycle, copied verbatim from
// monitor/session.py so the column order the display and emergency code rely on
// is preserved exactly.

const sessionMemQuery = `
            SELECT
                substring(sessid FROM '\.(.*)$') AS "sessid",
                SUM(totalsize)/1024/1024 AS "totalsize"
            FROM gs_session_memory_detail
            GROUP BY sessid;
        `

const sessionStatementQuery = `
            SELECT unique_sql_id, SUM(n_calls), SUM(n_soft_parse) FROM dbe_perf.statement GROUP BY unique_sql_id;
        `

const sessionQueryGaussDB = `WITH curr_clock_time AS (SELECT * FROM clock_timestamp())
        SELECT
            a.pid,
            a.usename,
            a.application_name,
            a.sessionid,
            a.unique_sql_id,
            a.query,
            (CASE
                WHEN a.query IS NULL OR LENGTH(a.query) = 0 THEN ''
                WHEN a.query ~* '^select.*for\s+update' THEN 'UPDATE'
                WHEN a.query ~* '^select' THEN 'SELECT'
                WHEN a.query ~* '^insert' THEN 'INSERT'
                WHEN a.query ~* '^update' THEN 'UPDATE'
                WHEN a.query ~* '^delete' THEN 'DELETE'
                WHEN a.query ~* '^create' THEN 'CREATE'
                WHEN a.query ~* '^alter' THEN 'ALTER'
                WHEN a.query ~* '^drop' THEN 'DROP'
                ELSE 'OTHER'
            END) AS "OPN",
            (CASE
                WHEN t.block_sessionid IS NULL THEN ''
                ELSE (SELECT pid FROM pg_catalog.pg_stat_get_activity_with_conninfo(NULL) WHERE sessionid = t.block_sessionid)
            END) AS "BLOCKER",
            (CASE
                WHEN a.state = 'active' THEN
                    EXTRACT(HOUR FROM ((SELECT * FROM curr_clock_time) - a.query_start)) * 3600 * 1000000 +
                    EXTRACT(MINUTE FROM ((SELECT * FROM curr_clock_time) - a.query_start)) * 60 * 1000000 +
                    EXTRACT(MICROSECOND FROM ((SELECT * FROM curr_clock_time) - a.query_start))
                ELSE 0
            END) AS "E/T",  --us
            a.state,
            (CASE
                WHEN a.state IS NULL THEN ''
                WHEN a.state NOT LIKE 'idle%' AND (t.wait_status = 'none' OR t.wait_status LIKE '% - %' OR t.wait_status = 'NestLoop' OR t.wait_status = 'create index' OR t.wait_status ~ '^(vacuum|analyze)') THEN 'ON CPU'
                WHEN t.wait_status = 'wait io' THEN 'USR I/O'
                ELSE 'WAITING'
            END) AS "STE",
            t.wait_status AS "EVENT",
            a.client_addr,
            a.client_hostname,
            a.client_port,
            a.datname,
            a.xact_start,
            a.query_start,
            (CASE
                WHEN a.state = 'active' THEN
                    EXTRACT(HOUR FROM ((SELECT * FROM curr_clock_time) - a.xact_start)) * 3600 * 1000000 +
                    EXTRACT(MINUTE FROM ((SELECT * FROM curr_clock_time) - a.xact_start)) * 60 * 1000000 +
                    EXTRACT(MICROSECOND FROM ((SELECT * FROM curr_clock_time) - a.xact_start))
                ELSE 0
            END),
            t.wait_event,
            t.block_sessionid
        FROM pg_catalog.pg_stat_activity a
            LEFT JOIN pg_thread_wait_status t ON a.sessionid = t.sessionid
        ORDER BY a.state;
        `
