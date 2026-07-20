package model

import (
	"strings"
	"testing"
)

func TestTerminateBuildersValidateSQLID(t *testing.T) {
	// Non-numeric or hostile ids are rejected, closing the injection vector.
	bad := []string{"1; DROP TABLE users", "abc", "", "12 34", "-1"}
	for _, id := range bad {
		if _, err := TerminateUnlimitedSessions(id); err == nil {
			t.Errorf("expected error for id %q", id)
		}
		if _, err := TerminateLimitedSessions(id, 5); err == nil {
			t.Errorf("expected error for id %q (limited)", id)
		}
	}
}

func TestTerminateLimitedEmbedsParams(t *testing.T) {
	sql, err := TerminateLimitedSessions("343801306", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "target_unique_sql_id bigint := 343801306") {
		t.Error("sql id not embedded")
	}
	if !strings.Contains(sql, "max_terminate_count integer := 7") {
		t.Error("max count not embedded")
	}
	// The literal percent signs for RAISE NOTICE must survive formatting.
	if !strings.Contains(sql, "unique_sql_id = %'") {
		t.Error("RAISE NOTICE percent placeholder was mangled")
	}
}

func TestTerminateWithTimeEmbedsInterval(t *testing.T) {
	sql, err := TerminateUnlimitedSessionsWithTime("100", 50)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "'50 ms'::interval") {
		t.Errorf("timeout interval not embedded: %s", sql)
	}
}
