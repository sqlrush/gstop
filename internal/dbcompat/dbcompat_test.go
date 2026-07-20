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
