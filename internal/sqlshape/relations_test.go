package sqlshape

import (
	"reflect"
	"testing"
)

func TestSchemaQualifiedRelationsExtractsQuotedFromAndAlias(t *testing.T) {
	got, err := SchemaQualifiedRelations(`
		SELECT p.id
		FROM "S.dot"."T""x" AS "P"
		JOIN analytics.events e ON e.id = p.id
	`)
	if err != nil {
		t.Fatalf("SchemaQualifiedRelations() error = %v", err)
	}
	want := []RelationRef{
		{
			Schema: Identifier{Value: "S.dot", Quoted: true},
			Table:  Identifier{Value: `T"x`, Quoted: true},
			Alias:  &Identifier{Value: "P", Quoted: true},
		},
		{
			Schema: Identifier{Value: "analytics"},
			Table:  Identifier{Value: "events"},
			Alias:  &Identifier{Value: "e"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SchemaQualifiedRelations() = %#v, want %#v", got, want)
	}
}

func TestSchemaQualifiedRelationsIgnoresNonCodeAndExpressionFrom(t *testing.T) {
	sqlText := `
		-- FROM line_comment.plan_data
		SELECT E'FROM escape_literal.plan_data',
		       'FROM literal.plan_data',
		       $body$FROM dollar_body.plan_data$body$,
		       substring(payload FROM fake.plan_data),
		       extract(day FROM fake.created_at)
		/* outer FROM block.plan_data /* nested FROM nested.plan_data */ still ignored */
		FROM real_schema.plan_data p
	`
	got, err := SchemaQualifiedRelations(sqlText)
	if err != nil {
		t.Fatalf("SchemaQualifiedRelations() error = %v", err)
	}
	want := []RelationRef{{
		Schema: Identifier{Value: "real_schema"},
		Table:  Identifier{Value: "plan_data"},
		Alias:  &Identifier{Value: "p"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SchemaQualifiedRelations() = %#v, want %#v", got, want)
	}
}

func TestSchemaQualifiedRelationsCoversRelationBearingStatements(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []RelationRef
	}{
		{name: "update", sql: `UPDATE Sales.Orders AS o SET total=1`, want: relation("sales", "orders", "o")},
		{name: "insert", sql: `INSERT INTO Sales.Orders(id) VALUES (1)`, want: relation("sales", "orders", "")},
		{name: "delete", sql: `DELETE FROM Sales.Orders o WHERE o.id=1`, want: relation("sales", "orders", "o")},
		{name: "merge", sql: `MERGE INTO Sales.Orders o USING Stage.Orders s ON o.id=s.id WHEN MATCHED THEN UPDATE SET total=1`, want: append(relation("sales", "orders", "o"), relation("stage", "orders", "s")...)},
		{name: "analyze", sql: `ANALYZE Sales.Orders`, want: relation("sales", "orders", "")},
		{name: "vacuum", sql: `VACUUM Sales.Orders`, want: relation("sales", "orders", "")},
		{name: "comma from", sql: `SELECT * FROM A.Plan_Data p, B.Plan_Data q`, want: append(relation("a", "plan_data", "p"), relation("b", "plan_data", "q")...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SchemaQualifiedRelations(tt.sql)
			if err != nil {
				t.Fatalf("SchemaQualifiedRelations() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SchemaQualifiedRelations() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSchemaQualifiedRelationsReturnsErrorForMalformedSQL(t *testing.T) {
	for _, sqlText := range []string{
		`SELECT 'unterminated FROM fake.plan_data`,
		`SELECT "unterminated FROM fake.plan_data`,
		`SELECT $tag$unterminated FROM fake.plan_data`,
		`SELECT 1 /* unterminated FROM fake.plan_data`,
	} {
		if got, err := SchemaQualifiedRelations(sqlText); err == nil {
			t.Fatalf("SchemaQualifiedRelations(%q) = %#v, nil; want error", sqlText, got)
		}
	}
}

func TestSchemaQualifiedRelationsRejectsUnprovableStatementScope(t *testing.T) {
	for _, sqlText := range []string{
		`SELECT * FROM plan_data WHERE EXISTS (SELECT 1 FROM s.plan_data)`,
		`SELECT * FROM s.plan_data; SELECT * FROM other.plan_data`,
		`UPDATE plan_data SET payload='x' FROM s.plan_data`,
	} {
		if got, err := SchemaQualifiedRelations(sqlText); err == nil {
			t.Fatalf("SchemaQualifiedRelations(%q) = %#v, nil; want fail-closed error", sqlText, got)
		}
	}

	got, err := SchemaQualifiedRelations(`SELECT * FROM s.plan_data;`)
	if err != nil {
		t.Fatalf("single statement with trailing semicolon error = %v", err)
	}
	want := relation("s", "plan_data", "")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trailing semicolon relations = %#v, want %#v", got, want)
	}
}

func TestRelationEvidenceSeparatesQualifiedAndUnqualifiedRelations(t *testing.T) {
	evidence, err := RelationEvidenceFor(
		`SELECT * FROM s.plan_data p JOIN dimensions d ON d.id=p.id`,
	)
	if err != nil {
		t.Fatalf("RelationEvidenceFor() error = %v", err)
	}
	wantQualified := relation("s", "plan_data", "p")
	wantUnqualified := []RelationRef{{
		Table: Identifier{Value: "dimensions"},
		Alias: &Identifier{Value: "d"},
	}}
	if !reflect.DeepEqual(evidence.Qualified, wantQualified) ||
		!reflect.DeepEqual(evidence.Unqualified, wantUnqualified) {
		t.Fatalf("RelationEvidenceFor() = %#v, want qualified=%#v unqualified=%#v",
			evidence, wantQualified, wantUnqualified)
	}
}

func TestLeadingKeywordSkipsCommentsAndRejectsMalformedSQL(t *testing.T) {
	for _, tt := range []struct {
		sql  string
		want string
	}{
		{sql: " -- comment\n /* nested /* comment */ done */ ANALYZE s.t", want: "ANALYZE"},
		{sql: "\nVACUUM s.t", want: "VACUUM"},
		{sql: "WITH q AS (SELECT 1) SELECT * FROM q", want: "WITH"},
	} {
		got, err := LeadingKeyword(tt.sql)
		if err != nil {
			t.Fatalf("LeadingKeyword(%q) error = %v", tt.sql, err)
		}
		if got != tt.want {
			t.Fatalf("LeadingKeyword(%q) = %q, want %q", tt.sql, got, tt.want)
		}
	}
	if got, err := LeadingKeyword("/* unterminated"); err == nil {
		t.Fatalf("LeadingKeyword malformed = %q, nil; want error", got)
	}
}

func relation(schema, table, alias string) []RelationRef {
	ref := RelationRef{
		Schema: Identifier{Value: schema},
		Table:  Identifier{Value: table},
	}
	if alias != "" {
		ref.Alias = &Identifier{Value: alias}
	}
	return []RelationRef{ref}
}
