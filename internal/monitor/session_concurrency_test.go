package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"

	"gstop/internal/dbconn"
	"gstop/internal/model"
)

func TestSessionRefreshPublicationAndReorderAreRaceFree(t *testing.T) {
	m := &SessionMonitor{currOrderByCol: -1}

	const iterations = 5000
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			m.handleSQLResult(nil, nil, nil)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := range iterations {
			switch i % 3 {
			case 0:
				m.RefreshByPGA()
			case 1:
				m.RefreshByElapsedTime()
			default:
				m.RefreshByEvent()
			}
		}
	}()

	close(start)
	wg.Wait()
}

func TestSelectedSessionRowIsImmutableSnapshot(t *testing.T) {
	row := make(model.SessionRow, model.SessionRowLen)
	row[model.SIdxPID] = int64(11)
	row[model.SIdxSessionID] = "session-11"
	m := &SessionMonitor{
		values: []model.SessionRow{row},
	}

	selected, ok := m.selectedSessionRow(1)
	if !ok {
		t.Fatal("selectedSessionRow rejected the first visible row")
	}

	m.mu.Lock()
	m.values[0][model.SIdxPID] = int64(22)
	m.values[0][model.SIdxSessionID] = "session-22"
	m.mu.Unlock()

	if got := selected.Get(model.SIdxPID); got != int64(11) {
		t.Fatalf("snapshot PID changed with published rows: %v", got)
	}
	if got := selected.Get(model.SIdxSessionID); got != "session-11" {
		t.Fatalf("snapshot session changed with published rows: %v", got)
	}
}

func TestSessionLookupHelpersAreRaceFreeDuringPublication(t *testing.T) {
	display := make(model.SessionRow, model.SessionRowLen)
	display[model.SIdxPID] = int64(11)
	display[model.SIdxSQL] = "select display"
	raw := dbconn.Row{
		int64(11), nil, nil, "session-11", nil, "select full text",
	}
	m := &SessionMonitor{
		values:         []model.SessionRow{display},
		currSessResult: []dbconn.Row{raw},
	}

	const iterations = 5000
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		<-start
		for i := range iterations {
			m.mu.Lock()
			if i%2 == 0 {
				m.values = []model.SessionRow{display}
				m.currSessResult = []dbconn.Row{raw}
			} else {
				m.values = nil
				m.currSessResult = nil
			}
			m.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			_ = m.sqlFullTextBySessionID("session-11")
			_ = m.sqlTextByPID(int64(11))
		}
	}()

	close(start)
	wg.Wait()
}

func TestValidatedActiveQueryMatchesWholeSessionTuple(t *testing.T) {
	row := make(model.SessionRow, model.SessionRowLen)
	row[model.SIdxPID] = int64(11)
	row[model.SIdxSessionID] = "session-'11"
	row[model.SIdxSQLID] = int64(22)

	var issued string
	m := &SessionMonitor{
		queryContext: func(_ context.Context, query string) []dbconn.Row {
			issued = query
			return []dbconn.Row{{"select * from t where id = 1"}}
		},
	}

	query, ok := m.validatedActiveQuery(row)
	if !ok || query != "select * from t where id = 1" {
		t.Fatalf("validated query = %q, ok=%v", query, ok)
	}
	for _, fragment := range []string{
		"state = 'active'",
		"pid = 11",
		"sessionid::text = 'session-''11'",
		"unique_sql_id = 22",
	} {
		if !strings.Contains(issued, fragment) {
			t.Errorf("validation query missing %q: %s", fragment, issued)
		}
	}
}

func TestValidatedActiveQueryRejectsStaleSession(t *testing.T) {
	row := make(model.SessionRow, model.SessionRowLen)
	row[model.SIdxPID] = int64(11)
	row[model.SIdxSessionID] = "session-11"
	row[model.SIdxSQLID] = int64(22)
	m := &SessionMonitor{
		queryContext: func(context.Context, string) []dbconn.Row {
			return []dbconn.Row{}
		},
	}

	if query, ok := m.validatedActiveQuery(row); ok || query != "" {
		t.Fatalf("stale session accepted: query=%q ok=%v", query, ok)
	}
}
