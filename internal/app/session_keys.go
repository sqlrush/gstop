package app

import (
	"gstop/internal/model"
	"gstop/internal/tui"
)

// runSessionKeys implements the session selection sub-view state machine, a port
// of handle_session_related_keys. The cursor row is tracked here (as the Python
// code tracked it via the hardware cursor) and handed to the panel so it can
// highlight the selected row and resolve terminate targets.
func (a *App) runSessionKeys() {
	cursorY, cursorX := model.SessionCursorYStart, model.SessionCursorXStart
	_, screenH := a.screen.Size()
	screenW, _ := a.screen.Size()

	for {
		key, ok := a.screen.GetKey(tui.BlockForever)
		if !ok {
			continue
		}
		a.screen.FlushInput()
		moveStep := minInt(a.session.Height()-1, screenH-model.SessionCursorYStart-1)

		if a.handleSessionKey(key, &cursorY, &cursorX, moveStep, screenH, screenW) {
			return
		}

		a.session.SetCursor(cursorY)
		a.redrawSession()
	}
}

// handleSessionKey applies one keypress. It returns true when the sub-view should
// exit (any unhandled key).
func (a *App) handleSessionKey(key tui.Key, cursorY, cursorX *int, moveStep, screenH, screenW int) bool {
	topRow := model.SessionCursorYStart + 1
	bottomRow := minInt(model.SessionCursorYStart+a.session.PadLength(), screenH-1)
	maxSelectable := minInt(a.session.Height()+model.SessionCursorYStart-1, screenH-1)

	switch {
	case key.IsRune('s'):
		// stay in the sub-view
	case key.Kind == tui.KeyUp:
		if *cursorY == topRow {
			if a.session.CheckHighlightLocation(-1, moveStep) {
				*cursorY = maxSelectable
			}
		} else {
			*cursorY--
		}
	case key.Kind == tui.KeyDown:
		if *cursorY == bottomRow {
			if a.session.CheckHighlightLocation(1, moveStep) {
				*cursorY = topRow
			}
		} else {
			*cursorY++
		}
	case key.Kind == tui.KeyLeft && *cursorX > 0:
		*cursorX--
	case key.Kind == tui.KeyRight && *cursorX < screenW-1:
		*cursorX++
	case key.IsRune('n'):
		if a.session.CheckHighlightLocation(1, moveStep) {
			*cursorY = topRow
		}
	case key.IsRune('N'):
		if a.session.CheckHighlightLocation(-1, moveStep) {
			*cursorY = maxSelectable
		}
	case key.IsRune('p'):
		a.session.PrintMoreDetails(a.screen, *cursorY)
	case key.IsRune('k') && a.supportTerminate():
		a.session.TerminateSelected(a.screen, *cursorY)
	case key.IsRune('K') && a.supportTerminate():
		a.session.TerminateAll(a.screen, *cursorY)
	case key.IsRune('t'):
		a.session.RefreshByElapsedTime()
	case key.IsRune('m'):
		a.session.RefreshByPGA()
	case key.IsRune('e'):
		a.session.RefreshByEvent()
	default:
		a.session.SetCursor(-1)
		a.session.ResetPrintLocation()
		return true
	}
	return false
}

// redrawSession redraws only the session panel, as the sub-view does while the
// other (frozen) panels stay as last drawn.
func (a *App) redrawSession() {
	a.session.Draw(a.screen.Raw())
	a.screen.Show()
}

func (a *App) supportTerminate() bool {
	return a.cfg.GetBool("main.support_terminate", false)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
