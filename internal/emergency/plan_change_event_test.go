package emergency

import (
	"testing"
	"time"

	"gstop/internal/config"
)

func TestPlanChangeEventLifecycleRetainsRecoveredHistory(t *testing.T) {
	first := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	s := &PlanChange{}
	previous := SQLInfo{UniqueSQLID: 88, SQLAcsCnt: 2, SQLLatency: 1_000}
	current := SQLInfo{UniqueSQLID: 88, SQLAcsCnt: 9, SQLLatency: 4_000}

	s.recordPlanChangeEvent(first, current, previous, "select * from orders")
	s.recordPlanChangeEvent(first.Add(time.Minute), SQLInfo{UniqueSQLID: 88, SQLAcsCnt: 11, SQLLatency: 5_000}, previous, "select * from orders")

	events := s.Events()
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one retained event", events)
	}
	event := events[0]
	if !event.FirstSeen.Equal(first) || !event.LastSeen.Equal(first.Add(time.Minute)) {
		t.Fatalf("event times = first %v last %v", event.FirstSeen, event.LastSeen)
	}
	if event.PreviousAcs != 2 || event.CurrentAcs != 11 || event.PreviousLatUS != 1_000 || event.CurrentLatUS != 5_000 {
		t.Fatalf("event values = %+v", event)
	}
	if event.Recovered {
		t.Fatal("event unexpectedly recovered")
	}

	recoveredAt := first.Add(2 * time.Minute)
	s.recordPlanChangeRecovered(88, recoveredAt)
	events = s.Events()
	if len(events) != 1 || !events[0].Recovered || !events[0].RecoveredAt.Equal(recoveredAt) {
		t.Fatalf("recovered event = %+v", events)
	}

	// Events must be an immutable copy.
	events[0].Query = "mutated"
	if s.Events()[0].Query != "select * from orders" {
		t.Fatal("Events exposed mutable internal state")
	}

	// A later regression of the same SQL is a new event; the recovered incident
	// must remain in startup history.
	s.recordPlanChangeEvent(recoveredAt.Add(time.Minute), current, previous, "select * from orders")
	events = s.Events()
	if len(events) != 2 || events[0].Recovered || !events[1].Recovered {
		t.Fatalf("re-trigger history = %+v, want new active + retained recovered event", events)
	}
}

func TestPlanChangeNotificationsCanBeDisabledForHealthOnlyMode(t *testing.T) {
	cfg := config.FromMap(map[string]any{"main": map[string]any{"support_emergency_command": true}})
	s := &PlanChange{Base: NewBase(planChangeName, planChangeHeader, Deps{Cfg: cfg}, 0)}
	s.SetNotificationsEnabled(false)

	// A health-only detector has no Alarm dependency. reportAlarm must return
	// before trying to emit an alarm or remediation command.
	s.reportAlarm(99, "select 99", "")
}
