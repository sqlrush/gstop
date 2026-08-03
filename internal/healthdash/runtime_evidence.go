package healthdash

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CPUObservation is one retained statement_history execution.
type CPUObservation struct {
	At       time.Time
	CPUUS    float64
	DBTimeUS float64
}

// ASHSample is one normalized local_active_session or GS_ASP row.
type ASHSample struct {
	At         time.Time
	State      string
	Event      string
	WaitStatus string
}

// CPUSummary reports exact statement-history CPU measurements in milliseconds.
type CPUSummary struct {
	Available        bool
	Samples          int
	NewestAt         time.Time
	NewestMS         float64
	MedianMS         float64
	MaxMS            float64
	CPUToDBRatio     float64
	CPUToDBAvailable bool
}

// ASHWait is one grouped active-session wait classification.
type ASHWait struct {
	Event   string
	Samples int
}

// ASHSummary reports sample counts. OnCPUShare is an inference, not exact CPU
// time, and excludes idle-in-transaction samples.
type ASHSummary struct {
	Available     bool
	ActiveSamples int
	OnCPUSamples  int
	OnCPUShare    float64
	Waits         []ASHWait
}

// RuntimeEvidence combines exact retained CPU and sample-based ASH evidence.
type RuntimeEvidence struct {
	CPU CPUSummary
	ASH ASHSummary
}

// SummarizeCPU calculates per-execution median/max values and a ratio of the
// aggregate CPU time to aggregate DB time.
func SummarizeCPU(rows []CPUObservation) CPUSummary {
	if len(rows) == 0 {
		return CPUSummary{}
	}
	result := CPUSummary{Available: true, Samples: len(rows)}
	values := make([]float64, 0, len(rows))
	var cpuSum, dbTimeSum float64
	for i, row := range rows {
		values = append(values, row.CPUUS)
		cpuSum += row.CPUUS
		dbTimeSum += row.DBTimeUS
		if i == 0 || row.At.After(result.NewestAt) {
			result.NewestAt = row.At
			result.NewestMS = row.CPUUS / 1000
		}
		if row.CPUUS/1000 > result.MaxMS {
			result.MaxMS = row.CPUUS / 1000
		}
	}
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 0 {
		result.MedianMS = (values[middle-1] + values[middle]) / 2000
	} else {
		result.MedianMS = values[middle] / 1000
	}
	if dbTimeSum > 0 {
		result.CPUToDBAvailable = true
		result.CPUToDBRatio = cpuSum / dbTimeSum
	}
	return result
}

// SummarizeASH keeps active samples only. A sample is inferred on-CPU when both
// event and wait status are empty/none.
func SummarizeASH(rows []ASHSample) ASHSummary {
	waitCounts := make(map[string]int)
	result := ASHSummary{}
	for _, row := range rows {
		if !strings.EqualFold(strings.TrimSpace(row.State), "active") {
			continue
		}
		result.ActiveSamples++
		eventNone := ashValueIsNone(row.Event)
		statusNone := ashValueIsNone(row.WaitStatus)
		if eventNone && statusNone {
			result.OnCPUSamples++
			continue
		}
		label := strings.TrimSpace(row.Event)
		if eventNone {
			label = strings.TrimSpace(row.WaitStatus)
		}
		if label == "" {
			label = "unknown wait"
		}
		waitCounts[label]++
	}
	if result.ActiveSamples == 0 {
		return result
	}
	result.Available = true
	result.OnCPUShare = float64(result.OnCPUSamples) / float64(result.ActiveSamples)
	for event, samples := range waitCounts {
		result.Waits = append(result.Waits, ASHWait{Event: event, Samples: samples})
	}
	sort.Slice(result.Waits, func(i, j int) bool {
		if result.Waits[i].Samples != result.Waits[j].Samples {
			return result.Waits[i].Samples > result.Waits[j].Samples
		}
		return result.Waits[i].Event < result.Waits[j].Event
	})
	return result
}

func ashValueIsNone(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || strings.EqualFold(value, "none") || strings.EqualFold(value, "null")
}

type cpuEvidenceResult struct {
	Summary CPUSummary
	Notices []DiagnosticNotice
	Failed  bool
	Message string
}

func (l *DetailLoader) collectCPUEvidence(
	ctx context.Context,
	sqlID int64,
) (CPUSummary, []DiagnosticNotice) {
	result := l.collectCPUEvidenceResult(ctx, sqlID)
	return result.Summary, result.Notices
}

