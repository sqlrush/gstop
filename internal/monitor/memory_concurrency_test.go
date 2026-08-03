package monitor

import "testing"

func TestMemoryTerminateTargetIsImmutableSnapshot(t *testing.T) {
	m := &MemoryMonitor{}
	m.panels[2].value = [][]any{{int64(11)}}
	m.panels[3].value = [][]any{{int64(33)}}

	sessionTarget, ok := m.memoryTerminateTarget(11)
	if !ok || sessionTarget.kind != memoryTerminateSession ||
		sessionTarget.id != int64(11) {
		t.Fatalf("session target = %+v, ok=%v", sessionTarget, ok)
	}
	threadTarget, ok := m.memoryTerminateTarget(15)
	if !ok || threadTarget.kind != memoryTerminateThread ||
		threadTarget.id != int64(33) {
		t.Fatalf("thread target = %+v, ok=%v", threadTarget, ok)
	}

	m.mu.Lock()
	m.panels[2].value[0][0] = int64(22)
	m.panels[3].value[0][0] = int64(44)
	m.mu.Unlock()

	if sessionTarget.id != int64(11) {
		t.Fatalf("session target changed with panel refresh: %+v", sessionTarget)
	}
	if threadTarget.id != int64(33) {
		t.Fatalf("thread target changed with panel refresh: %+v", threadTarget)
	}
}
