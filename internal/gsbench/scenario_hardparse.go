package gsbench

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

type HardParseScenario struct {
	code     ScenarioCode
	name     string
	observer HardParseObserver
	before   HardParseSample
	after    HardParseSample
	control  HardParseDelta
	workload *sqlWorkload
	sequence atomic.Int64
	prepared sync.Map
}

type HardParseProtocol struct {
	SimpleSQL    string
	ControlSQL   string
	SetupSQL     string
	PrepareSQL   string
	ExecuteSQL   string
	CleanupSQL   string
	MaxLiterals  int64
	literalQuery func(int64) (string, error)
}

func (p HardParseProtocol) LiteralSQL(sequence int64) (string, error) {
	if p.literalQuery == nil || sequence < 1 || sequence > p.MaxLiterals {
		return "", fmt.Errorf("hard-parse literal sequence %d is outside its unique bound", sequence)
	}
	return p.literalQuery(sequence)
}

func HardParseProtocolFor(code ScenarioCode, schema, tag string) (HardParseProtocol, error) {
	quoted, ok := quoteDatasetSchema(schema)
	if !ok {
		return HardParseProtocol{}, fmt.Errorf("unsafe schema %q", schema)
	}
	if tag == "" || !tagComponentRE.MatchString(tag) {
		return HardParseProtocol{}, fmt.Errorf("unsafe hard-parse tag %q", tag)
	}
	fact := quoted + ".fact_sales"
	targets := quoted + ".hardparse_targets"
	comment := "/* gsbench_hardparse_" + tag + " */"
	switch code {
	case 621:
		return HardParseProtocol{SimpleSQL: comment + " literal_flood", MaxLiterals: 100_000, literalQuery: func(sequence int64) (string, error) {
			return fmt.Sprintf("SELECT %s count(*),sum(id) FROM %s WHERE id=%d", comment, fact, sequence), nil
		}}, nil
	case 622:
		return HardParseProtocol{SimpleSQL: fmt.Sprintf("SELECT %s count(*),sum(id) FROM %s WHERE customer_id=424242", comment, fact), ControlSQL: fmt.Sprintf("PREPARE gsbench_hardparse_control(bigint) AS SELECT %s count(*),sum(id) FROM %s WHERE customer_id=$1", comment, fact), ExecuteSQL: "EXECUTE gsbench_hardparse_control(424242)", CleanupSQL: "DEALLOCATE gsbench_hardparse_control"}, nil
	case 623:
		return HardParseProtocol{SetupSQL: "SET plan_cache_mode=force_custom_plan", PrepareSQL: fmt.Sprintf("PREPARE gsbench_hardparse_623(bigint) AS SELECT %s count(*),sum(id) FROM %s WHERE customer_id=$1", comment, fact), ExecuteSQL: "EXECUTE gsbench_hardparse_623(424242)", CleanupSQL: "DEALLOCATE gsbench_hardparse_623"}, nil
	case 624:
		return HardParseProtocol{PrepareSQL: fmt.Sprintf("PREPARE gsbench_hardparse_624(bigint) AS SELECT %s count(*),sum(id) FROM %s WHERE customer_id=$1", comment, fact), ExecuteSQL: "EXECUTE gsbench_hardparse_624(424242)", CleanupSQL: "DEALLOCATE gsbench_hardparse_624"}, nil
	case 625:
		return HardParseProtocol{PrepareSQL: fmt.Sprintf("PREPARE gsbench_hardparse_625(bigint) AS SELECT %s count(*),sum(id) FROM %s WHERE lookup_key=$1", comment, targets), ExecuteSQL: "EXECUTE gsbench_hardparse_625(424242)", CleanupSQL: "DEALLOCATE gsbench_hardparse_625"}, nil
	default:
		return HardParseProtocol{}, fmt.Errorf("hard-parse scenario %d is not implemented", code)
	}
}

func NewHardParseScenario(code ScenarioCode, name string, observer HardParseObserver) *HardParseScenario {
	if observer == nil {
		observer = databaseHardParseObserver{}
	}
	return &HardParseScenario{code: code, name: name, observer: observer}
}

func (s *HardParseScenario) Code() ScenarioCode { return s.code }
func (s *HardParseScenario) Name() string       { return s.name }
func (s *HardParseScenario) Strategy() string   { return "direct_hard_parse_counter_delta" }

func HardParseStatement(code ScenarioCode, schema string, literal int64) (string, error) {
	protocol, err := HardParseProtocolFor(code, schema, fmt.Sprintf("shape%d", code))
	if err != nil {
		return "", err
	}
	if code == 621 {
		return protocol.LiteralSQL(literal)
	}
	if protocol.SimpleSQL != "" {
		return protocol.SimpleSQL, nil
	}
	return protocol.PrepareSQL, nil
}

