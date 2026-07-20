package monitor

import (
	"strings"

	"gstop/internal/model"
)

// renderAndShow draws all panels and, when the view is active, flushes to the
// screen so the background goroutine's refresh becomes visible, matching the
// Python thread that called print (pad.refresh) on its own cadence.
func (m *MemoryMonitor) renderAndShow() {
	m.render()

	m.mu.Lock()
	screen := m.screen
	visible := m.pad != nil && m.pad.Visible()
	m.mu.Unlock()

	if screen != nil && visible {
		screen.Show()
	}
}

// render redraws all four panels into the pad and blits to the attached screen (a
// no-op when hidden or when no screen is attached). Port of print.
func (m *MemoryMonitor) render() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pad == nil {
		return
	}
	m.pad.Clear()
	y := 0
	for pi := range m.panels {
		y = m.drawPanel(pi, m.panels[pi], y)
	}
	m.blit(m.screen)
}

// drawPanel renders one panel (optional title, header bar, value rows) starting at
// pad row y and returns the next free row after the trailing spacer.
func (m *MemoryMonitor) drawPanel(pi int, p memPanel, y int) int {
	if p.title != "" {
		m.pad.AddStr(y, 0, p.title, model.Normal)
		y++
	}

	m.drawHeaderRow(y, p.header, p.width)
	y++

	for i, row := range p.value {
		style := m.valueStyle(pi, i, y)
		if style.Reverse {
			m.pad.AddStr(y, 0, strings.Repeat(" ", m.width-1), model.Style{Pair: model.PairReverse})
		}
		m.drawValueRow(y, row, p.width, style)
		y++
	}

	y++ // trailing spacer between panels
	return y
}

// drawHeaderRow paints the reverse-video header bar and its truncated column
// labels.
func (m *MemoryMonitor) drawHeaderRow(y int, header []string, widths []int) {
	style := model.Style{Pair: model.PairReverse}
	m.pad.AddStr(y, 0, strings.Repeat(" ", m.width-1), style)
	x := 0
	for i, item := range header {
		if i >= len(widths) {
			break
		}
		m.pad.AddStr(y, x, sTruncate(item, widths[i]-1), style)
		x += widths[i]
	}
}

// drawValueRow paints one row of transposed cells, each truncated to its column
// width and rendered with Python str() semantics via DisplayValue.
func (m *MemoryMonitor) drawValueRow(y int, row []any, widths []int, style model.Style) {
	x := 0
	for j, cell := range row {
		if j >= len(widths) {
			break
		}
		m.pad.AddStr(y, x, sTruncate(model.DisplayValue(cell), widths[j]-1), style)
		x += widths[j]
	}
}

// valueStyle picks the row style: yellow-bold for emergency-highlighted rows
// (all of Panel1 for a dynamic-global full, or the first three rows of Panel2/3
// for a session/thread full) and reverse video for the selected row. The
// selection convention is cursorY == beginY + padRow + 1, matching print's check.
func (m *MemoryMonitor) valueStyle(pi, i, y int) model.Style {
	style := model.Normal
	switch {
	case m.memoryFullType == EmerDynamicGlobalMemoryFull && pi == 1:
		style = model.Style{Pair: model.PairConfirmYellow, Bold: true}
	case m.memoryFullType == EmerSessionThreadMemoryFull && (pi == 2 || pi == 3) && i < 3:
		style = model.Style{Pair: model.PairConfirmYellow, Bold: true}
	}
	if m.cursorY >= 0 && m.cursorY == m.beginY+y+1 {
		style.Reverse = true
	}
	return style
}
