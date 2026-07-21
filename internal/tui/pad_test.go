package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"

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

func TestPadBlitViewportStartsAtRequestedSourceRow(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(6, 2)

	pad := NewPad(5, 6)
	pad.AddStr(0, 0, "zero", model.Normal)
	pad.AddStr(2, 0, "two", model.Normal)
	pad.AddStr(3, 0, "three", model.Normal)
	pad.BlitViewport(screen, 0, 0, 2)

	if got, _, _, _ := screen.GetContent(0, 0); got != 't' {
		t.Fatalf("screen row 0 starts with %q, want source row 2", got)
	}
	if got, _, _, _ := screen.GetContent(0, 1); got != 't' {
		t.Fatalf("screen row 1 starts with %q, want source row 3", got)
	}
}

func TestPadBlitPreservesEveryWideRune(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(16, 1)

	const text = "历史真实计划"
	pad := NewPad(1, 16)
	pad.AddStr(0, 0, text, model.Normal)
	pad.Blit(screen, 0, 0)

	for i, want := range []rune(text) {
		column := i * 2
		if got, _, _, _ := screen.GetContent(column, 0); got != want {
			t.Fatalf("screen column %d = %q, want %q; wide text was not preserved", column, got, want)
		}
	}
	if got := pad.DumpData().Text(); got != text+"\n" {
		t.Fatalf("dump text = %q, want %q", got, text+"\n")
	}
}
