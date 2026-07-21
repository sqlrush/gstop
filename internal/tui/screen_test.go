package tui

import "testing"

func TestScreenNotifiesGlobalQuitHandler(t *testing.T) {
	screen := &Screen{}
	called := 0
	screen.SetQuitHandler(func() { called++ })
	screen.notifyQuit(Key{Kind: KeyRune, Rune: 'q'})
	screen.notifyQuit(Key{Kind: KeyRune, Rune: 'x'})
	if called != 1 {
		t.Fatalf("quit handler calls = %d, want 1", called)
	}
}
