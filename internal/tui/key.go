package tui

import "github.com/gdamore/tcell/v2"

// KeyKind classifies a keypress the way the original code branched on curses key
// codes: printable runes plus the handful of navigation and editing keys.
type KeyKind int

const (
	// KeyOther is any key the app does not act on.
	KeyOther KeyKind = iota
	KeyRune
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyEnter
	KeyBackspace
	KeyEscape
)

// Key is a normalised keypress. For KeyRune, Rune holds the character.
type Key struct {
	Kind KeyKind
	Rune rune
}

// IsRune reports whether the key is the given character.
func (k Key) IsRune(r rune) bool { return k.Kind == KeyRune && k.Rune == r }

// mapKey converts a tcell key event into a normalised Key.
func mapKey(ev *tcell.EventKey) Key {
	switch ev.Key() {
	case tcell.KeyRune:
		return Key{Kind: KeyRune, Rune: ev.Rune()}
	case tcell.KeyUp:
		return Key{Kind: KeyUp}
	case tcell.KeyDown:
		return Key{Kind: KeyDown}
	case tcell.KeyLeft:
		return Key{Kind: KeyLeft}
	case tcell.KeyRight:
		return Key{Kind: KeyRight}
	case tcell.KeyEnter:
		return Key{Kind: KeyEnter}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		return Key{Kind: KeyBackspace}
	case tcell.KeyEscape:
		return Key{Kind: KeyEscape}
	default:
		return Key{Kind: KeyOther}
	}
}
