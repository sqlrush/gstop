package healthdash

import (
	"sort"
	"strings"
	"time"
)

const (
	topSQLCount  = 5
	topWaitCount = 5
)

// BuildSQLMetrics returns the gstop-start-relative average-duration and
// execution-count TOP5. A running session contributes its complete elapsed
// time to the average.
func BuildSQLMetrics(current, baseline []StatementSample, active []ActiveSQL, capturedAt time.Time) (average, executions []SQLMetric) {
	current = aggregateStatementSamples(current)
	baseline = aggregateStatementSamples(baseline)
	baseByID := statementIndex(baseline)
	activeByID := activeIndex(active)
	currentIDs := make(map[int64]bool, len(current))
	average = make([]SQLMetric, 0, len(current))
	executions = make([]SQLMetric, 0, len(current))

	for _, sample := range current {
		if sample.SQLID == 0 {
			continue
		}
		currentIDs[sample.SQLID] = true
		running := activeByID[sample.SQLID]
		query := sample.Query
		if query == "" {
			query = running.query
		}
		completedCalls := sample.Calls
		completedDBTime := sample.DBTimeUS
		if base, ok := baseByID[sample.SQLID]; ok {
			completedCalls = nonNegativeDelta(sample.Calls, base.Calls)
			completedDBTime = nonNegativeFloatDelta(sample.DBTimeUS, base.DBTimeUS)
		}
		observedCalls := completedCalls + int64(running.count)
		if observedCalls == 0 {
			continue
		}
		metric := SQLMetric{
			SQLID:          sample.SQLID,
			Query:          query,
			Databases:      mergeSortedStrings(sample.Databases, running.databases),
			Users:          mergeSortedStrings(sample.Users, running.users),
			Calls:          completedCalls,
			CallsDelta:     completedCalls,
			ActiveSessions: running.count,
			AverageUS:      (completedDBTime + running.elapsedUS) / float64(observedCalls),
		}
		applyRepresentative(&metric, running, capturedAt)
		average = append(average, metric)

		if completedCalls > 0 {
			executions = append(executions, metric)
		}
	}
	seenActive := map[int64]bool{}
	for _, row := range active {
		if row.SQLID == 0 || currentIDs[row.SQLID] || seenActive[row.SQLID] {
			continue
		}
		seenActive[row.SQLID] = true
		running := activeByID[row.SQLID]
		metric := SQLMetric{
			SQLID: row.SQLID, Query: running.query, AverageUS: running.elapsedUS / float64(running.count),
			ActiveSessions: running.count, Databases: running.databases, Users: running.users,
		}
		applyRepresentative(&metric, running, capturedAt)
		average = append(average, metric)
	}

	sort.SliceStable(average, func(i, j int) bool {
		return average[i].AverageUS > average[j].AverageUS
	})
	sort.SliceStable(executions, func(i, j int) bool {
		return executions[i].CallsDelta > executions[j].CallsDelta
	})
	return limitSQL(average, topSQLCount), limitSQL(executions, topSQLCount)
}

// BuildActiveElapsedMetrics returns the TOP5 currently active SQL statements
// ranked by their longest-running session.
func BuildActiveElapsedMetrics(active []ActiveSQL, capturedAt time.Time) []SQLMetric {
	grouped := activeElapsedIndex(active)
	rows := make([]SQLMetric, 0, len(grouped))
	for sqlID, running := range grouped {
		metric := SQLMetric{
			SQLID: sqlID, Query: running.query,
			Databases: running.databases, Users: running.users,
			ActiveSessions: running.count,
		}
		applyRepresentative(&metric, running, capturedAt)
		rows = append(rows, metric)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].RepresentativeElapsedUS != rows[j].RepresentativeElapsedUS {
			return rows[i].RepresentativeElapsedUS > rows[j].RepresentativeElapsedUS
		}
		return rows[i].SQLID < rows[j].SQLID
	})
	return limitSQL(rows, topSQLCount)
}

type activeSessionIdentity struct {
	sessionID string
	pid       int64
}

