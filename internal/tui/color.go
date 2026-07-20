// Package tui implements gstop's terminal UI on top of tcell, reproducing the
// original curses screen: fixed-size off-screen pads per panel, the seven color
// pairs, selected-row reverse video, and paging. Monitors draw through the
// model.Surface interface, so nothing in the monitor layer depends on tcell.
package tui

import (
	"github.com/gdamore/tcell/v2"

	"gstop/internal/model"
)

// baseStyle maps each color pair to its foreground/background, matching the
// curses init_pair calls in gstop_main_routine.
func baseStyle(pair model.ColorPair) tcell.Style {
	fg, bg := tcell.ColorWhite, tcell.ColorBlack
	switch pair {
	case model.PairNormal:
		fg, bg = tcell.ColorWhite, tcell.ColorBlack
	case model.PairReverse:
		fg, bg = tcell.ColorBlack, tcell.ColorWhite
	case model.PairAlarmRed:
		fg, bg = tcell.ColorRed, tcell.ColorBlack
	case model.PairAlarmRedSel:
		fg, bg = tcell.ColorRed, tcell.ColorWhite
	case model.PairConfirmYellow:
		fg, bg = tcell.ColorYellow, tcell.ColorBlack
	case model.PairGreen:
		fg, bg = tcell.ColorGreen, tcell.ColorBlack
	case model.PairCyan:
		fg, bg = tcell.ColorTeal, tcell.ColorBlack
	}
	return tcell.StyleDefault.Foreground(fg).Background(bg)
}

// toTcell converts a model.Style, including the bold and reverse attributes, to
// a concrete tcell.Style.
func toTcell(s model.Style) tcell.Style {
	style := baseStyle(s.Pair)
	if s.Bold {
		style = style.Bold(true)
	}
	if s.Reverse {
		style = style.Reverse(true)
	}
	return style
}
