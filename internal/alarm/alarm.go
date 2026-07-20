// Package alarm reports threshold breaches to a local alarm file, with the same
// de-duplication policy as the Python common/alarm.py: within an observation
// window a key must recur report_thresh times before it is reported, and once
// reported it is suppressed for suppression_window. An external collector is
// expected to tail the alarm file; gstop never sends mail or network traffic.
package alarm

import (
	"fmt"
	"os"
	"sync"
	"time"

	"gstop/internal/config"
	"gstop/internal/logging"
)

// windows holds the resolved timing policy, read once from config per call so
// live config edits (in tests) take effect.
type windows struct {
	observation time.Duration
	suppression time.Duration
	reportCount int
}

// Alarm carries de-duplication state and the open alarm-file handle. It is safe
// for concurrent use. Construct with New and call Start before reporting.
type Alarm struct {
	cfg *config.Config
	now func() time.Time

	mu           sync.Mutex
	lastReported map[string]time.Time
	occurrences  map[string][]time.Time

	fileMu   sync.Mutex
	file     *os.File
	hostname string
	enabled  bool
}

// New builds an Alarm bound to cfg. Reporting is inert until Start succeeds.
func New(cfg *config.Config) *Alarm {
	return &Alarm{
		cfg:          cfg,
		now:          time.Now,
		lastReported: map[string]time.Time{},
		occurrences:  map[string][]time.Time{},
	}
}

// Start opens the configured alarm file for appending and caches the hostname.
// An unconfigured alarm_file disables reporting silently (matching the original).
// A configured-but-unopenable file is a hard error.
func (a *Alarm) Start() error {
	path := a.cfg.GetString("alarm.alarm_file", "")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open alarm file %q: %w", path, err)
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	a.fileMu.Lock()
	a.file = f
	a.hostname = host
	a.enabled = true
	a.fileMu.Unlock()
	return nil
}

// Stop closes the alarm file.
func (a *Alarm) Stop() {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	if a.file != nil {
		a.file.Close()
		a.file = nil
	}
	a.enabled = false
}

func (a *Alarm) resolveWindows() windows {
	return windows{
		observation: secondsToDuration(a.cfg.GetFloat("alarm.observation_window", 600)),
		suppression: secondsToDuration(a.cfg.GetFloat("alarm.suppression_window", 600)),
		reportCount: a.cfg.GetInt("alarm.report_thresh", 3),
	}
}

// ShouldReport applies the suppression and consecutive-count policy for key and
// returns whether this occurrence warrants a report. When checkConsecutive is
// false the event reports immediately (subject only to suppression), used for
// severe one-shot conditions such as a stalled refresh thread.
func (a *Alarm) ShouldReport(key string, checkConsecutive bool) bool {
	w := a.resolveWindows()
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()

	if last, ok := a.lastReported[key]; ok && now.Sub(last) < w.suppression {
		return false
	}

	if !checkConsecutive {
		a.lastReported[key] = now
		return true
	}

	kept := a.occurrences[key][:0:0]
	for _, t := range a.occurrences[key] {
		if now.Sub(t) <= w.observation {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)

	if len(kept) >= w.reportCount {
		a.lastReported[key] = now
		delete(a.occurrences, key)
		return true
	}
	a.occurrences[key] = kept
	return false
}

// CheckAndReport logs the value for local troubleshooting and, if the policy
// allows, appends a line to the alarm file. The key is matched case-insensitively.
func (a *Alarm) CheckAndReport(logger *logging.Logger, key, value string, checkConsecutive bool) {
	date := a.now().Format("2006-01-02 15:04:05")

	a.fileMu.Lock()
	host := a.hostname
	a.fileMu.Unlock()

	logMsg := fmt.Sprintf("[ALARM][%s][%s]%s", date, host, value)
	logger.Warning("%s", logMsg)

	if !a.ShouldReport(toLower(key), checkConsecutive) {
		return
	}

	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	if a.file == nil {
		return
	}
	if _, err := a.file.WriteString(logMsg + "\n"); err != nil {
		logger.Error("write alarm file failed: %v", err)
		return
	}
	a.file.Sync()
}

// CleanExpiredKeys drops keys with no activity for twice the suppression window
// to bound memory, matching common/alarm.clean_expired_keys.
func (a *Alarm) CleanExpiredKeys() {
	expire := 2 * secondsToDuration(a.cfg.GetFloat("alarm.suppression_window", 600))
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()
	for key, t := range a.lastReported {
		if now.Sub(t) > expire {
			delete(a.lastReported, key)
		}
	}
	for key, occ := range a.occurrences {
		if len(occ) == 0 || now.Sub(occ[len(occ)-1]) > expire {
			delete(a.occurrences, key)
		}
	}
}

func secondsToDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
