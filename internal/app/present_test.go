package app

import "testing"

type syncSpy struct {
	calls int
}

func (s *syncSpy) Sync() {
	s.calls++
}

func TestPresentMainFrameForcesPhysicalSync(t *testing.T) {
	spy := &syncSpy{}

	presentMainFrame(spy)

	if spy.calls != 1 {
		t.Fatalf("Sync calls = %d, want 1", spy.calls)
	}
}
