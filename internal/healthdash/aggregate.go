package healthdash

import "sort"

const (
	topSQLCount  = 3
	topWaitCount = 5
)

// BuildSQLMetrics returns the instance-wide average-duration TOP3 and the
// gstop-start-relative execution-count TOP3. A running session contributes one
// unfinished call and its elapsed time to the average.
func BuildSQLMetrics(current, baseline []StatementSample, active []ActiveSQL) (average, executions []SQLMetric) {
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
		denominator := sample.Calls + int64(running.count)
		metric := SQLMetric{
			SQLID:          sample.SQLID,
			Query:          query,
			Calls:          sample.Calls,
			ActiveSessions: running.count,
		}
		if denominator > 0 {
			metric.AverageUS = (sample.DBTimeUS + running.elapsedUS) / float64(denominator)
		}
		average = append(average, metric)

		delta := sample.Calls
		if base, ok := baseByID[sample.SQLID]; ok {
			delta = nonNegativeDelta(sample.Calls, base.Calls)
		}
		if delta > 0 {
			metric.CallsDelta = delta
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
		average = append(average, SQLMetric{
			SQLID: row.SQLID, Query: running.query, AverageUS: running.elapsedUS / float64(running.count),
			ActiveSessions: running.count,
		})
	}

	sort.SliceStable(average, func(i, j int) bool {
		return average[i].AverageUS > average[j].AverageUS
	})
	sort.SliceStable(executions, func(i, j int) bool {
		return executions[i].CallsDelta > executions[j].CallsDelta
	})
	return limitSQL(average, topSQLCount), limitSQL(executions, topSQLCount)
}

type activeAggregate struct {
	count     int
	elapsedUS float64
	query     string
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
		out[row.SQLID] = a
	}
	return out
}

func statementIndex(rows []StatementSample) map[int64]StatementSample {
	out := make(map[int64]StatementSample, len(rows))
	for _, row := range rows {
		out[row.SQLID] = row
	}
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
func BuildMemoryMetrics(active []ActiveSQL) []SQLMetric {
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
		metric.ActiveSessions++
		metric.TotalMemoryMB += row.MemoryMB
		if row.MemoryMB > metric.MaxMemoryMB {
			metric.MaxMemoryMB = row.MemoryMB
		}
		if metric.Query == "" {
			metric.Query = row.Query
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
