package healthdash

import (
	"math"
	"strings"
	"testing"
)

func TestAnalyzePlanUsesIncrementalSelfCost(t *testing.T) {
	lines := []string{
		"Sort  (cost=130.00..131.00 rows=100 width=8)",
		"  Sort Key: o.created_at",
		"  ->  Hash Join  (cost=20.00..100.00 rows=100 width=8)",
		"        Hash Cond: (o.customer_id = c.id)",
		"        ->  Seq Scan on orders o  (cost=0.00..60.00 rows=1000 width=8)",
		"              Filter: (o.status = 'OPEN'::text)",
		"        ->  Hash  (cost=10.00..10.00 rows=100 width=4)",
		"              ->  Seq Scan on customers c  (cost=0.00..10.00 rows=100 width=4)",
	}

	got := AnalyzePlan(lines)

	if got.Hotspot == nil {
		t.Fatal("hotspot is nil")
	}
	if got.Hotspot.NodeType != "Seq Scan" || got.Hotspot.Relation != "orders" {
		t.Fatalf("hotspot=%+v", got.Hotspot)
	}
	if got.Hotspot.SelfCost != 60 {
		t.Fatalf("self cost=%v, want 60", got.Hotspot.SelfCost)
	}
	if math.Abs(got.Hotspot.CostShare-60.0/131.0) > 0.000001 {
		t.Fatalf("cost share=%v, want %v", got.Hotspot.CostShare, 60.0/131.0)
	}
	if !strings.Contains(got.AnnotatedLines[4], "→ HOT") {
		t.Fatalf("hotspot line=%q", got.AnnotatedLines[4])
	}
	for i, line := range got.AnnotatedLines {
		if i != 4 && strings.Contains(line, "→ HOT") {
			t.Fatalf("unexpected second hotspot at line %d: %q", i, line)
		}
	}
}

func TestAnalyzePlanClampsNegativeSelfCost(t *testing.T) {
	got := AnalyzePlan([]string{
		"Result  (cost=0.00..10.00 rows=1 width=4)",
		"  ->  Seq Scan on oversized  (cost=0.00..20.00 rows=1 width=4)",
	})

	if len(got.Nodes) != 2 {
		t.Fatalf("nodes=%d", len(got.Nodes))
	}
	if got.Nodes[0].SelfCost != 0 {
		t.Fatalf("root self cost=%v, want zero", got.Nodes[0].SelfCost)
	}
	if got.Hotspot == nil || got.Hotspot.Relation != "oversized" {
		t.Fatalf("hotspot=%+v", got.Hotspot)
	}
}

func TestAnalyzePlanAttachesMetadataAndKeepsGaussOperators(t *testing.T) {
	got := AnalyzePlan([]string{
		"Streaming (type: REDISTRIBUTE)  (cost=1.00..40.00 rows=10 width=8)",
		"  ->  Index Scan using orders_customer_idx on orders o  (cost=0.10..20.00 rows=10 width=8)",
		"        Index Cond: (o.customer_id = 7)",
		"        Filter: (o.status = 'OPEN'::text)",
	})

	if len(got.Nodes) != 2 || got.Nodes[0].NodeType != "Streaming (type: REDISTRIBUTE)" {
		t.Fatalf("nodes=%+v", got.Nodes)
	}
	scan := got.Nodes[1]
	if scan.NodeType != "Index Scan" || scan.IndexName != "orders_customer_idx" ||
		scan.Relation != "orders" || scan.Alias != "o" {
		t.Fatalf("scan=%+v", scan)
	}
	if len(scan.Metadata["Index Cond"]) != 1 || len(scan.Metadata["Filter"]) != 1 {
		t.Fatalf("metadata=%+v", scan.Metadata)
	}
}

func TestAnalyzePlanMalformedTextDegradesWithoutInventingHotspot(t *testing.T) {
	lines := []string{"Remote query plan unavailable", "Seq Scan on t (cost=broken)"}

	got := AnalyzePlan(lines)

	if got.Hotspot != nil || len(got.Nodes) != 0 {
		t.Fatalf("analysis=%+v", got)
	}
	if len(got.Notices) == 0 || !strings.Contains(got.Notices[0].Message, "无法解析") {
		t.Fatalf("notices=%+v", got.Notices)
	}
	if strings.Join(got.AnnotatedLines, "\n") != strings.Join(lines, "\n") {
		t.Fatalf("annotated=%v want=%v", got.AnnotatedLines, lines)
	}
}
