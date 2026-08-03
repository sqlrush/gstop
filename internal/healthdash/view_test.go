package healthdash

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestViewRendersActiveElapsedSectionAndBuildsSQLSelections(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	snapshot := Snapshot{
		StartedAt:       now.Add(-time.Hour),
		FastRefreshedAt: now,
		ActiveElapsedSQL: []SQLMetric{{
			SQLID: 99, Query: "select current", ActiveSessions: 6,
			RepresentativeElapsedUS: 12_345_000,
			RepresentativePID:       990, RepresentativeSessionID: "s99",
			CapturedAt: now,
		}},
		AverageSQL: []SQLMetric{{
			SQLID: 1, AverageUS: 2_000, Calls: 3,
			ActiveSessions: 1, Query: "select average",
		}},
		ExecutionSQL:  []SQLMetric{{SQLID: 2, CallsDelta: 8, Query: "select executions"}},
		MemoryEnabled: false,
		PlanChanges: []PlanChangeEvent{{
			SQLID: 3, Query: "select plan", FirstSeen: now.Add(-time.Minute), LastSeen: now,
		}},
		AnalyzeHistory: []AnalyzeRecord{{Database: "app", Schema: "public", Table: "orders", Source: "AUTOANALYZE", At: now}},
		InvalidIndexes: []InvalidIndex{{Database: "app", Schema: "public", Table: "orders", Index: "orders_idx", Ready: true}},
		Waits:          []WaitMetric{{Event: "DataFileRead", WaitsDelta: 2, TimeUSDelta: 100, AverageUS: 50, Share: .25, Type: "IO"}},
		CPU:            CPUStat{TimeUSDelta: 300, Share: .75},
		Lock: LockHealth{Waiters: 1, Blockers: 1, LongestWaitUS: 2_000_000, Chains: []LockChain{{
			WaiterPID: 11, WaiterSession: "11.1", BlockerPID: 22, BlockerSession: "22.1",
			LockType: "relation", Mode: "RowExclusiveLock", LockTag: "tag-a", SQLID: 4, Query: "update locked", ElapsedUS: 2_000_000,
		}}},
		Replication: ReplicationHealth{LocalRole: "Primary", Channels: []ReplicationChannel{{
			Direction: "SENDER", PeerRole: "Standby", PeerState: "Normal", State: "Streaming",
			Channel: "10.0.0.2:5432", LagBytes: 128, SyncPercent: 99.5, SyncState: "Sync",
		}}},
		Cluster: ClusterHealth{
			SQLAvailable: true, CMAvailable: true,
			Nodes: []ClusterNode{
				{Name: "cn_5001", Type: "C", Host: "10.0.0.1", Port: 5001, ActiveKnown: true, Active: true},
				{Name: "dn_6001", Type: "D", Host: "10.0.0.1", Port: 6001, NodePrimary: true},
			},
			Components: []ClusterComponent{{Kind: "CM SERVER", Node: "cm1", Instance: "1", Role: "Primary", State: "Primary"}},
		},
	}
	view := NewView(151)

	view.Render(snapshot, -1, false)
	text := strings.Join(view.Lines(), "\n")

	headers := []string{
		"1. CURRENT ACTIVE SQL ELAPSED TOP5",
		"2. AVG ELAPSED SINCE GSTOP TOP5 SQL",
		"3. EXECUTIONS SINCE GSTOP TOP5 SQL",
		"4. ACTIVE SQL DYNAMIC MEMORY TOP5",
		"5. PLAN CHANGES SINCE GSTOP",
		"6. ANALYZE HISTORY",
		"7. INVALID INDEXES",
		"8. WAIT EVENTS SINCE GSTOP TOP5",
		"9. CURRENT LOCK CHAINS TOP5",
		"10. PRIMARY/STANDBY REPLICATION",
		"11. DISTRIBUTED CLUSTER HEALTH",
	}
	last := -1
	for _, header := range headers {
		idx := strings.Index(text, header)
		if idx < 0 || idx <= last {
			t.Fatalf("section %q missing or out of order in:\n%s", header, text)
		}
		last = idx
	}
	if !strings.Contains(text, "1                   2.00ms") ||
		!strings.Contains(text, "3          1") {
		t.Fatalf("process-relative CALLS/ACTIVE values missing:\n%s", text)
	}
	for _, want := range []string{"12.35s", "6", "select current"} {
		if !strings.Contains(text, want) {
			t.Fatalf("active elapsed SQL output missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "动态内存采集未启用") {
		t.Fatalf("disabled memory message missing:\n%s", text)
	}
	for _, want := range []string{"waiters=1", "10.0.0.2:5432", "cn_5001", "CM SERVER"} {
		if !strings.Contains(text, want) {
			t.Fatalf("health extension missing %q:\n%s", want, text)
		}
	}
	selections := view.SelectableSQL()
	if len(selections) != 4 || selections[0].Section != "active-elapsed" ||
		selections[0].SQLID != 99 || selections[0].RepresentativePID != 990 ||
		selections[0].RepresentativeSessionID != "s99" || selections[1].SQLID != 1 ||
		selections[2].SQLID != 2 || selections[3].SQLID != 3 {
		t.Fatalf("selections = %+v", selections)
	}
}

func TestViewRendersEmptyActiveElapsedState(t *testing.T) {
	view := NewView(80)
	view.Render(Snapshot{}, -1, false)

	if !strings.Contains(strings.Join(view.Lines(), "\n"), "当前无活跃 SQL") {
		t.Fatalf("empty active elapsed message missing:\n%s", strings.Join(view.Lines(), "\n"))
	}
}

func TestViewSelectionCarriesRepresentativeIdentity(t *testing.T) {
	view := NewView(80)
	queryStart := time.Unix(34, 0)
	metric := SQLMetric{
		SQLID: 42, Query: "select 42",
		RepresentativePID: 4242, RepresentativeSessionID: "session-42",
		RepresentativeElapsedUS: 8_000, CapturedAt: time.Unix(42, 0),
		RepresentativeQueryStart: queryStart,
	}
	view.Render(Snapshot{AverageSQL: []SQLMetric{metric}}, 0, true)
	selection := view.SelectableSQL()[0]
	if selection.RepresentativePID != metric.RepresentativePID ||
		selection.RepresentativeSessionID != metric.RepresentativeSessionID ||
		selection.RepresentativeElapsedUS != metric.RepresentativeElapsedUS ||
		!selection.RepresentativeQueryStart.Equal(queryStart) ||
		!selection.CapturedAt.Equal(metric.CapturedAt) {
		t.Fatalf("selection=%+v metric=%+v", selection, metric)
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

func TestViewDetailRendersPlanAccessStatisticsAndRuntimeEvidence(t *testing.T) {
	view := NewView(100)
	analyzedAt := time.Date(2026, 7, 14, 10, 30, 0, 0, time.Local)
	detail := Detail{
		SQLID:         55,
		SQLText:       "select * from orders where customer_id = 7 order by created_at",
		PlanSource:    PlanSourceHistory,
		CatalogSource: PlanSourceExplain,
		PlanLines:     []string{"→ HOT Seq Scan on orders  (cost=0.00..100.00 rows=10 width=8)"},
		Plan: PlanAnalysis{Hotspot: &PlanHotspot{
			NodeType: "Seq Scan", Relation: "orders", SelfCost: 100, CostShare: 1,
			Explanation: "全表扫描 orders",
		}},
		Tables: []TableDiagnosis{{
			Access: TableAccess{Schema: "public", Table: "orders", ScanType: "Seq Scan"},
			Index: IndexDiagnosis{
				Assessment: IndexUnreasonable,
				Reasons:    []string{"没有可用前导索引"},
				Existing: []IndexInfo{{
					Name: "orders_old_idx", Columns: []string{"status"},
					Valid: true, Ready: true, Usable: true,
				}},
				SuggestedDDL: `CREATE INDEX "gstop_orders_customer_id" ON "public"."orders" ("customer_id");`,
			},
			Statistics: StatisticsAssessment{
				State: FreshnessSuspect, LastAnalyze: analyzedAt,
				LastDataChanged:  analyzedAt.Add(24 * time.Hour),
				TriggerAvailable: true, Trigger: 150, DueRatio: .75,
				Reasons: []string{"统计信息早于 7 天"},
			},
		}},
		Runtime: RuntimeEvidence{
			CPU: CPUSummary{
				Available: true, Samples: 3, NewestMS: 12, MedianMS: 10, MaxMS: 20,
				CPUToDBAvailable: true, CPUToDBRatio: .6,
			},
			ASH: ASHSummary{
				Available: true, ActiveSamples: 10, OnCPUSamples: 6, OnCPUShare: .6,
				Waits: []ASHWait{{Event: "WALFlushWait", Samples: 4}},
			},
		},
	}

	view.RenderDetail(detail)
	text := strings.Join(view.Lines(), "\n")

	for _, want := range []string{
		"PLAN HOTSPOT", "→ HOT", "self cost=100.00", "cost share=100.00%",
		"ACCESS & STATISTICS", "诊断依据: EXPLAIN估算计划 + 当前索引目录",
		"索引策略: 不合理", "orders_old_idx",
		`CREATE INDEX "gstop_orders_customer_id"`, "统计信息: 疑似过期",
		"2026-07-14 10:30:00", "due=75.00%",
		"Last data changed: 2026-07-15 10:30:00",
		"CPU history", "newest=12.00ms", "median=10.00ms", "max=20.00ms",
		"parallel worker CPU may be accumulated",
		"ASH inference", "on-CPU=6/10 (60.00%)", "WALFlushWait",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail diagnostics missing %q:\n%s", want, text)
		}
	}
}

func TestViewDetailRendersMaintenanceIndexAdviceAsNotApplicable(t *testing.T) {
	view := NewView(100)
	view.RenderDetail(Detail{
		SQLID:      8890,
		SQLText:    `ANALYZE "public".orders`,
		PlanSource: PlanSourceHistory,
		PlanLines:  []string{"Seq Scan on orders"},
		Tables: []TableDiagnosis{{
			Access: TableAccess{Schema: "public", Table: "orders", ScanType: "Seq Scan"},
			Index: IndexDiagnosis{
				Assessment: IndexNotApplicable,
				Reasons:    []string{"维护语句的表扫描不适用索引优化建议"},
			},
		}},
	})

	text := strings.Join(view.Lines(), "\n")
	for _, want := range []string{
		"索引策略: 不适用",
		"维护语句的表扫描不适用索引优化建议",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("maintenance detail missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Display-only suggestion:") ||
		strings.Contains(text, "CREATE INDEX") {
		t.Fatalf("maintenance detail contains index DDL:\n%s", text)
	}
}

func TestViewDetailRendersLoadingAndUnavailableEvidence(t *testing.T) {
	view := NewView(80)
	view.RenderDetail(Detail{SQLID: 77, Loading: true})
	text := strings.Join(view.Lines(), "\n")
	if !strings.Contains(text, "正在加载") {
		t.Fatalf("loading message missing:\n%s", text)
	}
}

func TestDetailViewRendersDatabaseSchemaAndUserOnSQLIDLine(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{
		RequestID: 6,
		SQLID:     606,
		Databases: []string{"postgres", "sales"},
		Users:     []string{"alice", "bob"},
	})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 6, Stage: StageCatalog, State: StageReady,
		Tables: []TableDiagnosis{
			{Access: TableAccess{Schema: "public", Table: "orders"}},
			{Access: TableAccess{Schema: "gsbench", Table: "fact_sales"}},
			{Access: TableAccess{Schema: "public", Table: "customers"}},
		},
	})

	view := NewView(160)
	view.RenderDetail(detail)
	text := strings.Join(view.Lines(), "\n")
	want := "SQL_ID: 606 | DATABASE: postgres,sales | SCHEMA: gsbench,public | USER: alice,bob"
	if !strings.Contains(text, want) {
		t.Fatalf("detail identity line missing:\n%s", text)
	}
}

func TestDetailViewShowsPlanWhileCPUAndCatalogAreStillLoading(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{RequestID: 1, SQLID: 10, SQLText: "select * from t"})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 1, Stage: StagePlan, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityExplain, Revision: 1,
			Source: PlanSourceExplain,
			Lines:  []string{"Seq Scan on t  (cost=0.00..10.00 rows=1 width=4)"},
		},
	})
	view := NewView(120)
	view.RenderDetail(detail)
	text := strings.Join(view.Lines(), "\n")
	for _, want := range []string{
		"Seq Scan on t", "CPU history: loading", "ASH inference: loading",
		"索引与统计信息: loading",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail output missing %q:\n%s", want, text)
		}
	}
}

