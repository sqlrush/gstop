package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestMapKeyMapsPageNavigation(t *testing.T) {
	tests := []struct {
		name string
		key  tcell.Key
		want KeyKind
	}{
		{name: "page up", key: tcell.KeyPgUp, want: KeyPageUp},
		{name: "page down", key: tcell.KeyPgDn, want: KeyPageDown},
		{name: "home", key: tcell.KeyHome, want: KeyHome},
		{name: "end", key: tcell.KeyEnd, want: KeyEnd},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapKey(tcell.NewEventKey(tt.key, 0, tcell.ModNone))
			if got.Kind != tt.want {
				t.Fatalf("mapKey(%v).Kind = %v, want %v", tt.key, got.Kind, tt.want)
			}
		})
	}
}
