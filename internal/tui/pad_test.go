package tui

import (
	"testing"

	"gstop/internal/model"
)

func TestPadAddStrRecordsDump(t *testing.T) {
	p := NewPad(3, 10)
	p.AddStr(1, 2, "hello", model.Normal)
	dump := p.DumpData()
	row := dump[1]
	if row == nil {
		t.Fatal("row 1 not recorded")
	}
	want := "hello"
	for i, r := range want {
		if row[2+i] != r {
			t.Errorf("dump[1][%d] = %q, want %q", 2+i, row[2+i], r)
		}
	}
}

func TestPadAddStrClips(t *testing.T) {
	p := NewPad(2, 5)
	// starts at column 3, so only "ab" of "abcde" fit in width 5
	p.AddStr(0, 3, "abcde", model.Normal)
	dump := p.DumpData()
	row := dump[0]
	if row[3] != 'a' || row[4] != 'b' {
		t.Errorf("clip failed: got %q,%q", row[3], row[4])
	}
	if _, ok := row[5]; ok {
		t.Error("wrote past pad width")
	}
	// out-of-range row is ignored
	p.AddStr(9, 0, "x", model.Normal)
	if _, ok := p.DumpData()[9]; ok {
		t.Error("wrote out-of-range row")
	}
}

func TestPadClearEmptiesDump(t *testing.T) {
	p := NewPad(2, 5)
	p.AddStr(0, 0, "hi", model.Normal)
	p.Clear()
	if len(p.DumpData()) != 0 {
		t.Error("Clear did not empty dump")
	}
}