func TestDetailViewKeepsEstimateWhenHistoryTimesOut(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{RequestID: 2, SQLID: 20})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 2, Stage: StagePlan, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityExplain, Revision: 1,
			Source: PlanSourceExplain,
			Lines:  []string{"Seq Scan on t  (cost=0.00..10.00 rows=1 width=4)"},
		},
	})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 2, Stage: StageHistory, State: StageTimeout,
		Message: "查询超过30秒",
	})
	view := NewView(120)
	view.RenderDetail(detail)
	text := strings.Join(view.Lines(), "\n")
	if !strings.Contains(text, PlanSourceExplain) ||
		!strings.Contains(text, "历史计划: timeout - 查询超过30秒") {
		t.Fatalf("detail output:\n%s", text)
	}
}

func TestDetailViewCatalogErrorRetainsHotspot(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{RequestID: 3, SQLID: 30})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 3, Stage: StagePlan, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityHistory, Revision: 1,
			Source: PlanSourceHistory,
			Lines:  []string{"Seq Scan on t  (cost=0.00..100.00 rows=1 width=4)"},
		},
	})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 3, Stage: StageCatalog, State: StageError,
		Message: "目录无权限",
	})
	view := NewView(120)
	view.RenderDetail(detail)
	text := strings.Join(view.Lines(), "\n")
	if !strings.Contains(text, "self cost=") ||
		!strings.Contains(text, "索引与统计信息: error - 目录无权限") {
		t.Fatalf("detail output:\n%s", text)
	}
}

