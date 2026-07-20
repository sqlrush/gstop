// Package dbcompat makes gstop self-adapt to the connected database kind so users
// need not care whether they run against GaussDB (commercial) or openGauss (open
// source). The GaussDB SQL is the authoritative, production-validated form from
// the original tool; where openGauss diverges, a variant is selected at runtime
// by the kind detected from version().
//
// From an audit of every query the tool runs against openGauss Lite 5.0.3, only
// two divergences exist: the session query's BLOCKER CASE (openGauss enforces
// CASE branch type unity, so the pid branch is cast to text) and gs_get_explain
// (a GaussDB-only runtime-plan function, absent on openGauss). Everything else —
// dbe_perf.*, GS_INSTANCE_TIME, pv_*_memory, pg_thread_wait_status, etc. — is
// shared, so no variant is needed. New divergences are added here.
package dbcompat

import "strings"

// Kind identifies the connected database family.
type Kind int

const (
	// KindUnknown is used before detection; it behaves like GaussDB so the
	// production-validated queries are the default.
	KindUnknown Kind = iota
	KindGaussDB
	KindOpenGauss
)

// String renders the kind for logging.
func (k Kind) String() string {
	switch k {
	case KindGaussDB:
		return "GaussDB"
	case KindOpenGauss:
		return "openGauss"
	default:
		return "unknown"
	}
}

// IsOpenGauss reports whether the kind is openGauss (or a derivative).
func (k Kind) IsOpenGauss() bool { return k == KindOpenGauss }

// Detect classifies the server from its version() string. openGauss and its
// derivatives (MogDB, Vastbase) report "openGauss" in version(); GaussDB reports
// "GaussDB". An unrecognised string is treated as unknown (→ GaussDB defaults).
func Detect(version string) Kind {
	v := strings.ToLower(version)
	switch {
	case strings.Contains(v, "opengauss"), strings.Contains(v, "mogdb"), strings.Contains(v, "vastbase"):
		return KindOpenGauss
	case strings.Contains(v, "gaussdb"):
		return KindGaussDB
	default:
		return KindUnknown
	}
}

// Variant returns the openGauss SQL when connected to openGauss and an override
// is supplied; otherwise the GaussDB (default) SQL. This is the single routing
// primitive call sites use for a diverging query.
func Variant(kind Kind, gauss, openGauss string) string {
	if kind == KindOpenGauss && openGauss != "" {
		return openGauss
	}
	return gauss
}

// SupportsGsGetExplain reports whether the server provides gs_get_explain, used
// for a session's live runtime plan. It exists on GaussDB but not on openGauss,
// where the tool substitutes EXPLAIN of the current SQL (an estimate) plus the
// statement-history plan (which on openGauss carries real actual-time stats when
// resource_track_level=operator).
func SupportsGsGetExplain(kind Kind) bool {
	return kind != KindOpenGauss
}
