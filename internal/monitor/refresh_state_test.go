package monitor

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/model"
)

func TestRefreshBadgeText(t *testing.T) {
	cases := []struct {
		phase RefreshPhase
		want  string
	}{
		{RefreshLoading, "[L]"},
		{RefreshRefreshing, "[R]"},
		{RefreshFresh, ""},
		{RefreshTimeout, "[T]"},
		{RefreshError, "[E]"},
	}
	for _, tc := range cases {
		if got := refreshBadge(tc.phase); got != tc.want {
			t.Fatalf("refreshBadge(%s)=%q, want %q", tc.phase, got, tc.want)
		}
	}
}

func TestBaseBadgeUsesExistingHeaderRow(t *testing.T) {
	b := newBase("test", 2, Deps{})
	b.Init(0, 0, 20)
	b.refreshState = RefreshState{
		Phase: RefreshTimeout, LastSuccess: time.Unix(1, 0),
	}
	b.pad.AddStr(0, 0, strings.Repeat(" ", 19), model.Normal)
	b.drawRefreshBadgeLocked()
	if got := b.pad.DumpData().Text(); !strings.Contains(got, "[T]") {
		t.Fatalf("header=%q", got)
	}
	if b.Height() != 2 {
		t.Fatalf("badge changed panel height=%d", b.Height())
	}
}

type drawBadgeMonitor interface {
	Draw(tcell.Screen)
	DumpData() model.DumpData
	Height() int
	SetRefreshState(RefreshState)
}

func TestResidentMonitorDrawBadgesClearOnFreshRedrawWithoutGeometryChange(t *testing.T) {
	const width = 40
	deps := Deps{}

	db := &DBMonitor{
		base: newBase(dbName, dbHeight, deps), items: []string{"DB"},
		widths: []int{10}, values: []string{"ok"},
	}
	db.base.Init(0, 0, width)
	instance := &InstanceMonitor{
		base: newBase(instanceName, instanceHeight, deps), items: []string{"TPS"},
		widths: []int{10}, values: []string{"1"},
	}
	instance.base.Init(0, 0, width)
	event := &EventMonitor{
		base: newBase(eventName, eventHeight, deps), items: []string{"EVENT"},
		widths: []int{10}, currImmediate: true,
	}
	event.base.Init(0, 0, width)
	session := &SessionMonitor{
		base: newBase(sessionName, sessionHeight, deps), items: []string{"PID"},
		widths: []int{10},
	}
	session.base.Init(0, 0, width)
	osMonitor := &OSMonitor{
		base: newBase(osName, osHeight, deps), items: []string{"CPU"},
		widths: []int{10}, values: []string{"2"},
	}
	osMonitor.base.Init(0, 0, width)

	cases := []struct {
		name    string
		monitor drawBadgeMonitor
	}{
		{"db", db},
		{"instance", instance},
		{"event", event},
		{"session", session},
		{"os", osMonitor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			height := tc.monitor.Height()
			tc.monitor.SetRefreshState(RefreshState{Phase: RefreshError})
			tc.monitor.Draw(nil)
			if got := tc.monitor.DumpData().Text(); !strings.Contains(got, "[E]") {
				t.Fatalf("error draw missing badge: %q", got)
			}

			tc.monitor.SetRefreshState(RefreshState{Phase: RefreshFresh})
			tc.monitor.Draw(nil)
			if got := tc.monitor.DumpData().Text(); strings.Contains(got, "[E]") {
				t.Fatalf("fresh redraw retained badge: %q", got)
			}
			if tc.monitor.Height() != height {
				t.Fatalf("draw changed height from %d to %d", height, tc.monitor.Height())
			}
		})
	}
}
