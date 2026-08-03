package healthdash

import (
	"strings"
	"testing"
)

func TestExtractTableAccessesUsesHotspotDescendantScans(t *testing.T) {
	plan := AnalyzePlan([]string{
		"Hash Join  (cost=10.00..200.00 rows=20 width=8)",
		"  Hash Cond: (o.customer_id = c.id)",
		"  ->  Seq Scan on public.orders o  (cost=0.00..60.00 rows=100 width=8)",
		"        Filter: ((o.status = 'OPEN'::text) AND (o.created_at > '2026-01-01'::date))",
		"  ->  Hash  (cost=5.00..10.00 rows=20 width=4)",
		"        ->  Seq Scan on customers c  (cost=0.00..5.00 rows=20 width=4)",
	})

	accesses := ExtractTableAccesses(plan)
	if len(accesses) != 2 {
		t.Fatalf("accesses=%+v", accesses)
	}
	orders := accesses[0]
	if orders.Schema != "public" || orders.Table != "orders" || orders.Alias != "o" ||
		!orders.InHotspotSubtree {
		t.Fatalf("orders=%+v hotspot=%+v", orders, plan.Hotspot)
	}
	assertColumnUse(t, orders.Columns, "status", ColumnEquality)
	assertColumnUse(t, orders.Columns, "customer_id", ColumnJoin)
	assertColumnUse(t, orders.Columns, "created_at", ColumnRange)
	if !accesses[1].InHotspotSubtree {
		t.Fatalf("customers should be in join hotspot subtree: %+v", accesses[1])
	}
	assertColumnUse(t, accesses[1].Columns, "id", ColumnJoin)
}

func TestExtractTableAccessesIgnoresVerboseProjectionColumns(t *testing.T) {
	plan := AnalyzePlan([]string{
		"Hash Join  (cost=10.00..200.00 rows=20 width=8)",
		"  Output: o.id, o.payload, c.name",
		"  Hash Cond: (o.customer_id = c.id)",
		"  ->  Seq Scan on public.orders o  (cost=0.00..60.00 rows=100 width=8)",
		"        Output: o.id, o.customer_id, o.payload, o.status",
		"        Filter: (o.status = 'OPEN'::text)",
		"  ->  Hash  (cost=5.00..10.00 rows=20 width=4)",
		"        Output: c.name, c.id",
		"        ->  Seq Scan on public.customers c  (cost=0.00..5.00 rows=20 width=4)",
		"              Output: c.name, c.id",
	})

	accesses := ExtractTableAccesses(plan)
	if len(accesses) != 2 {
		t.Fatalf("accesses=%+v", accesses)
	}
	assertColumnUse(t, accesses[0].Columns, "status", ColumnEquality)
	assertColumnUse(t, accesses[0].Columns, "customer_id", ColumnJoin)
	assertColumnUse(t, accesses[1].Columns, "id", ColumnJoin)
	for _, access := range accesses {
		for _, column := range access.Columns {
			if column.Column == "payload" || column.Column == "name" ||
				column.Column == "id" && access.Table == "orders" {
				t.Fatalf("verbose Output column leaked into index evidence: access=%+v", access)
			}
		}
	}
}

func TestResolveScanLocalColumnUsesValidatesUnqualifiedColumn(t *testing.T) {
	access := TableAccess{LocalPredicates: []ScanPredicate{{
		Key: "Filter", Expression: "(lookup_key = 500000)",
	}}}
	got := resolveScanLocalColumnUses(access, map[string]bool{
		"id": true, "lookup_key": true, "payload": true,
	})

	assertColumnUse(t, got, "lookup_key", ColumnEquality)
}

func TestExtractTableAccessesRetainsOnlyDirectScanPredicates(t *testing.T) {
	plan := AnalyzePlan([]string{
		"Hash Join  (cost=0.00..20.00 rows=1 width=8)",
		"  Hash Cond: (lookup_key = other_key)",
		"  ->  Seq Scan on plan_data  (cost=0.00..10.00 rows=1 width=4)",
		"        Filter: (lookup_key = 500000)",
	})

	accesses := ExtractTableAccesses(plan)
	if len(accesses) != 1 || len(accesses[0].LocalPredicates) != 1 {
		t.Fatalf("accesses=%+v", accesses)
	}
	predicate := accesses[0].LocalPredicates[0]
	if predicate.Key != "Filter" || predicate.Expression != "(lookup_key = 500000)" {
		t.Fatalf("predicate=%+v", predicate)
	}
}

