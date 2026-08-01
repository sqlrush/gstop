package monitor

import (
	"strings"
	"testing"

	"gstop/internal/dbcompat"
)

func TestSessionDetailQueryExcludesThreadPoolWorkerRows(t *testing.T) {
	query := sessionQueryFor(dbcompat.KindOpenGauss)
	if !strings.Contains(query,
		"COALESCE(a.sessionid,0) <> 0 OR a.backend_start IS NOT NULL",
	) {
		t.Fatalf("session detail query includes thread-pool worker rows: %s", query)
	}
}
