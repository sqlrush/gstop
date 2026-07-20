// Package model holds the shared types and SQL that the monitor, emergency, TUI,
// and persistence layers all depend on. Keeping them here lets the monitors draw
// through the Surface interface without importing the tcell-based tui package,
// avoiding an import cycle between rendering and the things being rendered.
package model

// ColorPair enumerates the seven curses color pairs the original tool defined in
// gstop_main_routine. The tui package maps each to a concrete terminal style.
type ColorPair int

const (
	// PairNormal is white on black: ordinary text.
	PairNormal ColorPair = 1
	// PairReverse is black on white: header bars and the selected-row background.
	PairReverse ColorPair = 2
	// PairAlarmRed is red on black: a lock holder (BLK=H) and alarm text.
	PairAlarmRed ColorPair = 3
	// PairAlarmRedSel is red on white: a selected alarm row.
	PairAlarmRedSel ColorPair = 4
	// PairConfirmYellow is yellow on black: confirm prompts and emergency highlight.
	PairConfirmYellow ColorPair = 5
	// PairGreen is green on black: a lock waiter (BLK=W).
	PairGreen ColorPair = 6
	// PairCyan is cyan on black: a holder-and-waiter (BLK=H&W).
	PairCyan ColorPair = 7
)

// Style is a color pair plus the bold and reverse attributes used by curses
// addstr calls (color_pair(n) | A_BOLD | A_REVERSE).
type Style struct {
	Pair    ColorPair
	Bold    bool
	Reverse bool
}

// Normal is plain white-on-black text.
var Normal = Style{Pair: PairNormal}

// WithBold returns a copy of the style with bold enabled.
func (s Style) WithBold() Style { s.Bold = true; return s }

// WithReverse returns a copy of the style with the reverse attribute enabled.
func (s Style) WithReverse() Style { s.Reverse = true; return s }

// Surface is an addressable drawing target of fixed size. A monitor renders into
// a Surface with cell coordinates; the tui package's Pad implements it and also
// records every character into a DumpData snapshot for persistence.
type Surface interface {
	// AddStr writes text starting at row y, column x with the given style.
	// Writes outside the surface bounds are clipped.
	AddStr(y, x int, text string, style Style)
	// Height returns the surface's row count.
	Height() int
	// Width returns the surface's column count.
	Width() int
}
