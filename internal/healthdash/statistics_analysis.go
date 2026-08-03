package healthdash

import (
	"fmt"
	"math"
	"time"
)

// Freshness is the dashboard classification of optimizer statistics.
type Freshness string

const (
	FreshnessExpired Freshness = "已过期"
	FreshnessSuspect Freshness = "疑似过期"
	FreshnessNormal  Freshness = "正常"
	FreshnessVerify  Freshness = "需要验证"
)

// StatisticsEvidence contains the catalog values needed to judge whether a
// table has reached its current automatic-analyze trigger.
type StatisticsEvidence struct {
	Available             bool
	LastAnalyze           time.Time
	LastDataChanged       time.Time
	AnalyzeTimestampKnown bool
	TimestampOnly         bool
	LiveTuples            float64
	ModifiedSinceAnalyze  float64
	AnalyzeThreshold      float64
	AnalyzeScaleFactor    float64
}

// StatisticsAssessment is a deterministic, evidence-backed freshness result.
type StatisticsAssessment struct {
	State            Freshness
	LastAnalyze      time.Time
	LastDataChanged  time.Time
	TriggerAvailable bool
	Trigger          float64
	DueRatio         float64
	Reasons          []string
}

// PerformanceSignals provide supporting evidence only. They cannot on their
// own classify table statistics as stale.
type PerformanceSignals struct {
	InHotspotSubtree bool
	HotspotCostShare float64
	CPUToDBRatio     float64
	ASHCPUShare      float64
	CPUToDBAvailable bool
	ASHAvailable     bool
}

// ClassifyStatistics applies the documented analyze-trigger and seven-day
// boundaries. SQL-level signals are ignored unless this table is scanned in
// the selected hotspot subtree.
func ClassifyStatistics(now time.Time, evidence StatisticsEvidence, signals PerformanceSignals) StatisticsAssessment {
	result := StatisticsAssessment{
		LastAnalyze: evidence.LastAnalyze, LastDataChanged: evidence.LastDataChanged,
	}
	if !evidence.Available {
		result.State = FreshnessVerify
		result.Reasons = []string{"统计信息字段不可用或权限不足"}
		return result
	}
	if !evidence.AnalyzeTimestampKnown {
		result.State = FreshnessVerify
		result.Reasons = []string{"无法确认 ANALYZE 时间字段"}
		return result
	}

	if evidence.LastAnalyze.IsZero() {
		result.State = FreshnessExpired
		result.Reasons = []string{"该表从未记录 ANALYZE 或 AUTOANALYZE 时间"}
		return result
	}
	if evidence.TimestampOnly {
		if evidence.LastDataChanged.IsZero() {
			result.State = FreshnessVerify
			result.Reasons = []string{"GaussDB 未提供修改行数，且无法确认最近数据变更时间"}
			return result
		}
		if !evidence.LastDataChanged.After(evidence.LastAnalyze) {
			result.State = FreshnessNormal
			result.Reasons = []string{"最近一次数据变更不晚于统计信息收集时间"}
			return result
		}
		result.State = FreshnessVerify
		result.Reasons = []string{
			"数据在最近一次 ANALYZE 后发生过变化",
			"当前 GaussDB 视图不提供修改行数与 ANALYZE 触发参数，无法计算 due ratio",
		}
		return result
	}

	result.Trigger = evidence.AnalyzeThreshold + evidence.AnalyzeScaleFactor*evidence.LiveTuples
	result.DueRatio = evidence.ModifiedSinceAnalyze / math.Max(result.Trigger, 1)
	result.TriggerAvailable = true
	if result.DueRatio >= 1 {
		result.State = FreshnessExpired
		result.Reasons = []string{
			fmt.Sprintf("修改量已达到当前 ANALYZE 触发值的 %.0f%%", result.DueRatio*100),
		}
		return result
	}

	oldEnough := now.Sub(evidence.LastAnalyze) > 7*24*time.Hour
	supportingSignal := signals.InHotspotSubtree && (signals.HotspotCostShare >= .30 ||
		signals.CPUToDBAvailable && signals.CPUToDBRatio >= .50 ||
		signals.ASHAvailable && signals.ASHCPUShare >= .50)
	if oldEnough && result.DueRatio >= .50 && supportingSignal {
		result.State = FreshnessSuspect
		result.Reasons = []string{
			"统计信息早于 7 天且修改量已达到当前 ANALYZE 触发值的 50%",
			"热点子树存在高 cost 或 CPU 支持信号",
		}
		return result
	}

	result.State = FreshnessNormal
	result.Reasons = []string{"未达到统计信息过期判定条件"}
	return result
}
