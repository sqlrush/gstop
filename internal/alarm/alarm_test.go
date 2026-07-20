package alarm

import (
	"testing"
	"time"

	"gstop/internal/config"
)

func testAlarm(t *testing.T) (*Alarm, *time.Time) {
	t.Helper()
	cfg := config.FromMap(map[string]any{
		"alarm": map[string]any{
			"observation_window": int64(600),
			"report_thresh":      int64(3),
			"suppression_window": int64(600),
		},
	})
	clock := time.Unix(1_700_000_000, 0)
	a := New(cfg)
	a.now = func() time.Time { return clock }
	return a, &clock
}

func TestConsecutiveThreshold(t *testing.T) {
	a, clock := testAlarm(t)
	// First two occurrences within the window do not report.
	if a.ShouldReport("cpu", true) {
		t.Fatal("1st occurrence should not report")
	}
	*clock = clock.Add(1 * time.Second)
	if a.ShouldReport("cpu", true) {
		t.Fatal("2nd occurrence should not report")
	}
	*clock = clock.Add(1 * time.Second)
	if !a.ShouldReport("cpu", true) {
		t.Fatal("3rd occurrence should report (thresh=3)")
	}
	// After reporting, suppression window silences further occurrences.
	*clock = clock.Add(1 * time.Second)
	if a.ShouldReport("cpu", true) {
		t.Fatal("suppressed right after report")
	}
}

func TestObservationWindowExpiry(t *testing.T) {
	a, clock := testAlarm(t)
	a.ShouldReport("io", true) // t0
	*clock = clock.Add(601 * time.Second)
	// The t0 occurrence has aged past the 600s observation window, so this is
	// treated as the first fresh occurrence and must not report.
	if a.ShouldReport("io", true) {
		t.Fatal("stale occurrence should have been pruned")
	}
}

func TestNonConsecutiveReportsImmediately(t *testing.T) {
	a, clock := testAlarm(t)
	if !a.ShouldReport("stall", false) {
		t.Fatal("non-consecutive should report on first occurrence")
	}
	// but still suppressed within the suppression window
	*clock = clock.Add(10 * time.Second)
	if a.ShouldReport("stall", false) {
		t.Fatal("should be suppressed within window")
	}
	*clock = clock.Add(600 * time.Second)
	if !a.ShouldReport("stall", false) {
		t.Fatal("should report again after suppression window")
	}
}

func TestCleanExpiredKeys(t *testing.T) {
	a, clock := testAlarm(t)
	a.ShouldReport("k", false) // records lastReported
	*clock = clock.Add(1300 * time.Second)
	a.CleanExpiredKeys()
	a.mu.Lock()
	_, present := a.lastReported["k"]
	a.mu.Unlock()
	if present {
		t.Fatal("expired key should be cleaned")
	}
}
