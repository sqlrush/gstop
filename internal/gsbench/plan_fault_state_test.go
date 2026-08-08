package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

type planFaultCatalogFake struct {
	definition    string
	definitionErr error
	tableMissing  bool
	tableErr      error
	usable        int
	usableErr     error
	options       string
	optionsErr    error
	queries       []string
	args          [][]any
}

func (d *planFaultCatalogFake) ScanReadOnly(
	_ context.Context,
	query string,
	args []any,
	dest ...any,
) error {
	d.queries = append(d.queries, query)
	d.args = append(d.args, append([]any(nil), args...))
	switch {
	case strings.Contains(query, "FROM pg_tables"):
		if d.tableErr != nil {
			return d.tableErr
		}
		count := 1
		if d.tableMissing {
			count = 0
		}
		*(dest[0].(*int)) = count
		return nil
	case strings.Contains(query, "pg_get_indexdef"):
		if d.definitionErr != nil {
			return d.definitionErr
		}
		*(dest[0].(*string)) = d.definition
		return nil
	case strings.Contains(query, "FROM pg_index"):
		if d.usableErr != nil {
			return d.usableErr
		}
		*(dest[0].(*int)) = d.usable
		return nil
	case strings.Contains(query, "FROM pg_attribute"):
		if d.optionsErr != nil {
			return d.optionsErr
		}
		*(dest[0].(*string)) = d.options
		return nil
	default:
		return errors.New("unexpected catalog query")
	}
}

func TestInspectPlanFaultState601UsesOnlyLiveIndexStructure(t *testing.T) {
	tests := []struct {
		name          string
		definition    string
		definitionErr error
		tableMissing  bool
		tableErr      error
		usable        int
		usableErr     error
		want          PlanFaultLiveState
	}{
		{
			name:       "canonical",
			definition: `CREATE UNIQUE INDEX plan_data_lookup_idx ON "gsbench".plan_data USING ubtree (lookup_key, dist_key) TABLESPACE pg_default`,
			usable:     1,
			want:       PlanFaultRestored,
		},
		{
			name:          "missing",
			definitionErr: sql.ErrNoRows,
			want:          PlanFaultPresent,
		},
		{
			name:       "wrong shape",
			definition: `CREATE INDEX plan_data_lookup_idx ON "gsbench".plan_data (dist_key)`,
			usable:     1,
			want:       PlanFaultDrifted,
		},
		{
			name:       "unusable",
			definition: `CREATE UNIQUE INDEX plan_data_lookup_idx ON "gsbench".plan_data (lookup_key,dist_key)`,
			usable:     0,
			want:       PlanFaultDrifted,
		},
		{
			name:         "parent table missing",
			tableMissing: true,
			want:         PlanFaultUnavailable,
		},
		{
			name:     "parent table unavailable",
			tableErr: errors.New("table catalog unavailable"),
			want:     PlanFaultUnavailable,
		},
		{
			name:          "definition unavailable",
			definitionErr: errors.New("catalog unavailable\nprivate detail"),
			want:          PlanFaultUnavailable,
		},
		{
			name:       "usability unavailable",
			definition: `CREATE UNIQUE INDEX plan_data_lookup_idx ON "gsbench".plan_data (lookup_key,dist_key)`,
			usableErr:  errors.New("usability unavailable"),
			want:       PlanFaultUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &planFaultCatalogFake{
				definition:    tt.definition,
				definitionErr: tt.definitionErr,
				tableMissing:  tt.tableMissing,
				tableErr:      tt.tableErr,
				usable:        tt.usable,
				usableErr:     tt.usableErr,
			}
			got, err := InspectPlanFaultState(
				context.Background(), db, "gsbench", ScenarioCode(601),
			)
			if err != nil {
				t.Fatalf("InspectPlanFaultState() error = %v", err)
			}
			if got.State != tt.want {
				t.Fatalf("inspection=%+v want state=%s", got, tt.want)
			}
			if got.Code != ScenarioCode(601) || got.Object != `"gsbench".plan_data_lookup_idx` {
				t.Fatalf("inspection identity=%+v", got)
			}
			assertPlanFaultQueriesAreCatalogOnly(t, db.queries)
		})
	}
}

func TestInspectPlanFaultState602UsesOnlyLookupColumnOptions(t *testing.T) {
	tests := []struct {
		name       string
		options    string
		optionsErr error
		want       PlanFaultLiveState
	}{
		{name: "default", options: "", want: PlanFaultRestored},
		{name: "unrelated option", options: "compression=middle", want: PlanFaultRestored},
		{name: "fault present", options: "n_distinct=1", want: PlanFaultPresent},
		{name: "fault present among options", options: "compression=middle,n_distinct=1", want: PlanFaultPresent},
		{name: "different override", options: "n_distinct=0.25", want: PlanFaultDrifted},
		{name: "missing column", optionsErr: sql.ErrNoRows, want: PlanFaultUnavailable},
		{name: "catalog unavailable", optionsErr: errors.New("catalog unavailable"), want: PlanFaultUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &planFaultCatalogFake{options: tt.options, optionsErr: tt.optionsErr}
			got, err := InspectPlanFaultState(
				context.Background(), db, "gsbench", ScenarioCode(602),
			)
			if err != nil {
				t.Fatalf("InspectPlanFaultState() error = %v", err)
			}
			if got.State != tt.want {
				t.Fatalf("inspection=%+v want state=%s", got, tt.want)
			}
			if got.Code != ScenarioCode(602) || got.Object != `"gsbench".plan_data.lookup_key` {
				t.Fatalf("inspection identity=%+v", got)
			}
			assertPlanFaultQueriesAreCatalogOnly(t, db.queries)
		})
	}
}

func TestInspectPlanFaultStateRejectsUnsupportedOrUnsafeInput(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		code   ScenarioCode
	}{
		{name: "unsupported scenario", schema: "gsbench", code: ScenarioCode(603)},
		{name: "unsafe schema", schema: `gsbench'; DROP SCHEMA public; --`, code: ScenarioCode(601)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &planFaultCatalogFake{}
			if _, err := InspectPlanFaultState(
				context.Background(), db, tt.schema, tt.code,
			); err == nil {
				t.Fatal("InspectPlanFaultState() error = nil")
			}
			if len(db.queries) != 0 {
				t.Fatalf("unsafe inspection issued queries: %q", db.queries)
			}
		})
	}
}

func assertPlanFaultQueriesAreCatalogOnly(t *testing.T, queries []string) {
	t.Helper()
	if len(queries) == 0 {
		t.Fatal("inspection issued no catalog queries")
	}
	for _, query := range queries {
		lower := strings.ToLower(query)
		if strings.Contains(lower, "meta_runs") ||
			strings.Contains(lower, "meta_journal") ||
			strings.Contains(lower, "explain") ||
			strings.Contains(lower, "statement_history") ||
			strings.Contains(lower, "insert ") ||
			strings.Contains(lower, "update ") ||
			strings.Contains(lower, "delete ") ||
			strings.Contains(lower, "alter ") ||
			strings.Contains(lower, "drop ") ||
			strings.Contains(lower, "create ") {
			t.Fatalf("inspection issued non-catalog query %q", query)
		}
	}
}
