package healthdash

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gstop/internal/dbcompat"
	"gstop/internal/dbconn"
	"gstop/internal/sqlshape"
)

type validatedSession struct {
	PID       int64
	SessionID string
	SQLID     int64
	Query     string
}

type sessionLookupResult struct {
	Session validatedSession
	Found   bool
	Failed  bool
}

type planLookupResult struct {
	Lines   []string
	Failed  bool
	Message string
	Notices []DiagnosticNotice
}

func mustRowInt(row dbconn.Row, column int) int64 {
	value, _ := row.Int(column)
	return value
}

func (l *DetailLoader) validateSessionResult(
	ctx context.Context,
	target DetailTarget,
) sessionLookupResult {
	if target.RepresentativePID == 0 || target.SQLID == 0 ||
		(target.RepresentativeSessionID == "" && target.RepresentativeQueryStart.IsZero()) {
		return sessionLookupResult{}
	}
	query := "SELECT pid, sessionid::text, unique_sql_id, query " +
		"FROM pg_stat_activity WHERE state = 'active'" +
		" AND pid = " + strconv.FormatInt(target.RepresentativePID, 10) +
		" AND unique_sql_id = " + strconv.FormatInt(target.SQLID, 10)
	if target.RepresentativeSessionID != "" {
		query += " AND sessionid::text = " + sqlLiteral(target.RepresentativeSessionID)
	}
	if !target.RepresentativeQueryStart.IsZero() {
		query += " AND query_start = " +
			sqlLiteral(target.RepresentativeQueryStart.Format(time.RFC3339Nano)) +
			"::timestamptz"
	}
	query += " LIMIT 1;"
	rows := l.query(ctx, query)
	if rows == nil && ctx.Err() == nil {
		return sessionLookupResult{Failed: true}
	}
	if len(rows) != 1 {
		return sessionLookupResult{}
	}
	session := validatedSession{
		PID:       mustRowInt(rows[0], 0),
		SessionID: rows[0].Str(1),
		SQLID:     mustRowInt(rows[0], 2),
		Query:     rows[0].Str(3),
	}
	found := session.PID == target.RepresentativePID && session.SQLID == target.SQLID
	if target.RepresentativeSessionID != "" {
		found = found && session.SessionID == target.RepresentativeSessionID
	}
	return sessionLookupResult{Session: session, Found: found}
}

func (l *DetailLoader) relocateSessionResult(
	ctx context.Context,
	sqlID int64,
) sessionLookupResult {
	if sqlID == 0 {
		return sessionLookupResult{}
	}
	id := strconv.FormatInt(sqlID, 10)
	query := "SELECT pid, sessionid::text, unique_sql_id, query FROM pg_stat_activity " +
		"WHERE state = 'active' AND unique_sql_id = " + id +
		" ORDER BY query_start ASC, sessionid::text ASC, pid ASC LIMIT 1;"
	rows := l.query(ctx, query)
	if rows == nil && ctx.Err() == nil {
		return sessionLookupResult{Failed: true}
	}
	if len(rows) != 1 {
		return sessionLookupResult{}
	}
	session := validatedSession{
		PID:       mustRowInt(rows[0], 0),
		SessionID: rows[0].Str(1),
		SQLID:     mustRowInt(rows[0], 2),
		Query:     rows[0].Str(3),
	}
	if session.SQLID != sqlID || session.PID == 0 || session.SessionID == "" {
		return sessionLookupResult{}
	}
	return l.validateSessionResult(ctx, DetailTarget{
		SQLID:                   sqlID,
		RepresentativePID:       session.PID,
		RepresentativeSessionID: session.SessionID,
	})
}

func (l *DetailLoader) loadHistoricalPlanResult(ctx context.Context, sqlID int64) planLookupResult {
	if sqlID == 0 {
		return planLookupResult{}
	}
	columns, probeFailed := l.probeColumnsResult(ctx, "dbe_perf", "statement_history")
	if ctx.Err() != nil {
		return planLookupResult{}
	}
	if probeFailed {
		message := "statement_history 字段能力查询失败或无权限"
		return planLookupResult{
			Failed: true, Message: message,
			Notices: []DiagnosticNotice{{Area: "history", Message: message}},
		}
	}
	if !containsColumns(columns, "start_time", "unique_query_id", "query_plan") {
		message := "statement_history 缺少 start_time、unique_query_id 或 query_plan 字段"
		return planLookupResult{
			Failed: true, Message: message,
			Notices: []DiagnosticNotice{{Area: "history", Message: message}},
		}
	}
	id := strconv.FormatInt(sqlID, 10)
	query := "SELECT query_plan FROM dbe_perf.statement_history WHERE " +
		"start_time >= current_timestamp - interval '60 minutes'" +
		" AND unique_query_id = " + id +
		" AND query_plan IS NOT NULL AND BTRIM(query_plan) <> ''" +
		" ORDER BY start_time DESC LIMIT 1;"
	rows := l.query(ctx, query)
	if rows == nil && ctx.Err() == nil {
		message := "历史执行计划查询失败或无权限"
		return planLookupResult{
			Failed: true, Message: message,
			Notices: []DiagnosticNotice{{Area: "history", Message: message}},
		}
	}
	return planLookupResult{Lines: planLines(rows)}
}

func (l *DetailLoader) loadCachedWorkloadPlanResult(
	ctx context.Context,
	sqlText string,
) planLookupResult {
	sqlText = strings.TrimSpace(sqlText)
	if sqlText == "" {
		return planLookupResult{}
	}
	schemaRows := l.query(ctx, `SELECT n.nspname
FROM pg_catalog.pg_namespace n
JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid
WHERE c.relname = 'meta_plan_cache' AND c.relkind = 'r'
ORDER BY CASE WHEN n.nspname = 'gsbench' THEN 0 ELSE 1 END, n.nspname
LIMIT 32;`)
	if schemaRows == nil && ctx.Err() == nil {
		message := "场景预检计划缓存目录查询失败或无权限"
		return planLookupResult{Failed: true, Message: message}
	}
	signature := sqlshape.Signature(sqlText)
	var lookupFailed bool
	for _, row := range schemaRows {
		schema := strings.TrimSpace(row.Str(0))
		if schema == "" {
			continue
		}
		query := "SELECT plan_text, sql_text FROM " +
			quoteIdentifier(schema) + `."meta_plan_cache"` +
			" WHERE signature = " + sqlLiteral(signature) +
			" AND plan_text IS NOT NULL AND BTRIM(plan_text) <> ''" +
			" ORDER BY updated_at DESC LIMIT 1;"
		rows := l.query(ctx, query)
		if rows == nil && ctx.Err() == nil {
			lookupFailed = true
			continue
		}
		if len(rows) == 0 {
			continue
		}
		lines := planLines(rows)
		if len(lines) > 0 {
			return planLookupResult{Lines: lines}
		}
	}
	if lookupFailed {
		return planLookupResult{
			Failed:  true,
			Message: "场景预检计划缓存查询失败或无权限",
		}
	}
	return planLookupResult{}
}

func (l *DetailLoader) runtimePlanResult(ctx context.Context, session validatedSession) ([]string, bool) {
	if session.PID == 0 || !dbcompat.SupportsGsGetExplain(l.db.Kind()) {
		return nil, false
	}
	rows := l.query(ctx, fmt.Sprintf("SELECT * FROM gs_get_explain(%d);", session.PID))
	return planLines(rows), rows == nil && ctx.Err() == nil
}

func safeExplainStatement(query string) (string, error) {
	return dbcompat.SafeExplainStatement(query)
}