func hardParseLiteralSQL(code ScenarioCode, schema string, literal int64) (string, error) {
	quoted, ok := quoteDatasetSchema(schema)
	if !ok {
		return "", fmt.Errorf("unsafe schema %q", schema)
	}
	if literal < 1 || literal > 1_000_000 {
		return "", fmt.Errorf("hard-parse literal %d is outside the bounded range", literal)
	}
	return fmt.Sprintf("SELECT count(*),sum(id) FROM %s.fact_sales WHERE id BETWEEN %d AND %d", quoted, literal, literal+99), nil
}

func (s *HardParseScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return fmt.Errorf("hard-parse database is unavailable")
	}
	if s.code < 621 || s.code > 626 || s.name == "" {
		return fmt.Errorf("invalid hard-parse scenario %d", s.code)
	}
	if s.code == 623 {
		if err := rt.Database.ExecSession(ctx, "BEGIN", "SET LOCAL plan_cache_mode=force_custom_plan", "ROLLBACK"); err != nil {
			return fmt.Errorf("force_custom_plan is unsupported: %w", err)
		}
	}
	if s.code == 625 && rt.Journal == nil {
		return fmt.Errorf("mutation journal is unavailable")
	}
	before, err := s.observer.Sample(ctx, rt, s.name)
	if err != nil || !before.Available {
		if err != nil {
			return err
		}
		return fmt.Errorf("direct hard-parse counters are unavailable")
	}
	s.before = before
	if s.code == 622 {
		if err := s.runPreparedControl(ctx, rt); err != nil {
			return err
		}
		controlAfter, err := s.observer.Sample(ctx, rt, s.name)
		if err != nil || !controlAfter.Available {
			if err != nil {
				return err
			}
			return fmt.Errorf("prepared control counters are unavailable")
		}
		s.control = hardParseDelta(s.before, controlAfter)
		s.before = controlAfter
	}
	if s.code != 624 && s.code != 625 {
		s.workload = newSQLWorkloadWithCleanup(ctx, rt, s.name, 1, s.operation(rt), s.cleanup)
	}
	return nil
}

func (s *HardParseScenario) Ramp(context.Context, *Runtime) error { return nil }

func (s *HardParseScenario) Hold(ctx context.Context, rt *Runtime) error {
	if s.code == 625 {
		return s.runInvalidation(ctx, rt)
	}
	if s.code == 624 {
		return s.runFreshSessions(ctx, rt)
	}
	if err := s.workload.SetTarget(1); err != nil {
		return err
	}
	return waitContext(ctx, rt.Config.Run.Duration)
}

func (s *HardParseScenario) Verify(ctx context.Context, rt *Runtime) (Result, error) {
	after, err := s.observer.Sample(ctx, rt, s.name)
	if err != nil {
		return Result{}, err
	}
	s.after = after
	result := EvaluateHardParse(s.name, hardParseDelta(s.before, s.after))
	if s.code == 622 {
		result.Evidence = append(result.Evidence, Evidence{Metric: "prepared_control_hard_parse_delta", Actual: float64(s.control.Hard), Available: s.control.Available})
		if !s.control.Available || hardParseDelta(s.before, s.after).Hard <= s.control.Hard {
			result.Outcome = OutcomeFailed
			result.Message = "simple-query hard parses did not exceed the prepared control window"
		}
	}
	return result, nil
}

func (s *HardParseScenario) Stop(ctx context.Context, _ *Runtime) error {
	if s.workload == nil {
		return nil
	}
	return s.workload.Stop(ctx)
}

func (s *HardParseScenario) Restore(context.Context, *Runtime) error { return nil }

func (s *HardParseScenario) operation(rt *Runtime) SQLWorkerOp {
	return func(ctx context.Context, conn *sql.Conn, _ int) error {
		protocol, err := HardParseProtocolFor(s.code, rt.Config.Data.Schema, s.name)
		if err != nil {
			return err
		}
		if s.code == 621 {
			query, err := protocol.LiteralSQL(s.sequence.Add(1))
			if err != nil {
				return err
			}
			rows, err := conn.QueryContext(ctx, query)
			if err != nil {
				return err
			}
			defer rows.Close()
			return consumeRows(rows)
		}
		switch s.code {
		case 622:
			rows, err := conn.QueryContext(ctx, protocol.SimpleSQL)
			if err != nil {
				return err
			}
			defer rows.Close()
			return consumeRows(rows)
		case 623:
			if _, loaded := s.prepared.LoadOrStore(0, true); !loaded {
				if _, err := conn.ExecContext(ctx, protocol.SetupSQL); err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx, protocol.PrepareSQL); err != nil {
					return err
				}
			}
			rows, err := conn.QueryContext(ctx, protocol.ExecuteSQL)
			if err != nil {
				return err
			}
			defer rows.Close()
			return consumeRows(rows)
		}
		rows, err := conn.QueryContext(ctx, protocol.SimpleSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		return consumeRows(rows)
	}
}

