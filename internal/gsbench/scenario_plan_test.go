package gsbench

import (
	"strings"
	"testing"
	"time"
)

func TestPlanRegressionRequiresChangedPlanSameResultAndSlowdown(t *testing.T) {
	base := PlanObservation{PlanHash: "fast", ResultFingerprint: "42:900", Median: 10 * time.Millisecond}
	worse := PlanObservation{PlanHash: "slow", ResultFingerprint: "42:900", Median: 25 * time.Millisecond}
	result := EvaluatePlanRegression(base, worse, 2.0)
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	worse.PlanHash = "fast"
	if got := EvaluatePlanRegression(base, worse, 2.0).Outcome; got != OutcomeFailed {
		t.Fatalf("unchanged plan outcome=%s", got)
	}
	worse.PlanHash = "slow"
	worse.ResultFingerprint = "different"
	if got := EvaluatePlanRegression(base, worse, 2.0).Outcome; got != OutcomeFailed {
		t.Fatalf("wrong-result outcome=%s", got)
	}
}

func TestPlanMutationHasVerifiedInverseForEveryTrigger(t *testing.T) {
	for _, trigger := range []string{"index_unusable", "stats_skew", "hard_parse"} {
		mutation, err := PlanMutation("run-1", "gsbench", trigger)
		if err != nil {
			t.Fatal(err)
		}
		if mutation.ForwardSQL == "" || mutation.InverseSQL == "" || mutation.VerifySQL == "" {
			t.Fatalf("trigger %s mutation=%+v", trigger, mutation)
		}
	}
	index, _ := PlanMutation("run-1", "gsbench", "index_unusable")
	if !strings.Contains(index.ForwardSQL, "UNUSABLE") || !strings.Contains(index.InverseSQL, "REBUILD") {
		t.Fatalf("index mutation=%+v", index)
	}
}
