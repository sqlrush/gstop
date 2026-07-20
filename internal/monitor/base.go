package monitor

import (
	"strconv"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/config"
	"gstop/internal/model"
	"gstop/internal/tui"
)

// base carries the layout, pad, lock, and dependencies shared by every panel,
// reproducing the fields and helper methods of the Python Monitor base class.
// Concrete panels embed it.
type base struct {
	name   string
	height int
	width  int
	beginX int
	beginY int
	pad    *tui.Pad
	mu     sync.Mutex
	deps   Deps
}

func newBase(name string, height int, deps Deps) base {
	return base{name: name, height: height, deps: deps}
}

// Name returns the panel name.
func (b *base) Name() string { return b.name }

// Height returns the panel's row count.
func (b *base) Height() int { return b.height }

// Init records the panel's position and allocates its pad. The pad is always
// created (even in daemon mode) so the dump snapshot stays available for
// persistence; it is simply never blitted without a screen.
func (b *base) Init(beginX, beginY, width int) {
	b.beginX = beginX
	b.beginY = beginY
	b.width = width
	b.pad = tui.NewPad(b.height, width)
}

// SetVisible toggles whether the panel is blitted, implementing the
// print_to_screen flag used to switch to the memory view.
func (b *base) SetVisible(v bool) {
	if b.pad != nil {
		b.pad.SetVisible(v)
	}
}

// DumpData returns a copy of the last-drawn screen snapshot.
func (b *base) DumpData() model.DumpData {
	if b.pad == nil {
		return model.NewDumpData()
	}
	return b.pad.DumpData()
}

// blit copies the pad to the screen at the panel's origin.
func (b *base) blit(screen tcell.Screen) {
	if b.pad != nil {
		b.pad.Blit(screen, b.beginX, b.beginY)
	}
}

// terminateSession terminates one backend by pid and session id.
func (b *base) terminateSession(pid, sessionID any) {
	cmd := model.TerminateSessionCmd(pid, sessionID)
	b.deps.Logger.Warning("Exec command: %s", cmd)
	b.deps.DB.NoReturn(cmd)
}

// terminateBackend terminates one backend by pid.
func (b *base) terminateBackend(pid any) {
	cmd := model.TerminateBackendCmd(pid)
	b.deps.Logger.Warning("Exec command: %s", cmd)
	b.deps.DB.NoReturn(cmd)
}

// checkAndReportAlarm reports any monitored value that meets or exceeds a
// configured alarm.<item> threshold, reproducing Monitor.check_and_report_alarm
// including the special parsing of the CONNECTION and THREADPOOL cells.
func (b *base) checkAndReportAlarm(items, values []string) {
	for i, key := range items {
		if i >= len(values) {
			break
		}
		raw := values[i]
		threshold, ok := alarmThreshold(b.deps.Cfg, key)
		if !ok || raw == "" {
			continue
		}

		value, ok := parseAlarmValue(key, raw)
		if !ok || value < threshold {
			continue
		}
		msg := "Gausstop监控指标\"" + key + "\"超过预先设定的阈值，当前值为：" +
			strconv.FormatFloat(value, 'f', -1, 64) + "，阈值为：" +
			strconv.FormatFloat(threshold, 'f', -1, 64)
		b.deps.Alarm.CheckAndReport(b.deps.Logger, key, msg, true)
	}
}

// alarmThreshold looks up alarm.<lower(key)> and returns it as a float.
func alarmThreshold(cfg *config.Config, key string) (float64, bool) {
	v := cfg.Get("alarm." + strings.ToLower(key))
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// parseAlarmValue extracts the numeric part of a monitored cell. CONNECTION and
// THREADPOOL cells embed their value before a '%'.
func parseAlarmValue(key, raw string) (float64, bool) {
	if key == "CONNECTION(c/m)" || key == "THREADPOOL" {
		raw = strings.SplitN(raw, "%", 2)[0]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return f, err == nil
}