func (s *HardParseScenario) cleanup(ctx context.Context, conn *sql.Conn, _ int) error {
	for _, statement := range []string{"DEALLOCATE ALL", "RESET plan_cache_mode", "RESET no_gpc"} {
		_, err := conn.ExecContext(ctx, statement)
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "unrecognized configuration parameter") {
			return err
		}
	}
	return nil
}

func (s *HardParseScenario) runFreshSessions(ctx context.Context, rt *Runtime) error {
	limit := runtimeInt(rt, "scenario.hardparse_session_churn.sessions", 8)
	if limit < 1 {
		limit = 1
	}
	for index := 0; index < limit; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := rt.Database.OpenTagged(ctx, rt.RunID, s.name, fmt.Sprintf("fresh-%d", index))
		if err != nil {
			return err
		}
		protocol, queryErr := HardParseProtocolFor(s.code, rt.Config.Data.Schema, s.name)
		if queryErr == nil {
			_, queryErr = conn.Conn.ExecContext(ctx, protocol.PrepareSQL)
		}
		if queryErr == nil {
			rows, err := conn.Conn.QueryContext(ctx, protocol.ExecuteSQL)
			if err == nil {
				queryErr = consumeRows(rows)
				_ = rows.Close()
			} else {
				queryErr = err
			}
		}
		if queryErr == nil {
			_, queryErr = conn.Conn.ExecContext(ctx, protocol.CleanupSQL)
		}
		closeErr := conn.Close()
		if queryErr != nil {
			return queryErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (s *HardParseScenario) runPreparedControl(ctx context.Context, rt *Runtime) error {
	conn, err := rt.Database.OpenTagged(ctx, rt.RunID, s.name, "prepared-control")
	if err != nil {
		return err
	}
	defer conn.Close()
	protocol, err := HardParseProtocolFor(622, rt.Config.Data.Schema, s.name)
	if err != nil {
		return err
	}
	if _, err := conn.Conn.ExecContext(ctx, protocol.ControlSQL); err != nil {
		return err
	}
	defer conn.Conn.ExecContext(context.WithoutCancel(ctx), protocol.CleanupSQL)
	for index := 0; index < 4; index++ {
		rows, err := conn.Conn.QueryContext(ctx, protocol.ExecuteSQL)
		if err != nil {
			return err
		}
		if err := consumeRows(rows); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *HardParseScenario) runInvalidation(ctx context.Context, rt *Runtime) error {
	conn, err := rt.Database.OpenTagged(ctx, rt.RunID, s.name, "invalidation")
	if err != nil {
		return err
	}
	defer conn.Close()
	protocol, err := HardParseProtocolFor(625, rt.Config.Data.Schema, s.name)
	if err != nil {
		return err
	}
	if _, err := conn.Conn.ExecContext(ctx, protocol.PrepareSQL); err != nil {
		return err
	}
	defer conn.Conn.ExecContext(context.WithoutCancel(ctx), protocol.CleanupSQL)
	mutations, err := HardParseInvalidationMutations(rt.RunID, rt.Config.Data.Schema)
	if err != nil {
		return err
	}
	for _, mutation := range mutations {
		mutation.Scenario = s.name
		if err := rt.Journal.Apply(ctx, mutation); err != nil {
			return err
		}
		rows, err := conn.Conn.QueryContext(ctx, protocol.ExecuteSQL)
		if err != nil {
			return err
		}
		if err := consumeRows(rows); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

// HardParseInvalidationMutations is the complete, fixed-object DDL journal
// for 625.  Its inverse actions restore the no-index baseline in reverse
// order, including when a run is interrupted between the two DDL changes.
func HardParseInvalidationMutations(runID, schema string) ([]Mutation, error) {
	if strings.TrimSpace(runID) == "" || !tagComponentRE.MatchString(runID) {
		return nil, fmt.Errorf("unsafe run ID %q", runID)
	}
	quoted, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil, fmt.Errorf("unsafe schema %q", schema)
	}
	index := quoted + ".hardparse_invalidation_idx"
	exists := "SELECT count(*) FROM pg_indexes WHERE schemaname='" + schema + "' AND indexname='hardparse_invalidation_idx'"
	return []Mutation{
		{RunID: runID, ScenarioCode: 625, Scenario: "hardparse_ddl_invalidation", Kind: "hardparse_invalidation_create", Target: index, ForwardSQL: "CREATE INDEX hardparse_invalidation_idx ON " + quoted + ".hardparse_targets (lookup_key,id)", InverseSQL: "DROP INDEX IF EXISTS " + index, VerifySQL: exists, VerifyValue: "0"},
		{RunID: runID, ScenarioCode: 625, Scenario: "hardparse_ddl_invalidation", Kind: "hardparse_invalidation_drop", Target: index, ForwardSQL: "DROP INDEX " + index, InverseSQL: "CREATE INDEX hardparse_invalidation_idx ON " + quoted + ".hardparse_targets (lookup_key,id)", VerifySQL: exists, VerifyValue: "1"},
	}, nil
}
