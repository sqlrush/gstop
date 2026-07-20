package emergency

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"gstop/internal/dbconn"
)

// statValue is one (stat_name, value) pair from pv_session_time.
type statValue struct {
	name  string
	value float64
}

// buildSessionTime groups pv_session_time rows by session id text.
func buildSessionTime(rows []dbconn.Row) map[string][]statValue {
	out := map[string][]statValue{}
	for _, row := range rows {
		sessid := row.Str(0)
		val, _ := row.Float(2)
		out[sessid] = append(out[sessid], statValue{name: row.Str(1), value: val})
	}
	return out
}

// sortTopSQL returns the aggregates ordered by active session count descending,
// preserving insertion order among ties (stable), matching sorted(..., reverse=True).
func sortTopSQL(agg map[int64]*TopSQL, order []int64) []TopSQL {
	out := make([]TopSQL, 0, len(order))
	for _, id := range order {
		out = append(out, *agg[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ActiveNum > out[j].ActiveNum
	})
	return out
}

// dbInt64 coerces a dynamically-typed column value to int64.
func dbInt64(v any) (int64, bool) {
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

// dbFloat coerces a dynamically-typed column value to float64 (0 on failure).
func dbFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case []byte:
		f, _ := strconv.ParseFloat(strings.TrimSpace(string(x)), 64)
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	}
	return 0
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

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// pyFloat renders a float like Python str(): whole numbers keep a ".0".
func pyFloat(x float64) string {
	s := strconv.FormatFloat(x, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}
