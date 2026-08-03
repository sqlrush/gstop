package dbcompat

import "testing"

func TestDetect(t *testing.T) {
	cases := []struct {
		version string
		want    Kind
	}{
		{"PostgreSQL 9.2.4 (openGauss-lite 5.0.3 build 89d144c2) compiled at ...", KindOpenGauss},
		{"(openGauss 5.0.0 build ...) ", KindOpenGauss},
		{"MogDB 5.0.5 ...", KindOpenGauss},
		{"Vastbase G100 ...", KindOpenGauss},
		{"GaussDB Kernel V500R... 503.1.0.SPC0300", KindGaussDB},
		{"503.1.0.SPCXXX GaussDB", KindGaussDB},
		{"PostgreSQL 12.0", KindUnknown},
	}
	for _, tc := range cases {
		if got := Detect(tc.version); got != tc.want {
			t.Errorf("Detect(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestVariant(t *testing.T) {
	// openGauss with an override uses the override; everything else uses gauss.
	if got := Variant(KindOpenGauss, "gauss", "og"); got != "og" {
		t.Errorf("openGauss variant = %q, want og", got)
	}
	if got := Variant(KindOpenGauss, "gauss", ""); got != "gauss" {
		t.Errorf("openGauss with no override = %q, want gauss", got)
	}
	if got := Variant(KindGaussDB, "gauss", "og"); got != "gauss" {
		t.Errorf("GaussDB variant = %q, want gauss", got)
	}
	if got := Variant(KindUnknown, "gauss", "og"); got != "gauss" {
		t.Errorf("unknown variant = %q, want gauss (default)", got)
	}
}

func TestSupportsGsGetExplain(t *testing.T) {
	if SupportsGsGetExplain(KindOpenGauss) {
		t.Error("openGauss should not support gs_get_explain")
	}
	if !SupportsGsGetExplain(KindGaussDB) {
		t.Error("GaussDB should support gs_get_explain")
	}
	if !SupportsGsGetExplain(KindUnknown) {
		t.Error("unknown should default to supported (GaussDB behaviour)")
	}
}

func TestSafeExplainStatementAllowsOnePlannableStatement(t *testing.T) {
	for _, query := range []string{
		"select * from t where id = 1;",
		"select now()::date",
		"with x as (select 1) select * from x",
		"update t set value = 1 where id = 2",
	} {
		if statement, err := SafeExplainStatement(query); err != nil || statement == "" {
			t.Errorf("SafeExplainStatement(%q) = %q, %v", query, statement, err)
		}
	}
}

func TestSafeExplainStatementRejectsUnsafeOrUnplannableSQL(t *testing.T) {
	for _, query := range []string{
		"",
		"select 1; delete from important",
		"select * from t where id = $1",
		"select * from t where id = ?",
		"select * from t where id = :value",
		"begin",
		"explain select 1",
	} {
		if _, err := SafeExplainStatement(query); err == nil {
			t.Errorf("SafeExplainStatement(%q) accepted unsafe SQL", query)
		}
	}
}
