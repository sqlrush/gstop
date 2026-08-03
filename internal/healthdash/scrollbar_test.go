package healthdash

import "testing"

func TestComputeScrollbarGeometry(t *testing.T) {
	tests := []struct {
		name                       string
		document, viewport, scroll int
		visible                    bool
		trackStart, trackEnd       int
		thumbStart, thumbEnd       int
	}{
		{name: "exact fit hidden", document: 20, viewport: 20, visible: false},
		{name: "top", document: 100, viewport: 10, scroll: 0, visible: true, trackStart: 1, trackEnd: 8, thumbStart: 1, thumbEnd: 1},
		{name: "middle", document: 100, viewport: 10, scroll: 45, visible: true, trackStart: 1, trackEnd: 8, thumbStart: 4, thumbEnd: 4},
		{name: "bottom and high clamp", document: 100, viewport: 10, scroll: 999, visible: true, trackStart: 1, trackEnd: 8, thumbStart: 8, thumbEnd: 8},
		{name: "negative clamp", document: 100, viewport: 10, scroll: -8, visible: true, trackStart: 1, trackEnd: 8, thumbStart: 1, thumbEnd: 1},
		{name: "one row viewport", document: 10, viewport: 1, scroll: 9, visible: true, trackStart: 0, trackEnd: 0, thumbStart: 0, thumbEnd: 0},
		{name: "proportional thumb", document: 20, viewport: 10, scroll: 10, visible: true, trackStart: 1, trackEnd: 8, thumbStart: 5, thumbEnd: 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeScrollbar(tt.document, tt.viewport, tt.scroll)
			if got.Visible != tt.visible || got.TrackStart != tt.trackStart || got.TrackEnd != tt.trackEnd || got.ThumbStart != tt.thumbStart || got.ThumbEnd != tt.thumbEnd {
				t.Fatalf("geometry=%+v, want visible=%v track=%d..%d thumb=%d..%d", got, tt.visible, tt.trackStart, tt.trackEnd, tt.thumbStart, tt.thumbEnd)
			}
		})
	}
}
