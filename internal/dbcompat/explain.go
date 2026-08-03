package dbcompat

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	dollarBindPattern = regexp.MustCompile(`\$[0-9]+`)
	namedBindPattern  = regexp.MustCompile(`(^|[^:]):[A-Za-z_][A-Za-z0-9_$]*`)
)

// SafeExplainStatement returns one statement that is safe to prefix with plain
// EXPLAIN. Plain EXPLAIN only plans the allowed DML classes; rejecting statement
// separators prevents a simple-query batch from executing trailing SQL.
func SafeExplainStatement(query string) (string, error) {
	statement := strings.TrimSpace(query)
	statement = strings.TrimRight(statement, "; \t\r\n")
	if statement == "" {
		return "", fmt.Errorf("无法获取执行计划：没有可用的完整SQL文本")
	}
	if strings.Contains(statement, ";") {
		return "", fmt.Errorf("为避免执行附加语句，不对多语句SQL运行EXPLAIN")
	}
	if dollarBindPattern.MatchString(statement) ||
		strings.Contains(statement, "?") ||
		namedBindPattern.MatchString(statement) {
		return "", fmt.Errorf("SQL包含绑定变量，无法安全运行普通EXPLAIN")
	}
	fields := strings.Fields(statement)
	if len(fields) == 0 {
		return "", fmt.Errorf("无法获取执行计划：没有可用的完整SQL文本")
	}
	switch strings.ToUpper(fields[0]) {
	case "SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "MERGE":
		return statement, nil
	default:
		return "", fmt.Errorf("不支持对%s语句运行EXPLAIN", strings.ToUpper(fields[0]))
	}
}