func TestResolveScanLocalColumnUsesRejectsUnknownQualifiedAndLiteralIdentifiers(t *testing.T) {
	access := TableAccess{LocalPredicates: []ScanPredicate{
		{Key: "Filter", Expression: "(unknown_key = 1)"},
		{Key: "Filter", Expression: "(p.lookup_key = 2)"},
		{Key: "Filter", Expression: "('lookup_key = 3' = payload)"},
	}}
	got := resolveScanLocalColumnUses(access, map[string]bool{
		"lookup_key": true,
		"payload":    true,
	})

	if len(got) != 0 {
		t.Fatalf("unsafe columns=%+v", got)
	}
}

func TestAssessIndexesRejectsNoIndexSequentialScan(t *testing.T) {
	got := AssessIndexes(TableAccess{
		Schema: "public", Table: "orders", ScanType: "Seq Scan",
		Columns: []ColumnUse{{Column: "customer_id", Kind: ColumnEquality}},
	}, nil, nil)

	if got.Assessment != IndexUnreasonable ||
		!strings.Contains(got.SuggestedDDL, `"public"."orders"`) ||
		!strings.Contains(got.SuggestedDDL, `("customer_id")`) {
		t.Fatalf("diagnosis=%+v", got)
	}
}

func TestAssessIndexesReportsDroppedPlanIndexAndSuggestsDDL(t *testing.T) {
	got := AssessIndexes(TableAccess{
		Schema:   "gsbench",
		Table:    "plan_data",
		ScanType: "Seq Scan",
		Columns: []ColumnUse{{
			Column: "lookup_key",
			Kind:   ColumnEquality,
		}},
		ReferencedIndexNames: []string{"plan_data_lookup_idx"},
	}, nil, nil)

	if got.Assessment != IndexUnreasonable || got.SuggestedDDL == "" {
		t.Fatalf("diagnosis=%+v", got)
	}
	if !strings.Contains(
		strings.Join(got.Reasons, "\n"),
		"plan_data_lookup_idx 当前不存在",
	) {
		t.Fatalf("reasons=%v", got.Reasons)
	}
}

func TestMergePlanIndexReferencesCarriesUnambiguousHistoricalIndex(t *testing.T) {
	current := []TableAccess{{Table: "plan_data", ScanType: "Seq Scan"}}
	history := []TableAccess{{
		Table:     "plan_data",
		ScanType:  "Index Scan",
		IndexName: "plan_data_lookup_idx",
	}}

	got := mergePlanIndexReferences(current, history)
	if len(got) != 1 ||
		strings.Join(got[0].ReferencedIndexNames, ",") != "plan_data_lookup_idx" {
		t.Fatalf("accesses=%+v", got)
	}
}

func TestMergePlanIndexReferencesSkipsAmbiguousSelfJoin(t *testing.T) {
	current := []TableAccess{
		{Table: "plan_data", Alias: "left_side", ScanType: "Seq Scan"},
		{Table: "plan_data", Alias: "right_side", ScanType: "Seq Scan"},
	}
	history := []TableAccess{{
		Table:     "plan_data",
		ScanType:  "Index Scan",
		IndexName: "plan_data_lookup_idx",
	}}

	got := mergePlanIndexReferences(current, history)
	if len(got[0].ReferencedIndexNames) != 0 ||
		len(got[1].ReferencedIndexNames) != 0 {
		t.Fatalf("ambiguous reference was assigned: %+v", got)
	}
}

func TestAssessIndexesUsesLeadingPrefixAndSuppressesDuplicateDDL(t *testing.T) {
	access := TableAccess{
		Schema: "public", Table: "orders", ScanType: "Seq Scan",
		Columns: []ColumnUse{
			{Column: "tenant_id", Kind: ColumnEquality},
			{Column: "created_at", Kind: ColumnRange},
		},
	}
	indexes := []IndexInfo{{
		Name: "orders_tenant_created_idx", Columns: []string{"tenant_id", "created_at"},
		Valid: true, Ready: true, Usable: true,
	}}
	got := AssessIndexes(access, indexes, nil)
	if got.Assessment != IndexReasonable || got.SuggestedDDL != "" {
		t.Fatalf("diagnosis=%+v", got)
	}

	indexes[0].Columns = []string{"created_at", "tenant_id"}
	got = AssessIndexes(access, indexes, nil)
	if got.Assessment != IndexUnreasonable || got.SuggestedDDL == "" {
		t.Fatalf("wrong-order diagnosis=%+v", got)
	}
}