func TestDetailViewCompletionRemovesGlobalLoadingBanner(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{RequestID: 4, SQLID: 40})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 4, Stage: StageComplete, State: StageDone, Done: true,
	})
	view := NewView(120)
	view.RenderDetail(detail)
	text := strings.Join(view.Lines(), "\n")
	if strings.Contains(text, "正在加载执行计划、运行证据和目录信息") {
		t.Fatalf("completed detail retained loading banner:\n%s", text)
	}
}

func TestDetailViewKeepsFailureCategoriesDistinct(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{RequestID: 5, SQLID: 50})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 5, Stage: StageEstimate, State: StageError,
		Message: "SQL包含绑定变量，不能安全估算",
	})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 5, Stage: StageHistory, State: StageTimeout,
		Message: "查询超过30s",
	})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 5, Stage: StageRelocate, State: StageCancelled,
		Message: "查询已取消",
	})
	view := NewView(120)
	view.RenderDetail(detail)
	text := strings.Join(view.Lines(), "\n")
	for _, want := range []string{"绑定变量", "timeout", "已取消"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "SQL可能包含绑定变量、权限不足或不是可独立规划的语句") {
		t.Fatalf("detail output collapsed distinct causes:\n%s", text)
	}
}

func TestViewDetailDoesNotInventUnavailableAnalyzeTrigger(t *testing.T) {
	view := NewView(80)
	view.RenderDetail(Detail{
		SQLID: 88,
		Tables: []TableDiagnosis{{
			Access: TableAccess{Schema: "public", Table: "orders", ScanType: "Seq Scan"},
			Index:  IndexDiagnosis{Assessment: IndexVerify},
			Statistics: StatisticsAssessment{
				State:       FreshnessVerify,
				LastAnalyze: time.Date(2026, 7, 20, 8, 0, 0, 0, time.Local),
			},
		}},
	})
	text := strings.Join(view.Lines(), "\n")
	if !strings.Contains(text, "trigger=unavailable | due=unavailable") {
		t.Fatalf("missing unavailable trigger:\n%s", text)
	}
	if strings.Contains(text, "trigger=0 | due=0.00%") {
		t.Fatalf("invented trigger values:\n%s", text)
	}
}
