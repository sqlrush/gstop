package tui

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/model"
)

// BlockForever is passed to GetKey to wait indefinitely for a key, the analogue
// of curses timeout(-1).
const BlockForever time.Duration = -1

// Screen wraps a tcell screen as the app's single drawing/input surface, the
// analogue of the curses stdscr. Panels blit their pads onto it, and it delivers
// keypresses with a curses-style timeout.
type Screen struct {
	scr    tcell.Screen
	events chan tcell.Event
	done   chan struct{}
}

// NewScreen initialises the terminal: raw input, hidden cursor, and a background
// event pump. The caller must Close it to restore the terminal.
func NewScreen() (*Screen, error) {
	scr, err := tcell.NewScreen()
	if err != nil {
		return nil, fmt.Errorf("create screen: %w", err)
	}
	if err := scr.Init(); err != nil {
		return nil, fmt.Errorf("init screen: %w", err)
	}
	scr.SetStyle(baseStyle(model.PairNormal))
	scr.HideCursor()
	scr.Clear()

	s := &Screen{
		scr:    scr,
		events: make(chan tcell.Event, 16),
		done:   make(chan struct{}),
	}
	go s.pump()
	return s, nil
}

// pump forwards tcell events to the channel until the screen is finalised.
func (s *Screen) pump() {
	for {
		ev := s.scr.PollEvent()
		if ev == nil {
			return
		}
		select {
		case s.events <- ev:
		case <-s.done:
			return
		}
	}
}

// Close restores the terminal.
func (s *Screen) Close() {
	close(s.done)
	s.scr.Fini()
}

// Size returns the terminal width and height in cells.
func (s *Screen) Size() (width, height int) { return s.scr.Size() }

// Raw exposes the underlying tcell screen so a Pad can blit onto it.
func (s *Screen) Raw() tcell.Screen { return s.scr }

// Clear blanks the screen buffer.
func (s *Screen) Clear() { s.scr.Clear() }

// Show flushes the buffer to the terminal.
func (s *Screen) Show() { s.scr.Show() }

// SetContent writes a single styled rune, used by the inline prompt helpers.
func (s *Screen) SetContent(x, y int, r rune, style model.Style) {
	s.scr.SetContent(x, y, r, nil, toTcell(style))
}

// SetString writes styled text starting at (x, y).
func (s *Screen) SetString(x, y int, text string, style model.Style) {
	col := x
	for _, r := range text {
		s.scr.SetContent(col, y, r, nil, toTcell(style))
		col++
	}
}

// GetKey waits up to timeout for a keypress, returning ok=false on timeout. A
// negative timeout (BlockForever) waits indefinitely. Resize events are absorbed
// and redraw the screen, continuing to wait, matching curses' getch behaviour of
// only returning on real input.
func (s *Screen) GetKey(timeout time.Duration) (Key, bool) {
	var deadline <-chan time.Time
	if timeout >= 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		select {
		case ev := <-s.events:
			switch e := ev.(type) {
			case *tcell.EventKey:
				return mapKey(e), true
			case *tcell.EventResize:
				s.scr.Sync()
				// keep waiting for an actual key
			}
		case <-deadline:
			return Key{}, false
		}
	}
}

// FlushInput drains any buffered input events, matching curses.flushinp used to
// discard type-ahead after handling a command.
func (s *Screen) FlushInput() {
	for {
		select {
		case <-s.events:
		default:
			return
		}
	}
}
