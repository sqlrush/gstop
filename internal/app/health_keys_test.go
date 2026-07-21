package app

import (
	"testing"

	"gstop/internal/healthdash"
	"gstop/internal/tui"
)

func TestHealthStateSelectsSQLAndReturnsActions(t *testing.T) {
	view := healthdash.NewView(100)
	view.Render(healthdash.Snapshot{
		MemoryEnabled: false,
		AverageSQL: []healthdash.SQLMetric{
			{SQLID: 1, Query: "one"},
			{SQLID: 2, Query: "two"},
		},
	}, -1, false)
	state := healthViewState{selected: -1}

	if action := state.handleKey(tui.Key{Kind: tui.KeyRune, Rune: 's'}, view, 8); action != healthStay {
		t.Fatalf("s action = %v", action)
	}
	if !state.selecting || state.selected != 0 {
		t.Fatalf("selection state = %+v", state)
	}
	state.handleKey(tui.Key{Kind: tui.KeyDown}, view, 8)
	if state.selected != 1 {
		t.Fatalf("selected = %d, want 1", state.selected)
	}
	if action := state.handleKey(tui.Key{Kind: tui.KeyRune, Rune: 'p'}, view, 8); action != healthOpenDetail {
		t.Fatalf("p action = %v, want open detail", action)
	}
	if action := state.handleKey(tui.Key{Kind: tui.KeyRune, Rune: 'r'}, view, 8); action != healthRefreshSlow {
		t.Fatalf("r action = %v, want refresh slow", action)
	}
}

func TestHealthStateEscapeReturnsOneLevelAtATime(t *testing.T) {
	view := healthdash.NewView(100)
	view.Render(healthdash.Snapshot{}, -1, false)
	state := healthViewState{detail: &healthdash.Detail{SQLID: 1}, selected: -1}

	if action := state.handleKey(tui.Key{Kind: tui.KeyEscape}, view, 8); action != healthStay || state.detail != nil {
		t.Fatalf("escape from detail = action %v state %+v", action, state)
	}
	if action := state.handleKey(tui.Key{Kind: tui.KeyEscape}, view, 8); action != healthExit {
		t.Fatalf("escape from dashboard = %v, want exit health", action)
	}
}

func TestHealthStateArrowsScrollOutsideSelectionMode(t *testing.T) {
	view := healthdash.NewView(100)
	view.Render(healthdash.Snapshot{
		MemoryEnabled: false,
		AnalyzeHistory: []healthdash.AnalyzeRecord{
			{Database: "a"}, {Database: "b"}, {Database: "c"}, {Database: "d"},
		},
	}, -1, false)
	state := healthViewState{selected: -1}

	state.handleKey(tui.Key{Kind: tui.KeyDown}, view, 5)
	if state.scroll != 1 {
		t.Fatalf("scroll = %d, want 1", state.scroll)
	}
	state.handleKey(tui.Key{Kind: tui.KeyUp}, view, 5)
	if state.scroll != 0 {
		t.Fatalf("scroll = %d, want 0", state.scroll)
	}
}
