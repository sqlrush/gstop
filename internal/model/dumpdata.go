package model

import "strings"

// DumpData is a sparse character matrix keyed by row then column, mirroring the
// Python monitors' dump_data (defaultdict of {y: {x: char}}). Every character a
// monitor draws is recorded here so the persistence and emergency-snapshot code
// can reconstruct exactly what was on screen without re-querying.
type DumpData map[int]map[int]rune

// NewDumpData returns an empty matrix.
func NewDumpData() DumpData { return DumpData{} }

// Set records rune r at (y, x), extending the row as needed.
func (d DumpData) Set(y, x int, r rune) {
	row := d[y]
	if row == nil {
		row = map[int]rune{}
		d[y] = row
	}
	row[x] = r
}

// Clear removes all recorded characters, reusing the backing map.
func (d DumpData) Clear() {
	for k := range d {
		delete(d, k)
	}
}

// Clone returns a deep copy so a caller can retain a snapshot while the monitor
// keeps drawing.
func (d DumpData) Clone() DumpData {
	out := make(DumpData, len(d))
	for y, row := range d {
		cp := make(map[int]rune, len(row))
		for x, r := range row {
			cp[x] = r
		}
		out[y] = cp
	}
	return out
}

// Text renders the matrix to newline-separated lines, rows 0..maxY, each row
// spanning columns 0..maxX of that row with spaces for gaps. Trailing spaces on
// each line are trimmed. Used when persisting a screen snapshot to a log file.
func (d DumpData) Text() string {
	if len(d) == 0 {
		return ""
	}
	maxY := 0
	for y := range d {
		if y > maxY {
			maxY = y
		}
	}

	var b strings.Builder
	for y := 0; y <= maxY; y++ {
		row := d[y]
		if len(row) == 0 {
			b.WriteByte('\n')
			continue
		}
		maxX := 0
		for x := range row {
			if x > maxX {
				maxX = x
			}
		}
		var line strings.Builder
		for x := 0; x <= maxX; x++ {
			if r, ok := row[x]; ok {
				if r != 0 { // zero marks the continuation column of a wide rune.
					line.WriteRune(r)
				}
			} else {
				line.WriteByte(' ')
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}
