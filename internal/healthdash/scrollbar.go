package healthdash

// ScrollbarGeometry describes rows inside a zero-based terminal viewport.
type ScrollbarGeometry struct {
	Visible    bool
	Arrows     bool
	TrackStart int
	TrackEnd   int
	ThumbStart int
	ThumbEnd   int
}

// ComputeScrollbar returns proportional, clamped scrollbar geometry. Viewports
// of three or more rows reserve the first and last row for arrow markers.
func ComputeScrollbar(documentHeight, viewportHeight, scroll int) ScrollbarGeometry {
	if documentHeight <= viewportHeight || documentHeight <= 0 || viewportHeight <= 0 {
		return ScrollbarGeometry{}
	}
	geometry := ScrollbarGeometry{Visible: true}
	trackHeight := viewportHeight
	if viewportHeight >= 3 {
		geometry.Arrows = true
		geometry.TrackStart = 1
		trackHeight = viewportHeight - 2
	}
	geometry.TrackEnd = geometry.TrackStart + trackHeight - 1

	thumbHeight := trackHeight * viewportHeight / documentHeight
	if thumbHeight < 1 {
		thumbHeight = 1
	}
	if thumbHeight > trackHeight {
		thumbHeight = trackHeight
	}
	maxScroll := documentHeight - viewportHeight
	if scroll < 0 {
		scroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	travel := trackHeight - thumbHeight
	geometry.ThumbStart = geometry.TrackStart
	if travel > 0 && maxScroll > 0 {
		geometry.ThumbStart += scroll * travel / maxScroll
	}
	geometry.ThumbEnd = geometry.ThumbStart + thumbHeight - 1
	return geometry
}
