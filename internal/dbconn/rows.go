package dbconn

import (
	"strconv"
	"strings"
	"time"
)

// Row is one result row as a slice of column values. Values are the driver's
// native Go types (int64, float64, bool, string, []byte, time.Time, or nil).
// The typed accessors below normalise them for the monitor formatting code,
// which in the original Python operated on dynamically typed tuples.
type Row []any

// Col returns the value at index i, or nil if out of range.
func (r Row) Col(i int) any {
	if i < 0 || i >= len(r) {
		return nil
	}
	return r[i]
}

// Str returns column i as a string. []byte is decoded as UTF-8; nil becomes "".
func (r Row) Str(i int) string {
	return toString(r.Col(i))
}

// Int returns column i as int64 and whether the conversion succeeded.
func (r Row) Int(i int) (int64, bool) {
	switch v := r.Col(i).(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	case []byte:
		return parseInt(string(v))
	case string:
		return parseInt(v)
	}
	return 0, false
}

// Float returns column i as float64 and whether the conversion succeeded.
func (r Row) Float(i int) (float64, bool) {
	switch v := r.Col(i).(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	case []byte:
		return parseFloat(string(v))
	case string:
		return parseFloat(v)
	}
	return 0, false
}

// Time returns column i as time.Time and whether it held a timestamp.
func (r Row) Time(i int) (time.Time, bool) {
	if t, ok := r.Col(i).(time.Time); ok {
		return t, true
	}
	return time.Time{}, false
}

// IsNull reports whether column i is SQL NULL.
func (r Row) IsNull(i int) bool {
	return r.Col(i) == nil
}

func toString(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case []byte:
		return string(s)
	case bool:
		if s {
			return "t"
		}
		return "f"
	case int64:
		return strconv.FormatInt(s, 10)
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	case time.Time:
		return s.Format("2006-01-02 15:04:05")
	default:
		return ""
	}
}

func parseInt(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f), true
	}
	return 0, false
}

func parseFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}
