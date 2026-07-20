package app

import (
	"gstop/internal/model"
	"gstop/internal/tui"
)

// runMemoryKeys implements the memory dashboard selection sub-view, a port of
// handle_memory_related_keys: arrow keys move the cursor within the panels and
// k terminates the selected session or thread (when permitted).
func (a *App) runMemoryKeys() {
	cursorY, cursorX := model.MemoryCursorYStart, model.MemoryCursorXStart
	screenW, screenH := a.screen.Size()

	for {
		key, ok := a.screen.GetKey(tui.BlockForever)
		if !ok {
			continue
		}
		a.screen.FlushInput()

		if a.handleMemoryKey(key, &cursorY, &cursorX, screenH, screenW) {
			return
		}
		a.memory.SetCursor(cursorY)
		a.memory.Draw(a.screen.Raw())
		a.screen.Show()
	}
}

// handleMemoryKey applies one keypress, returning true to leave the sub-view.
func (a *App) handleMemoryKey(key tui.Key, cursorY, cursorX *int, screenH, screenW int) bool {
	maxRow := minInt(model.MemoryCursorYStart+a.memory.Height(), screenH-1)
	switch {
	case key.IsRune('m'):
		// stay in the sub-view
	case key.Kind == tui.KeyUp:
		if *cursorY > model.MemoryCursorYStart {
			*cursorY--
		}
	case key.Kind == tui.KeyDown:
		if *cursorY < maxRow {
			*cursorY++
		}
	case key.Kind == tui.KeyLeft:
		if *cursorX > 0 {
			*cursorX--
		}
	case key.Kind == tui.KeyRight:
		if *cursorX < screenW-1 {
			*cursorX++
		}
	case key.IsRune('k') && a.supportTerminate():
		a.memory.TerminateSessionOrThread(a.screen, *cursorY)
	default:
		return true
	}
	return false
}
