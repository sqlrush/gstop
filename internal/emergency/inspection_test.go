package emergency

import (
	"strings"
	"testing"
	"time"

	"gstop/internal/dbconn"
)

func TestInspectionSlowSQLWindowQueryUsesIndexedTimeBounds(t *testing.T) {
	from := time.Date(2026, 7, 20, 10, 0, 0, 123000000, time.FixedZone("CST", 8*60*60))
	to := from.Add(5 * time.Minute)
	query := slowSQLWindowQuery(from, to)
	for _, want := range []string{
		"start_time > '2026-07-20 10:00:00.123+08:00'",
		"start_time <= '2026-07-20 10:05:00.123+08:00'",
		"is_slow_sql = true",
		"db_name != 'postgres'",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("window query missing %q:\n%s", want, query)
		}
	}
}

func TestInspectionSlowSQLFailureDoesNotAdvanceSuccessfulCursor(t *testing.T) {
	from := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	firstUpper := from.Add(time.Minute)
	secondUpper := from.Add(2 * time.Minute)
	now := firstUpper
	calls := 0
	var successfulQuery string
	s := &Inspection{
		slowSQLSince: from,
		now:          func() time.Time { return now },
		query: func(query string) []dbconn.Row {
			calls++
			if calls == 1 {
				return nil
			}
			successfulQuery = query
			return []dbconn.Row{{int64(7)}}
		},
	}

	if got := s.checkSlowSQL().value; got != 0 {
		t.Fatalf("failed window value = %v, want 0", got)
	}
	if !s.slowSQLSince.Equal(from) {
		t.Fatalf("failed query advanced cursor to %v", s.slowSQLSince)
	}

	now = secondUpper
	if got := s.checkSlowSQL().value; got != 7 {
		t.Fatalf("successful window value = %v, want 7", got)
	}
	if !s.slowSQLSince.Equal(secondUpper) {
		t.Fatalf("successful query cursor = %v, want %v", s.slowSQLSince, secondUpper)
	}
	if !strings.Contains(successfulQuery, "start_time > '2026-07-20 10:00:00Z'") {
		t.Fatalf("retry skipped failed interval: %s", successfulQuery)
	}
}
