package healthdash

import (
	"math"
	"testing"
	"time"
)

func TestClassifyStatisticsBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	base := StatisticsEvidence{
		Available:             true,
		AnalyzeTimestampKnown: true,
		LastAnalyze:           now.Add(-8 * 24 * time.Hour),
		LiveTuples:            1000,
		AnalyzeThreshold:      50,
		AnalyzeScaleFactor:    .1,
	}
	tests := []struct {
		name    string
		mods    float64
		signals PerformanceSignals
		want    Freshness
	}{
		{"normal below half", 74, PerformanceSignals{}, FreshnessNormal},
		{"suspect at half with hotspot", 75, PerformanceSignals{
			InHotspotSubtree: true, HotspotCostShare: .30,
		}, FreshnessSuspect},
		{"expired at trigger", 150, PerformanceSignals{}, FreshnessExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evidence := base
			evidence.ModifiedSinceAnalyze = tt.mods
			got := ClassifyStatistics(now, evidence, tt.signals)
			if got.State != tt.want {
				t.Fatalf("state=%s want=%s assessment=%+v", got.State, tt.want, got)
			}
			if math.Abs(got.Trigger-150) > .000001 {
				t.Fatalf("trigger=%v want=150", got.Trigger)
			}
		})
	}
}

func TestClassifyStatisticsRequiresCompleteEvidence(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	if got := ClassifyStatistics(now, StatisticsEvidence{}, PerformanceSignals{}); got.State != FreshnessVerify {
		t.Fatalf("unavailable state=%s", got.State)
	}
	if got := ClassifyStatistics(now, StatisticsEvidence{
		Available: true, AnalyzeTimestampKnown: false,
	}, PerformanceSignals{}); got.State != FreshnessVerify {
		t.Fatalf("unknown timestamp state=%s", got.State)
	}
	if got := ClassifyStatistics(now, StatisticsEvidence{
		Available: true, AnalyzeTimestampKnown: true,
		LiveTuples: 100, AnalyzeThreshold: 50, AnalyzeScaleFactor: .1,
	}, PerformanceSignals{}); got.State != FreshnessExpired {
		t.Fatalf("never analyzed state=%s", got.State)
	}
}

func TestClassifyStatisticsSuspectSignalRules(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	base := StatisticsEvidence{
		Available: true, AnalyzeTimestampKnown: true,
		LastAnalyze: now.Add(-8 * 24 * time.Hour),
		LiveTuples:  1000, ModifiedSinceAnalyze: 75,
		AnalyzeThreshold: 50, AnalyzeScaleFactor: .1,
	}

	tests := []struct {
		name     string
		evidence StatisticsEvidence
		signals  PerformanceSignals
		want     Freshness
	}{
		{
			name: "exactly seven days is not older",
			evidence: func() StatisticsEvidence {
				e := base
				e.LastAnalyze = now.Add(-7 * 24 * time.Hour)
				return e
			}(),
			signals: PerformanceSignals{InHotspotSubtree: true, HotspotCostShare: .3},
			want:    FreshnessNormal,
		},
		{
			name:     "signal outside hotspot subtree is ignored",
			evidence: base,
			signals:  PerformanceSignals{HotspotCostShare: .9, CPUToDBAvailable: true, CPUToDBRatio: .9},
			want:     FreshnessNormal,
		},
		{
			name:     "cpu threshold is inclusive",
			evidence: base,
			signals:  PerformanceSignals{InHotspotSubtree: true, CPUToDBAvailable: true, CPUToDBRatio: .5},
			want:     FreshnessSuspect,
		},
		{
			name:     "ash threshold is inclusive",
			evidence: base,
			signals:  PerformanceSignals{InHotspotSubtree: true, ASHAvailable: true, ASHCPUShare: .5},
			want:     FreshnessSuspect,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyStatistics(now, tt.evidence, tt.signals); got.State != tt.want {
				t.Fatalf("state=%s want=%s assessment=%+v", got.State, tt.want, got)
			}
		})
	}
}

func TestClassifyStatisticsGaussTimestampFallback(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	base := StatisticsEvidence{
		Available: true, AnalyzeTimestampKnown: true, TimestampOnly: true,
		LastAnalyze: now.Add(-8 * 24 * time.Hour),
		LiveTuples:  1000,
	}

	changedAfter := base
	changedAfter.LastDataChanged = now.Add(-2 * 24 * time.Hour)
	got := ClassifyStatistics(now, changedAfter, PerformanceSignals{
		InHotspotSubtree: true, HotspotCostShare: .8,
	})
	if got.State != FreshnessVerify || got.LastDataChanged != changedAfter.LastDataChanged {
		t.Fatalf("changed-after assessment=%+v", got)
	}

	unchangedSince := base
	unchangedSince.LastDataChanged = now.Add(-9 * 24 * time.Hour)
	if got := ClassifyStatistics(now, unchangedSince, PerformanceSignals{}); got.State != FreshnessNormal {
		t.Fatalf("unchanged-since assessment=%+v", got)
	}

	neverAnalyzed := base
	neverAnalyzed.LastAnalyze = time.Time{}
	if got := ClassifyStatistics(now, neverAnalyzed, PerformanceSignals{}); got.State != FreshnessExpired {
		t.Fatalf("never-analyzed assessment=%+v", got)
	}
}