func (l *DetailLoader) collectCPUEvidenceResult(
	ctx context.Context,
	sqlID int64,
) cpuEvidenceResult {
	columns, probeFailed := l.probeColumnsResult(ctx, "dbe_perf", "statement_history")
	if ctx.Err() != nil {
		return cpuEvidenceResult{}
	}
	if probeFailed {
		message := "statement_history CPU 字段能力查询失败或无权限"
		return cpuEvidenceResult{
			Failed: true, Message: message,
			Notices: []DiagnosticNotice{{Area: "cpu", Message: message}},
		}
	}
	if !containsColumns(columns, "start_time", "cpu_time", "db_time", "unique_query_id") {
		if ctx.Err() != nil {
			return cpuEvidenceResult{}
		}
		return cpuEvidenceResult{Notices: []DiagnosticNotice{{
			Area: "cpu", Message: "statement_history 缺少 CPU 时间所需字段或当前用户不可见",
		}}}
	}
	query := "SELECT start_time, cpu_time, db_time FROM dbe_perf.statement_history " +
		"WHERE start_time >= current_timestamp - interval '60 minutes'" +
		" AND unique_query_id = " + strconv.FormatInt(sqlID, 10) +
		" ORDER BY start_time DESC LIMIT 20;"
	rows := l.query(ctx, query)
	if rows == nil && ctx.Err() == nil {
		message := "statement_history CPU 历史查询失败或无权限"
		return cpuEvidenceResult{
			Failed: true, Message: message,
			Notices: []DiagnosticNotice{{Area: "cpu", Message: message}},
		}
	}
	observations := make([]CPUObservation, 0, len(rows))
	for _, row := range rows {
		at, _ := rowTimestamp(row, 0)
		cpu, cpuOK := row.Float(1)
		dbTime, dbTimeOK := row.Float(2)
		if !cpuOK {
			continue
		}
		if !dbTimeOK {
			dbTime = 0
		}
		observations = append(observations, CPUObservation{
			At: at, CPUUS: cpu, DBTimeUS: dbTime,
		})
	}
	summary := SummarizeCPU(observations)
	if summary.Available || ctx.Err() != nil {
		return cpuEvidenceResult{Summary: summary}
	}
	return cpuEvidenceResult{
		Summary: summary,
		Notices: []DiagnosticNotice{{
			Area: "cpu", Message: "statement_history 中没有该 SQL 最近 60 分钟的 CPU 历史记录",
		}},
	}
}

var ashCandidates = []struct {
	Schema   string
	Relation string
}{
	{Schema: "dbe_perf", Relation: "local_active_session"},
	{Schema: "dbe_perf", Relation: "local_active_session_history"},
	{Schema: "dbe_perf", Relation: "gs_asp"},
}

type ashEvidenceResult struct {
	Summary ASHSummary
	Notices []DiagnosticNotice
	Failed  bool
	Message string
}

func (l *DetailLoader) collectASHEvidence(
	ctx context.Context,
	sqlID int64,
) (ASHSummary, []DiagnosticNotice) {
	result := l.collectASHEvidenceResult(ctx, sqlID)
	return result.Summary, result.Notices
}

func (l *DetailLoader) collectASHEvidenceResult(
	ctx context.Context,
	sqlID int64,
) ashEvidenceResult {
	var source, sqlIDColumn string
	for _, candidate := range ashCandidates {
		columns, probeFailed := l.probeColumnsResult(ctx, candidate.Schema, candidate.Relation)
		if ctx.Err() != nil {
			return ashEvidenceResult{}
		}
		if probeFailed {
			message := candidate.Schema + "." + candidate.Relation +
				" ASH 字段能力查询失败或无权限"
			return ashEvidenceResult{
				Failed: true, Message: message,
				Notices: []DiagnosticNotice{{Area: "ash", Message: message}},
			}
		}
		if !containsColumns(columns, "sample_time", "state", "event", "wait_status") {
			continue
		}
		switch {
		case columns["unique_query_id"]:
			sqlIDColumn = "unique_query_id"
		case columns["unique_sql_id"]:
			sqlIDColumn = "unique_sql_id"
		default:
			continue
		}
		source = candidate.Schema + "." + candidate.Relation
		break
	}
	if source == "" {
		return ashEvidenceResult{Notices: []DiagnosticNotice{{
			Area:    "ash",
			Message: "local_active_session、local_active_session_history 与 GS_ASP 均不可用或字段不兼容",
		}}}
	}

	query := "SELECT sample_time, state, event, wait_status FROM " + source +
		" WHERE sample_time >= current_timestamp - interval '15 minutes'" +
		" AND " + sqlIDColumn + " = " + strconv.FormatInt(sqlID, 10) +
		" ORDER BY sample_time DESC;"
	rows := l.query(ctx, query)
	if rows == nil && ctx.Err() == nil {
		message := source + " ASH 样本查询失败或无权限"
		return ashEvidenceResult{
			Failed: true, Message: message,
			Notices: []DiagnosticNotice{{Area: "ash", Message: message}},
		}
	}
	samples := make([]ASHSample, 0, len(rows))
	for _, row := range rows {
		at, _ := rowTimestamp(row, 0)
		samples = append(samples, ASHSample{
			At: at, State: row.Str(1), Event: row.Str(2), WaitStatus: row.Str(3),
		})
	}
	summary := SummarizeASH(samples)
	if summary.Available || ctx.Err() != nil {
		return ashEvidenceResult{Summary: summary}
	}
	return ashEvidenceResult{
		Summary: summary,
		Notices: []DiagnosticNotice{{
			Area:    "ash",
			Message: "最近 15 分钟没有该 SQL 的 active ASH 样本；ASH on-CPU 仅为推断且排除 idle in transaction",
		}},
	}
}
