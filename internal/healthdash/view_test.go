package healthdash

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestViewRendersSevenSectionsAndBuildsSQLSelections(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	snapshot := Snapshot{
		StartedAt:       now.Add(-time.Hour),
		FastRefreshedAt: now,
		AverageSQL:      []SQLMetric{{SQLID: 1, AverageUS: 2_000, Query: "select average"}},
		ExecutionSQL:    []SQLMetric{{SQLID: 2, CallsDelta: 8, Query: "select executions"}},
		MemoryEnabled:   false,
		PlanChanges: []PlanChangeEvent{{
			SQLID: 3, Query: "select plan", FirstSeen: now.Add(-time.Minute), LastSeen: now,
		}},
		AnalyzeHistory: []AnalyzeRecord{{Database: "app", Schema: "public", Table: "orders", Source: "AUTOANALYZE", At: now}},
		InvalidIndexes: []InvalidIndex{{Database: "app", Schema: "public", Table: "orders", Index: "orders_idx", Ready: true}},
		Waits:          []WaitMetric{{Event: "DataFileRead", WaitsDelta: 2, TimeUSDelta: 100, AverageUS: 50, Share: .25, Type: "IO"}},
		CPU:            CPUStat{TimeUSDelta: 300, Share: .75},
	}
	view := NewView(151)

	view.Render(snapshot, -1, false)
	text := strings.Join(view.Lines(), "\n")

	headers := []string{
		"1. AVG ELAPSED TOP3 SQL",
		"2. EXECUTIONS SINCE GSTOP TOP3 SQL",
		"3. ACTIVE SQL DYNAMIC MEMORY TOP3",
		"4. PLAN CHANGES SINCE GSTOP",
		"5. ANALYZE HISTORY",
		"6. INVALID INDEXES",
		"7. WAIT EVENTS SINCE GSTOP TOP5",
	}
	last := -1
	for _, header := range headers {
		idx := strings.Index(text, header)
		if idx < 0 || idx <= last {
			t.Fatalf("section %q missing or out of order in:\n%s", header, text)
		}
		last = idx
	}
	if !strings.Contains(text, "动态内存采集未启用") {
		t.Fatalf("disabled memory message missing:\n%s", text)
	}
	selections := view.SelectableSQL()
	if len(selections) != 3 || selections[0].SQLID != 1 || selections[1].SQLID != 2 || selections[2].SQLID != 3 {
		t.Fatalf("selections = %+v", selections)
	}
}

func TestViewEnsureVisibleScrollsSelectedSQLIntoViewport(t *testing.T) {
	view := NewView(80)
	view.Render(Snapshot{
		MemoryEnabled: true,
		AverageSQL: []SQLMetric{
			{SQLID: 1, Query: "one"},
			{SQLID: 2, Query: "two"},
			{SQLID: 3, Query: "three"},
		},
		ExecutionSQL: []SQLMetric{{SQLID: 4, Query: "four"}},
	}, -1, true)

	selections := view.SelectableSQL()
	selected := len(selections) - 1
	scroll := view.EnsureVisible(selected, 0, 5)
	row := selections[selected].Row
	if row < scroll || row >= scroll+5 {
		t.Fatalf("selected row %d not visible in [%d,%d)", row, scroll, scroll+5)
	}
}

func TestViewHighlightsSelectedSQLRow(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(100, 20)

	view := NewView(100)
	snapshot := Snapshot{MemoryEnabled: false, AverageSQL: []SQLMetric{{SQLID: 1, Query: "select one"}}}
	view.Render(snapshot, 0, true)
	selected := view.SelectableSQL()[0]
	view.Blit(screen, selected.Row)

	_, _, style, _ := screen.GetContent(0, 0)
	_, _, attrs := style.Decompose()
	if attrs&tcell.AttrReverse == 0 {
		t.Fatalf("selected SQL row style attributes = %v, want reverse", attrs)
	}
}

func TestViewDetailRendersCompleteSQLPlanAndSource(t *testing.T) {
	view := NewView(60)
	detail := Detail{
		SQLID:      55,
		SQLText:    "select a_really_long_column_name from a_really_long_table_name where id = 55",
		PlanSource: PlanSourceHistory,
		PlanLines:  []string{"Seq Scan on a_really_long_table_name", "  Filter: (id = 55)"},
	}

	view.RenderDetail(detail)
	text := strings.Join(view.Lines(), "\n")

	for _, want := range []string{"SQL DETAILS", "SQL_ID: 55", PlanSourceHistory, "a_really_long_column_name", "a_really_long_table_name", "Filter: (id = 55)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail view missing %q:\n%s", want, text)
		}
	}
}
