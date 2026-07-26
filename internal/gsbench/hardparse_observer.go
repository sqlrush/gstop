package gsbench

import (
	"context"
	"fmt"
)

// HardParseSample is a direct statement-history counter snapshot.  A sample
// is usable only when every required counter was read from the target.
type HardParseSample struct {
	Node      string
	Hard      int64
	Soft      int64
	ParseUS   int64
	PlanUS    int64
	Available bool
}

type HardParseDelta struct {
	Node      string
	Hard      int64
	Soft      int64
	ParseUS   int64
	PlanUS    int64
	Ratio     float64
	Available bool
}

type HardParseObserver interface {
	Sample(context.Context, *Runtime, string) (HardParseSample, error)
}

type databaseHardParseObserver struct{}

func (databaseHardParseObserver) Sample(ctx context.Context, rt *Runtime, scenario string) (HardParseSample, error) {
	if rt == nil || rt.Database == nil {
		return HardParseSample{}, fmt.Errorf("hard-parse observer database is unavailable")
	}
	applicationPrefix, err := taggedScenarioPattern(rt.RunID, scenario)
	if err != nil {
		return HardParseSample{}, fmt.Errorf("hard-parse observer application identity: %w", err)
	}
	var sample HardParseSample
	if err := rt.Database.Scan(ctx, `SELECT COALESCE(sum(n_hard_parse),0)::bigint,
		COALESCE(sum(n_soft_parse),0)::bigint,
		COALESCE(sum(parse_time),0)::bigint,
		COALESCE(sum(plan_time),0)::bigint
		FROM dbe_perf.statement_history WHERE application_name LIKE $1 ESCAPE E'\\'`,
		[]any{applicationPrefix},
		&sample.Hard, &sample.Soft, &sample.ParseUS, &sample.PlanUS); err != nil {
		return HardParseSample{}, fmt.Errorf("read direct hard-parse counters: %w", err)
	}
	if err := rt.Database.Scan(ctx,
		"SELECT COALESCE(inet_server_addr()::text,'local')", nil, &sample.Node); err != nil {
		return HardParseSample{}, fmt.Errorf("read hard-parse observer node: %w", err)
	}
	sample.Available = sample.Node != ""
	return sample, nil
}

func hardParseDelta(before, after HardParseSample) HardParseDelta {
	if !before.Available || !after.Available || after.Hard < before.Hard ||
		after.Soft < before.Soft || after.ParseUS < before.ParseUS || after.PlanUS < before.PlanUS {
		return HardParseDelta{}
	}
	delta := HardParseDelta{
		Node: after.Node, Hard: after.Hard - before.Hard, Soft: after.Soft - before.Soft,
		ParseUS: after.ParseUS - before.ParseUS, PlanUS: after.PlanUS - before.PlanUS,
		Available: true,
	}
	if total := delta.Hard + delta.Soft; total > 0 {
		delta.Ratio = float64(delta.Hard) / float64(total)
	}
	return delta
}

func EvaluateHardParse(name string, delta HardParseDelta) Result {
	result := Result{Scenario: name, Outcome: OutcomeFailed}
	if !delta.Available {
		result.Message = "direct hard/soft parse and parse/plan time counters are unavailable"
		return result
	}
	result.Evidence = []Evidence{
		{Metric: "hard_parse_delta", Target: 1, Actual: float64(delta.Hard), Available: true, Details: map[string]any{"node": delta.Node}},
		{Metric: "soft_parse_delta", Actual: float64(delta.Soft), Available: true, Details: map[string]any{"node": delta.Node}},
		{Metric: "hard_parse_ratio", Actual: delta.Ratio, Available: true},
		{Metric: "parse_time_us_delta", Actual: float64(delta.ParseUS), Available: true},
		{Metric: "plan_time_us_delta", Actual: float64(delta.PlanUS), Available: true},
	}
	if delta.Hard < 1 {
		result.Message = "direct hard-parse counter did not increase"
		return result
	}
	result.Outcome = OutcomeSuccess
	result.Message = "direct hard-parse counter increased"
	return result
}
