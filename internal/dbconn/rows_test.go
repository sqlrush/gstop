package dbconn

import (
	"testing"
	"time"
)

func TestRowStr(t *testing.T) {
	ts := time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC)
	r := Row{nil, "text", []byte("bytes"), int64(42), 3.5, true, ts}
	cases := []struct {
		idx  int
		want string
	}{
		{0, ""},
		{1, "text"},
		{2, "bytes"},
		{3, "42"},
		{4, "3.5"},
		{5, "t"},
		{6, "2026-07-19 01:02:03"},
		{99, ""}, // out of range
	}
	for _, tc := range cases {
		if got := r.Str(tc.idx); got != tc.want {
			t.Errorf("Str(%d) = %q, want %q", tc.idx, got, tc.want)
		}
	}
}

func TestRowInt(t *testing.T) {
	r := Row{int64(10), 3.9, []byte("55"), "77", "abc", nil}
	cases := []struct {
		idx  int
		want int64
		ok   bool
	}{
		{0, 10, true},
		{1, 3, true}, // float truncates
		{2, 55, true},
		{3, 77, true},
		{4, 0, false},
		{5, 0, false}, // nil
	}
	for _, tc := range cases {
		got, ok := r.Int(tc.idx)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Int(%d) = (%d,%v), want (%d,%v)", tc.idx, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRowFloat(t *testing.T) {
	r := Row{3.5, int64(7), []byte("2.25"), "1.5", "x"}
	cases := []struct {
		idx  int
		want float64
		ok   bool
	}{
		{0, 3.5, true},
		{1, 7, true},
		{2, 2.25, true},
		{3, 1.5, true},
		{4, 0, false},
	}
	for _, tc := range cases {
		got, ok := r.Float(tc.idx)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Float(%d) = (%v,%v), want (%v,%v)", tc.idx, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRowIsNull(t *testing.T) {
	r := Row{nil, "x"}
	if !r.IsNull(0) {
		t.Error("expected col 0 null")
	}
	if r.IsNull(1) {
		t.Error("col 1 should not be null")
	}
	if !r.IsNull(5) {
		t.Error("out-of-range should read as null")
	}
}
