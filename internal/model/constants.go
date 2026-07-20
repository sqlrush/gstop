package model

// Screen layout constants ported verbatim from common/constants.py. They anchor
// the cursor when the corresponding sub-view takes over keyboard input.
const (
	// EmergencyCursorYStart is the first selectable row of the emergency panel.
	EmergencyCursorYStart = 44
	EmergencyCursorXStart = 0

	// MemoryCursorYStart is the first selectable row of the memory dashboard.
	MemoryCursorYStart = 10
	MemoryCursorXStart = 0

	// SessionCursorYStart is the first selectable row of the session panel,
	// set in tool/gstop.py (CURSOR_Y_START = 12).
	SessionCursorYStart = 12
	SessionCursorXStart = 0

	// MonitorWidth is the drawing width shared by every panel (MONITOR_WIDTH+1).
	MonitorWidth = 151
)