func TestAssessIndexesRejectsUnavailableIndexStates(t *testing.T) {
	access := TableAccess{
		Table: "orders", ScanType: "Seq Scan",
		Columns: []ColumnUse{{Column: "customer_id", Kind: ColumnEquality}},
	}
	for _, index := range []IndexInfo{
		{Name: "invalid", Columns: []string{"customer_id"}, Ready: true, Usable: true},
		{Name: "unready", Columns: []string{"customer_id"}, Valid: true, Usable: true},
		{Name: "unusable", Columns: []string{"customer_id"}, Valid: true, Ready: true},
	} {
		got := AssessIndexes(access, []IndexInfo{index}, nil)
		if got.Assessment != IndexUnreasonable || got.SuggestedDDL == "" {
			t.Fatalf("index=%+v diagnosis=%+v", index, got)
		}
	}
}

func TestAssessIndexesOrdersCandidatesByUseKind(t *testing.T) {
	got := AssessIndexes(TableAccess{
		Table: "events", ScanType: "Seq Scan",
		Columns: []ColumnUse{
			{Column: "group_col", Kind: ColumnGroup},
			{Column: "range_col", Kind: ColumnRange},
			{Column: "join_col", Kind: ColumnJoin},
			{Column: "eq_col", Kind: ColumnEquality},
			{Column: "order_col", Kind: ColumnOrder},
			{Column: "eq_col", Kind: ColumnJoin},
		},
	}, nil, nil)
	want := []string{"eq_col", "join_col", "range_col", "order_col", "group_col"}
	if strings.Join(got.SuggestedColumns, ",") != strings.Join(want, ",") {
		t.Fatalf("columns=%v want=%v", got.SuggestedColumns, want)
	}
}

func TestAssessIndexesAvoidsLowSelectivityAndIneffectiveDDL(t *testing.T) {
	lowSelectivity := AssessIndexes(TableAccess{
		Table: "orders", ScanType: "Seq Scan",
		Columns: []ColumnUse{{Column: "status", Kind: ColumnEquality}},
	}, nil, map[string]ColumnStatistics{
		"status": {Available: true, NDistinct: 3, MostCommonFrequency: .70},
	})
	if lowSelectivity.Assessment != IndexVerify || lowSelectivity.SuggestedDDL != "" {
		t.Fatalf("low-selectivity=%+v", lowSelectivity)
	}

	noPredicate := AssessIndexes(TableAccess{
		Table: "audit_log", ScanType: "Seq Scan",
	}, nil, nil)
	if noPredicate.Assessment != IndexUnreasonable || noPredicate.SuggestedDDL != "" {
		t.Fatalf("no-predicate=%+v", noPredicate)
	}
}

func TestAssessIndexesRequiresExactPartialOrExpressionEvidence(t *testing.T) {
	access := TableAccess{
		Table: "orders", ScanType: "Seq Scan",
		Columns:    []ColumnUse{{Column: "customer_id", Kind: ColumnEquality}},
		Conditions: []string{"customer_id = 7", "status = 'OPEN'"},
	}
	partial := IndexInfo{
		Name: "orders_open_customer_idx", Columns: []string{"customer_id"},
		Valid: true, Ready: true, Usable: true, Predicate: "status = 'OPEN'",
	}
	if got := AssessIndexes(access, []IndexInfo{partial}, nil); got.Assessment != IndexReasonable {
		t.Fatalf("exact partial=%+v", got)
	}

	partial.Predicate = "status = 'CLOSED'"
	if got := AssessIndexes(access, []IndexInfo{partial}, nil); got.Assessment != IndexVerify {
		t.Fatalf("nonmatching partial=%+v", got)
	}

	expressionAccess := TableAccess{
		Table: "users", ScanType: "Seq Scan",
		Columns:    []ColumnUse{{Column: "email", Kind: ColumnEquality}},
		Conditions: []string{"lower(email) = 'person@example.com'"},
	}
	expression := IndexInfo{
		Name: "users_lower_email_idx", Valid: true, Ready: true, Usable: true,
		Expression: "lower(email)", Definition: "CREATE INDEX users_lower_email_idx ON users (lower(email))",
	}
	if got := AssessIndexes(expressionAccess, []IndexInfo{expression}, nil); got.Assessment != IndexReasonable {
		t.Fatalf("exact expression=%+v", got)
	}

	expression.Expression = "upper(email)"
	expression.Definition = "CREATE INDEX users_upper_email_idx ON users (upper(email))"
	if got := AssessIndexes(expressionAccess, []IndexInfo{expression}, nil); got.Assessment != IndexVerify {
		t.Fatalf("nonmatching expression=%+v", got)
	}
}

func assertColumnUse(t *testing.T, columns []ColumnUse, column string, kind ColumnUseKind) {
	t.Helper()
	for _, got := range columns {
		if got.Column == column && got.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s/%s in %+v", column, kind, columns)
}
