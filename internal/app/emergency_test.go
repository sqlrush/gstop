package app

import "testing"

func TestEmergencyCursorRangeUsesOverlayOrigin(t *testing.T) {
	tests := []struct {
		name                    string
		beginY, panelH, screenH int
		wantStart, wantEnd      int
	}{
		{name: "forty rows", beginY: 20, panelH: 20, screenH: 40, wantStart: 21, wantEnd: 39},
		{name: "sixty three rows", beginY: 43, panelH: 20, screenH: 63, wantStart: 44, wantEnd: 62},
		{name: "one row", beginY: 0, panelH: 20, screenH: 1, wantStart: 0, wantEnd: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := emergencyCursorRange(tt.beginY, tt.panelH, tt.screenH)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("range=(%d,%d), want (%d,%d)", start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}
