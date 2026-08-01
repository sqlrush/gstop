package sqlshape

import "testing"

func TestSignatureTreatsLiteralsAndBindMarkersAsOneSQLShape(t *testing.T) {
	tests := [][2]string{
		{
			`SELECT * FROM gsbench.fact_sales WHERE id=42 LIMIT 100`,
			`select * from gsbench.fact_sales where id = ? limit ?`,
		},
		{
			`SELECT * FROM gsbench.orders WHERE id=42 AND status='paid'`,
			`select * from gsbench.orders where id=$1 and status=:status`,
		},
		{
			`UPDATE gsbench.accounts SET balance=balance+1 WHERE id=9;`,
			`update GSBENCH.accounts set balance = balance + ? where id = ?`,
		},
	}
	for _, pair := range tests {
		if Signature(pair[0]) != Signature(pair[1]) {
			t.Fatalf("signatures differ:\nleft=%s\nright=%s",
				Canonical(pair[0]), Canonical(pair[1]))
		}
	}
}

func TestCanonicalPreservesQuotedIdentifiersAndOperators(t *testing.T) {
	a := `SELECT "CaseSensitive" FROM t WHERE a::text='x' AND b>=10`
	b := `select "CaseSensitive" from t where a :: text = ? and b >= ?`
	if Signature(a) != Signature(b) {
		t.Fatalf("signatures differ: %s != %s", Canonical(a), Canonical(b))
	}
	different := `SELECT "casesensitive" FROM t WHERE a::text='x' AND b>=10`
	if Signature(a) == Signature(different) {
		t.Fatal("quoted identifier case was discarded")
	}
}

func TestCanonicalDropsCommentsWhitespaceAndTrailingSemicolon(t *testing.T) {
	a := "SELECT /* sample */ 1 -- trailing\nFROM dual;"
	b := "select ? from dual"
	if Canonical(a) != Canonical(b) {
		t.Fatalf("canonical a=%q b=%q", Canonical(a), Canonical(b))
	}
}
