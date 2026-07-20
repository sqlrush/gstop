package monitor

import (
	"strconv"
	"strings"

	"gstop/internal/model"
	"gstop/internal/tui"
)

// TerminateSelected terminates the currently selected session after a second
// confirmation. Port of session.terminate_selected_session.
func (m *SessionMonitor) TerminateSelected(screen *tui.Screen, cursorY int) {
	idx := m.SelectedIndex(cursorY)
	if idx < 0 || idx >= len(m.values) {
		return
	}
	if !tui.TerminateConfirmPassed(screen, 0, cursorY) {
		return
	}
	row := m.values[idx]
	m.terminateSession(row.Get(model.SIdxPID), row.Get(model.SIdxSessionID))
}

// TerminateAll terminates every session sharing the selected row's unique SQL id.
// Port of session.terminate_all_sessions.
func (m *SessionMonitor) TerminateAll(screen *tui.Screen, cursorY int) {
	idx := m.SelectedIndex(cursorY)
	if idx < 0 || idx >= len(m.values) {
		return
	}
	if !tui.TerminateConfirmPassed(screen, 0, cursorY) {
		return
	}
	row := m.values[idx]
	sqlID, ok := sInt64(row.Get(model.SIdxSQLID))
	if !ok || sqlID == 0 {
		m.deps.Logger.Error("Cannot terminate the sessions with SQL ID is 0")
		return
	}
	cmd, err := model.TerminateUnlimitedSessions(strconv.FormatInt(sqlID, 10))
	if err != nil {
		m.deps.Logger.Error("build terminate block failed: %v", err)
		return
	}
	m.deps.DB.NoReturn(cmd)
}

// pidInBlockers reports whether pid appears among the collected blocker values,
// matching "pid in tmp_curr_sess_blocker" for integer pids.
func pidInBlockers(pid any, blockers []any) bool {
	p, ok := sInt64(pid)
	if !ok {
		return false
	}
	for _, b := range blockers {
		if bID, ok := sInt64(b); ok && bID == p {
			return true
		}
	}
	return false
}

// sortKeyGreater orders a before b for a descending sort, comparing numerically
// when both values are numeric and lexically otherwise (nil sorts as "").
func sortKeyGreater(a, b any) bool {
	af, aok := sFloat(a)
	bf, bok := sFloat(b)
	if aok && bok {
		return af > bf
	}
	return sortStr(a) > sortStr(b)
}

func sortStr(v any) string {
	if v == nil {
		return ""
	}
	return model.DisplayValue(v)
}

func containsInt64(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// sInt64 coerces a dynamically-typed cell to int64.
func sInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	case []byte:
		return parseInt64(string(x))
	case string:
		return parseInt64(x)
	}
	return 0, false
}

// sFloat coerces a dynamically-typed cell to float64.
func sFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case []byte:
		return parseFloat64(string(x))
	case string:
		return parseFloat64(x)
	}
	return 0, false
}

func parseInt64(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f), true
	}
	return 0, false
}

func parseFloat64(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}

// sTruncate returns the first n runes of s, or "" when n <= 0, matching the
// Python slice s[:width-1].
func sTruncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
