package healthdash

import (
	"testing"

	"gstop/internal/sqlshape"
)

func TestBindAccessSchemasParsesQuotedPlanRelationIdentifiers(t *testing.T) {
	tests := []struct {
		name     string
		planLine string
		relation sqlshape.RelationRef
	}{
		{
			name:     "escaped quote and dot in schema",
			planLine: `Seq Scan on "S.with.dot"."T""x"  (cost=0.00..1.00 rows=1 width=8)`,
			relation: sqlshape.RelationRef{
				Schema: sqlshape.Identifier{Value: "S.with.dot", Quoted: true},
				Table:  sqlshape.Identifier{Value: `T"x`, Quoted: true},
			},
		},
		{
			name:     "dot in table",
			planLine: `Seq Scan on "S"."T.with.dot"  (cost=0.00..1.00 rows=1 width=8)`,
			relation: sqlshape.RelationRef{
				Schema: sqlshape.Identifier{Value: "S", Quoted: true},
				Table:  sqlshape.Identifier{Value: "T.with.dot", Quoted: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accesses := ExtractTableAccesses(AnalyzePlan([]string{tt.planLine}))
			got := bindAccessSchemas(accesses, []sqlshape.RelationRef{tt.relation})
			if len(got) != 1 {
				t.Fatalf("bindAccessSchemas() returned %d accesses, want 1: %+v", len(got), got)
			}
			if got[0].Schema != tt.relation.Schema.Value || got[0].Table != tt.relation.Table.Value {
				t.Fatalf(
					"bindAccessSchemas() access = %q.%q, want %q.%q",
					got[0].Schema,
					got[0].Table,
					tt.relation.Schema.Value,
					tt.relation.Table.Value,
				)
			}
		})
	}
}
