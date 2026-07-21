package tui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"

	"gstop/internal/model"
)

// cell is one rendered character and its style.
type cell struct {
	r            rune
	combining    []rune
	style        model.Style
	continuation bool
}

// Pad is a fixed-size off-screen character buffer, the analogue of a curses pad
// created by curses.newpad. It implements model.Surface, records everything
// drawn into a DumpData snapshot (as create_logged_pad did), and blits its
// contents to a region of the real screen with clipping.
//
// A Pad is not safe for concurrent use; callers serialise draw and blit with the
// owning monitor's lock, exactly as the original held monitor.lock around print.
type Pad struct {
	height, width int
	cells         []cell
	dump          model.DumpData
	toScreen      bool
}

// NewPad returns a blank pad of the given size with drawing-to-screen enabled.
func NewPad(height, width int) *Pad {
	p := &Pad{
		height:   height,
		width:    width,
		cells:    make([]cell, height*width),
		dump:     model.NewDumpData(),
		toScreen: true,
	}
	p.blank()
	return p
}

func (p *Pad) blank() {
	for i := range p.cells {
		p.cells[i] = cell{r: ' ', style: model.Normal}
	}
}

// Height implements model.Surface.
func (p *Pad) Height() int { return p.height }

// Width implements model.Surface.
func (p *Pad) Width() int { return p.width }

// AddStr writes text at (y, x) with style, clipping anything outside the pad,
// and records each visible rune into the dump snapshot. Implements model.Surface.
func (p *Pad) AddStr(y, x int, text string, style model.Style) {
	if y < 0 || y >= p.height {
		return
	}
	col := x
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		runes := graphemes.Runes()
		if len(runes) == 0 {
			continue
		}
		width := graphemes.Width()
		if width < 1 {
			width = 1
		}
		if col < 0 {
			col += width
			continue
		}
		if col+width > p.width {
			break
		}
		combining := append([]rune(nil), runes[1:]...)
		p.cells[y*p.width+col] = cell{r: runes[0], combining: combining, style: style}
		p.dump.Set(y, col, runes[0])
		for offset := 1; offset < width; offset++ {
			p.cells[y*p.width+col+offset] = cell{style: style, continuation: true}
			p.dump.Set(y, col+offset, 0)
		}
		col += width
	}
}

// Clear resets every cell to a blank and empties the dump snapshot, matching
// clear_screen followed by clear_dump_data.
func (p *Pad) Clear() {
	p.blank()
	p.dump.Clear()
}

// SetVisible controls whether Blit draws to the screen, corresponding to the
// print_to_screen flag used to switch between the normal and memory views.
func (p *Pad) SetVisible(v bool) { p.toScreen = v }

// Visible reports the current print_to_screen flag.
func (p *Pad) Visible() bool { return p.toScreen }

// DumpData returns a deep copy of the current screen snapshot.
func (p *Pad) DumpData() model.DumpData { return p.dump.Clone() }

// Blit copies the pad to the screen with its top-left at (beginY, beginX),
// clipped to the screen bounds, reproducing pad.refresh(0,0, begin_y, begin_x,
// begin_y+height-1, begin_x+width-1). It is a no-op when the pad is hidden.
func (p *Pad) Blit(screen tcell.Screen, beginX, beginY int) {
	p.BlitViewport(screen, beginX, beginY, 0)
}

// BlitViewport copies the pad to screen starting at sourceY. It is used by
// scrollable full-screen views whose content is taller than the terminal.
func (p *Pad) BlitViewport(screen tcell.Screen, beginX, beginY, sourceY int) {
	if !p.toScreen || screen == nil {
		return
	}
	if sourceY < 0 {
		sourceY = 0
	}
	screenW, screenH := screen.Size()
	for y := sourceY; y < p.height; y++ {
		sy := beginY + y - sourceY
		if sy < 0 || sy >= screenH {
			continue
		}
		for x := 0; x < p.width; x++ {
			sx := beginX + x
			if sx < 0 || sx >= screenW {
				continue
			}
			c := p.cells[y*p.width+x]
			if c.continuation {
				continue
			}
			screen.SetContent(sx, sy, c.r, c.combining, toTcell(c.style))
		}
	}
}
