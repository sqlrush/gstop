package model

import (
	"strconv"
	"strings"
	"time"
)

// SessionRow is one session's monitored data as a 24-element slice, matching the
// Python monitor_value_per_line exactly (dynamically typed, index-addressed).
// The first 14 elements are the displayed columns; 14..23 are hidden fields the
// detail panel, blocking tree, and emergency modules read by index. Keeping the
// slice-of-any shape (rather than a struct) preserves the original's index
// contract that the emergency subsystem depends on.
type SessionRow []any

// Column indices into a SessionRow.
const (
	SIdxPID         = 0  // backend/thread pid
	SIdxUser        = 1  // login user
	SIdxProgram     = 2  // application_name
	SIdxPGA         = 3  // dynamic memory (MB) or 0
	SIdxSQLID       = 4  // unique_sql_id
	SIdxSQL         = 5  // first line of query text
	SIdxOPN         = 6  // operation kind
	SIdxBlocker     = 7  // blocker pid, "" (no block), or nil (blocker not found)
	SIdxElapsedMS   = 8  // query elapsed time (ms)
	SIdxState       = 9  // session state
	SIdxSTE         = 10 // ON CPU / USR I/O / WAITING
	SIdxEvent       = 11 // wait_status
	SIdxSoftParse   = 12 // soft parse rate (%)
	SIdxBLK         = 13 // "", H, W, or H&W
	SIdxSessionID   = 14 // session id (hidden)
	SIdxClientAddr  = 15
	SIdxClientHost  = 16
	SIdxClientPort  = 17
	SIdxDatabase    = 18
	SIdxXactStart   = 19
	SIdxQueryStart  = 20
	SIdxXactRuntime = 21 // transaction elapsed time (us)
	SIdxWaitEvent   = 22
	SIdxBlockSessID = 23

	// SessionRowLen is the number of elements in a fully-populated SessionRow.
	SessionRowLen = 24
)

// Get returns the value at index i, or nil if out of range.
func (r SessionRow) Get(i int) any {
	if i < 0 || i >= len(r) {
		return nil
	}
	return r[i]
}

// Display renders column i the way Python's f-string did: nil -> "None",
// float -> str(float) with a decimal point, everything else naturally.
func (r SessionRow) Display(i int) string {
	return DisplayValue(r.Get(i))
}

// DisplayValue formats a dynamically-typed cell for the screen, matching
// Python's str()/f-string output so columns read identically.
func DisplayValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return floatStr(x)
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05")
	default:
		return ""
	}
}

// floatStr renders a float like Python str(): whole values keep a ".0".
func floatStr(x float64) string {
	s := strconv.FormatFloat(x, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}