// activeElapsedIndex collapses openGauss parallel-worker rows into their
// logical session. Parallel workers share sessionid but have different pids;
// when sessionid is unavailable, pid is the best available identity.
func activeElapsedIndex(active []ActiveSQL) map[int64]activeAggregate {
	out := make(map[int64]activeAggregate, len(active))
	seen := make(map[int64]map[activeSessionIdentity]struct{}, len(active))
	for _, row := range active {
		if row.SQLID == 0 {
			continue
		}
		row.SessionID = strings.TrimSpace(row.SessionID)
		if row.SessionID == "0" {
			row.SessionID = ""
		}
		if row.SessionID == "" && row.PID == 0 {
			continue
		}
		identity := activeSessionIdentity{sessionID: row.SessionID}
		if identity.sessionID == "" {
			identity.pid = row.PID
		}
		if seen[row.SQLID] == nil {
			seen[row.SQLID] = make(map[activeSessionIdentity]struct{})
		}
		a := out[row.SQLID]
		if _, exists := seen[row.SQLID][identity]; !exists {
			a.count++
			seen[row.SQLID][identity] = struct{}{}
		}
		if a.query == "" {
			a.query = row.Query
		}
		a.databases = mergeSortedStrings(a.databases, []string{row.Database})
		a.users = mergeSortedStrings(a.users, []string{row.User})
		if preferRepresentative(row, a.representative) {
			a.representative = row
		}
		out[row.SQLID] = a
	}
	return out
}

type activeAggregate struct {
	count          int
	elapsedUS      float64
	query          string
	databases      []string
	users          []string
	representative ActiveSQL
}

func preferRepresentative(candidate, current ActiveSQL) bool {
	if current.PID == 0 && current.SessionID == "" {
		return true
	}
	if candidate.ElapsedUS != current.ElapsedUS {
		return candidate.ElapsedUS > current.ElapsedUS
	}
	if candidate.SessionID != current.SessionID {
		return candidate.SessionID < current.SessionID
	}
	return candidate.PID < current.PID
}

func activeIndex(active []ActiveSQL) map[int64]activeAggregate {
	out := make(map[int64]activeAggregate, len(active))
	for _, row := range active {
		if row.SQLID == 0 {
			continue
		}
		a := out[row.SQLID]
		a.count++
		a.elapsedUS += row.ElapsedUS
		if a.query == "" {
			a.query = row.Query
		}
		a.databases = mergeSortedStrings(a.databases, []string{row.Database})
		a.users = mergeSortedStrings(a.users, []string{row.User})
		if preferRepresentative(row, a.representative) {
			a.representative = row
		}
		out[row.SQLID] = a
	}
	return out
}

func applyRepresentative(metric *SQLMetric, running activeAggregate, capturedAt time.Time) {
	if running.representative.PID == 0 && running.representative.SessionID == "" {
		return
	}
	metric.RepresentativePID = running.representative.PID
	metric.RepresentativeSessionID = running.representative.SessionID
	metric.RepresentativeElapsedUS = running.representative.ElapsedUS
	metric.RepresentativeQueryStart = running.representative.QueryStart
	metric.CapturedAt = capturedAt
}

func statementIndex(rows []StatementSample) map[int64]StatementSample {
	out := make(map[int64]StatementSample, len(rows))
	for _, row := range rows {
		out[row.SQLID] = row
	}
	return out
}

func aggregateStatementSamples(rows []StatementSample) []StatementSample {
	out := make([]StatementSample, 0, len(rows))
	positions := make(map[int64]int, len(rows))
	for _, row := range rows {
		if index, ok := positions[row.SQLID]; ok {
			out[index] = mergeStatementSample(out[index], row)
			continue
		}
		row.Databases = sortedUniqueStrings(row.Databases)
		row.Users = sortedUniqueStrings(row.Users)
		positions[row.SQLID] = len(out)
		out = append(out, row)
	}
	return out
}

func mergeStatementSample(left, right StatementSample) StatementSample {
	left.Calls += right.Calls
	left.DBTimeUS += right.DBTimeUS
	if left.Query == "" {
		left.Query = right.Query
	}
	left.Databases = mergeSortedStrings(left.Databases, right.Databases)
	left.Users = mergeSortedStrings(left.Users, right.Users)
	return left
}

func mergeSortedStrings(left, right []string) []string {
	return sortedUniqueStrings(append(append([]string(nil), left...), right...))
}

func sortedUniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func limitSQL(rows []SQLMetric, n int) []SQLMetric {
	if len(rows) > n {
		rows = rows[:n]
	}
	return append([]SQLMetric(nil), rows...)
}

// BuildMemoryMetrics aggregates active-session memory by SQL ID and returns the
// TOP3 by total memory.
func BuildMemoryMetrics(active []ActiveSQL, capturedAt time.Time) []SQLMetric {
	byID := make(map[int64]SQLMetric, len(active))
	var order []int64
	for _, row := range active {
		if row.SQLID == 0 {
			continue
		}
		metric, exists := byID[row.SQLID]
		if !exists {
			metric = SQLMetric{SQLID: row.SQLID, Query: row.Query}
			order = append(order, row.SQLID)
		}
		metric.Databases = mergeSortedStrings(metric.Databases, []string{row.Database})
		metric.Users = mergeSortedStrings(metric.Users, []string{row.User})
		metric.ActiveSessions++
		metric.TotalMemoryMB += row.MemoryMB
		if row.MemoryMB > metric.MaxMemoryMB {
			metric.MaxMemoryMB = row.MemoryMB
		}
		if metric.Query == "" {
			metric.Query = row.Query
		}
		if preferRepresentative(row, ActiveSQL{
			PID: metric.RepresentativePID, SessionID: metric.RepresentativeSessionID,
			ElapsedUS: metric.RepresentativeElapsedUS,
		}) {
			metric.RepresentativePID = row.PID
			metric.RepresentativeSessionID = row.SessionID
			metric.RepresentativeElapsedUS = row.ElapsedUS
			metric.CapturedAt = capturedAt
		}
		byID[row.SQLID] = metric
	}

	rows := make([]SQLMetric, 0, len(order))
	for _, id := range order {
		rows = append(rows, byID[id])
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].TotalMemoryMB > rows[j].TotalMemoryMB
	})
	return limitSQL(rows, topSQLCount)
}

// BuildWaitMetrics returns the TOP5 wait events ranked by cumulative wait-time
// delta since gstop startup. DB CPU is calculated in the same denominator but
// returned separately so it never occupies a TOP5 row.
func BuildWaitMetrics(current, baseline []WaitSample, currentCPU, baselineCPU int64) ([]WaitMetric, CPUStat) {
	baseByEvent := make(map[string]WaitSample, len(baseline))
	for _, row := range baseline {
		baseByEvent[row.Event] = row
	}

	metrics := make([]WaitMetric, 0, len(current))
	var totalWaitUS int64
	for _, row := range current {
		base := baseByEvent[row.Event]
		waitsDelta := nonNegativeDelta(row.Waits, base.Waits)
		timeDelta := nonNegativeDelta(row.TimeUS, base.TimeUS)
		if timeDelta == 0 {
			continue
		}
		metric := WaitMetric{
			Event:       row.Event,
			WaitsDelta:  waitsDelta,
			TimeUSDelta: timeDelta,
			Type:        row.Type,
		}
		if waitsDelta > 0 {
			metric.AverageUS = float64(timeDelta) / float64(waitsDelta)
		}
		metrics = append(metrics, metric)
		totalWaitUS += timeDelta
	}

	cpuDelta := nonNegativeDelta(currentCPU, baselineCPU)
	totalTimeUS := totalWaitUS + cpuDelta
	if totalTimeUS > 0 {
		for i := range metrics {
			metrics[i].Share = float64(metrics[i].TimeUSDelta) / float64(totalTimeUS)
		}
	}
	sort.SliceStable(metrics, func(i, j int) bool {
		return metrics[i].TimeUSDelta > metrics[j].TimeUSDelta
	})
	if len(metrics) > topWaitCount {
		metrics = metrics[:topWaitCount]
	}
	cpu := CPUStat{TimeUSDelta: cpuDelta}
	if totalTimeUS > 0 {
		cpu.Share = float64(cpuDelta) / float64(totalTimeUS)
	}
	return append([]WaitMetric(nil), metrics...), cpu
}

func nonNegativeDelta(current, baseline int64) int64 {
	if current < baseline {
		return 0
	}
	return current - baseline
}

func nonNegativeFloatDelta(current, baseline float64) float64 {
	if current < baseline {
		return 0
	}
	return current - baseline
}
