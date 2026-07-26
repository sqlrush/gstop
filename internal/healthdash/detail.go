package healthdash

import (
	"fmt"
	"strconv"
	"strings"

	"gstop/internal/dbcompat"
	"gstop/internal/dbconn"
)

const (
	PlanSourceHistory = "历史真实计划"
	PlanSourceRuntime = "运行中实时计划"
	PlanSourceExplain = "EXPLAIN估算计划"
)

// Detail is the scrollable SQL detail page's data.
type Detail struct {
	SQLID      int64
	SQLText    string
	RunningPID int64
	PlanSource string
	PlanLines  []string
	Error      string
}

// DetailLoader resolves SQL text and the best available plan without executing
// the statement itself.
type DetailLoader struct{ db Queryer }

func NewDetailLoader(db Queryer) *DetailLoader { return &DetailLoader{db: db} }

// Load applies the fixed priority: real history plan, GaussDB live plan, then a
// read-only EXPLAIN estimate.
func (l *DetailLoader) Load(sqlID int64, query string) Detail {
	detail := Detail{SQLID: sqlID, SQLText: strings.TrimSpace(query)}
	id := strconv.FormatInt(sqlID, 10)

	historySQL := "SELECT query_plan FROM dbe_perf.statement_history " +
		"WHERE start_time >= current_timestamp - interval '10 minutes' " +
		"AND unique_query_id = '" + id + "' ORDER BY start_time DESC LIMIT 1;"
	if lines := planLines(l.db.Query(historySQL)); len(lines) > 0 {
		detail.PlanSource = PlanSourceHistory
		detail.PlanLines = lines
		return detail
	}

	activitySQL := "SELECT pid, query FROM pg_stat_activity WHERE state = 'active' AND unique_sql_id = " + id + " ORDER BY query_start LIMIT 1;"
	activity := l.db.Query(activitySQL)
	if len(activity) > 0 {
		detail.RunningPID, _ = activity[0].Int(0)
		if detail.SQLText == "" {
			detail.SQLText = activity[0].Str(1)
		}
	}
	if detail.RunningPID != 0 && dbcompat.SupportsGsGetExplain(l.db.Kind()) {
		runtimeSQL := fmt.Sprintf("SELECT * FROM gs_get_explain(%d);", detail.RunningPID)
		if lines := planLines(l.db.Query(runtimeSQL)); len(lines) > 0 {
			detail.PlanSource = PlanSourceRuntime
			detail.PlanLines = lines
			return detail
		}
	}

	if detail.SQLText == "" {
		textSQL := "SELECT query FROM dbe_perf.statement WHERE unique_sql_id = " + id + " AND query IS NOT NULL LIMIT 1;"
		if rows := l.db.Query(textSQL); len(rows) > 0 {
			detail.SQLText = rows[0].Str(0)
		}
	}
	statement, err := safeExplainStatement(detail.SQLText)
	if err != nil {
		detail.Error = err.Error()
		return detail
	}
	if lines := planLines(l.db.Query("EXPLAIN " + statement)); len(lines) > 0 {
		detail.PlanSource = PlanSourceExplain
		detail.PlanLines = lines
		return detail
	}
	detail.Error = "无法获取执行计划：SQL可能包含绑定变量、权限不足或不是可独立规划的语句"
	return detail
}

func safeExplainStatement(query string) (string, error) {
	statement := strings.TrimSpace(query)
	statement = strings.TrimRight(statement, "; \t\r\n")
	if statement == "" {
		return "", fmt.Errorf("无法获取执行计划：没有可用的完整SQL文本")
	}
	if strings.Contains(statement, ";") {
		return "", fmt.Errorf("为避免执行附加语句，不对多语句SQL运行EXPLAIN")
	}
	return statement, nil
}

func planLines(rows []dbconn.Row) []string {
	if rows == nil {
		return nil
	}
	var lines []string
	for _, row := range rows {
		for _, line := range strings.Split(row.Str(0), "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}
	}
	return lines
}
