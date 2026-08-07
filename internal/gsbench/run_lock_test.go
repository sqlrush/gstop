package gsbench

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeRunLockSession struct {
	tryResult    bool
	tryErr       error
	unlockResult bool
	unlockErr    error
	keys         []string
	closed       int
	discarded    int
}

func (s *fakeRunLockSession) TryLockShared(
	_ context.Context,
	key string,
) (bool, error) {
	s.keys = append(s.keys, "try-shared:"+key)
	return s.tryResult, s.tryErr
}

func (s *fakeRunLockSession) UnlockShared(
	_ context.Context,
	key string,
) (bool, error) {
	s.keys = append(s.keys, "unlock-shared:"+key)
	return s.unlockResult, s.unlockErr
}

func (s *fakeRunLockSession) TryLock(_ context.Context, key string) (bool, error) {
	s.keys = append(s.keys, "try:"+key)
	return s.tryResult, s.tryErr
}

func (s *fakeRunLockSession) Unlock(_ context.Context, key string) (bool, error) {
	s.keys = append(s.keys, "unlock:"+key)
	return s.unlockResult, s.unlockErr
}

func (s *fakeRunLockSession) Close() error {
	s.closed++
	return nil
}

func (s *fakeRunLockSession) Discard() error {
	s.discarded++
	s.closed++
	return nil
}

func TestDatabaseRunLockUsesOneSessionForAcquireAndRelease(t *testing.T) {
	session := &fakeRunLockSession{tryResult: true, unlockResult: true}
	release, err := acquireDatabaseRunLock(
		context.Background(),
		"gsbench:init:postgres:gsbench_e2e_20260801",
		func(context.Context) (runLockSession, error) { return session, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"try:gsbench:init:postgres:gsbench_e2e_20260801",
		"unlock:gsbench:init:postgres:gsbench_e2e_20260801",
	}
	if strings.Join(session.keys, ",") != strings.Join(want, ",") ||
		session.closed != 1 || session.discarded != 0 {
		t.Fatalf(
			"keys=%v closed=%d discarded=%d",
			session.keys,
			session.closed,
			session.discarded,
		)
	}
}

func TestDatabaseRunLockFailsClearlyWhenBusy(t *testing.T) {
	session := &fakeRunLockSession{}
	_, err := acquireDatabaseRunLock(
		context.Background(),
		"gsbench:init:postgres:gsbench_e2e_20260801",
		func(context.Context) (runLockSession, error) { return session, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("error=%v", err)
	}
	if session.closed != 1 {
		t.Fatalf("busy session close count=%d", session.closed)
	}
	if session.discarded != 0 {
		t.Fatalf("busy session discard count=%d", session.discarded)
	}
}

func TestDatabaseRunLockDiscardsSessionWhenAcquireResultIsUncertain(t *testing.T) {
	session := &fakeRunLockSession{tryErr: errors.New("try query failed")}
	_, err := acquireDatabaseRunLock(
		context.Background(),
		"gsbench:init:postgres:gsbench_e2e_20260801",
		func(context.Context) (runLockSession, error) { return session, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "try query failed") {
		t.Fatalf("acquire error=%v", err)
	}
	if session.closed != 1 || session.discarded != 1 {
		t.Fatalf(
			"uncertain acquire closed=%d discarded=%d want closed=1 discarded=1",
			session.closed,
			session.discarded,
		)
	}
}

func TestDatabaseRunLockDiscardsSessionWhenReleaseResultIsUncertain(t *testing.T) {
	for _, test := range []struct {
		name         string
		unlockResult bool
		unlockErr    error
		wantError    string
	}{
		{
			name:      "unlock query error",
			unlockErr: errors.New("unlock failed"),
			wantError: "unlock failed",
		},
		{
			name:         "lock not held",
			unlockResult: false,
			wantError:    "was not held",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeRunLockSession{
				tryResult:    true,
				unlockResult: test.unlockResult,
				unlockErr:    test.unlockErr,
			}
			release, err := acquireDatabaseRunLock(
				context.Background(),
				"gsbench:init:postgres:gsbench_e2e_20260801",
				func(context.Context) (runLockSession, error) { return session, nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := release(); err == nil ||
				!strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("release error=%v", err)
			}
			if session.closed != 1 || session.discarded != 1 {
				t.Fatalf(
					"uncertain release closed=%d discarded=%d want closed=1 discarded=1",
					session.closed,
					session.discarded,
				)
			}
		})
	}
}

func TestProbeDatabaseRunLockReportsAnotherSessionHoldingLock(t *testing.T) {
	session := &fakeRunLockSession{tryResult: false}
	held, err := probeDatabaseRunLock(
		context.Background(),
		"gsbench:plan-workload:postgres:gsbench",
		func(context.Context) (runLockSession, error) { return session, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("held=false want true")
	}
	if session.closed != 1 || session.discarded != 0 {
		t.Fatalf("closed=%d discarded=%d", session.closed, session.discarded)
	}
	if strings.Join(session.keys, ",") !=
		"try:gsbench:plan-workload:postgres:gsbench" {
		t.Fatalf("events=%v", session.keys)
	}
}

func TestProbeDatabaseRunLockReleasesProbeAcquisition(t *testing.T) {
	session := &fakeRunLockSession{tryResult: true, unlockResult: true}
	held, err := probeDatabaseRunLock(
		context.Background(),
		"gsbench:plan-workload:postgres:gsbench",
		func(context.Context) (runLockSession, error) { return session, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("held=true want false")
	}
	want := "try:gsbench:plan-workload:postgres:gsbench," +
		"unlock:gsbench:plan-workload:postgres:gsbench"
	if strings.Join(session.keys, ",") != want ||
		session.closed != 1 || session.discarded != 0 {
		t.Fatalf(
			"events=%v closed=%d discarded=%d",
			session.keys,
			session.closed,
			session.discarded,
		)
	}
}

func TestProbeDatabaseRunLockDiscardsUncertainProbe(t *testing.T) {
	session := &fakeRunLockSession{
		tryResult: true,
		unlockErr: errors.New("unlock failed"),
	}
	_, err := probeDatabaseRunLock(
		context.Background(),
		"gsbench:plan-workload:postgres:gsbench",
		func(context.Context) (runLockSession, error) { return session, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "unlock failed") {
		t.Fatalf("error=%v", err)
	}
	if session.discarded != 1 {
		t.Fatalf("discarded=%d want 1", session.discarded)
	}
}

func TestWithRunExecutionDatabaseLockHoldsLeaseUntilRunCompletes(
	t *testing.T,
) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	var events []string

	code, err := withRunExecutionDatabaseLock(
		context.Background(),
		nil,
		cfg,
		"run-101",
		func(
			_ context.Context,
			_ *Database,
			identity string,
		) (func() error, error) {
			events = append(events, "acquire "+identity)
			return func() error {
				events = append(events, "release "+identity)
				return nil
			}, nil
		},
		func() int {
			events = append(events, "run workload and restore")
			return 3
		},
	)
	if err != nil || code != 3 {
		t.Fatalf("exit code=%d error=%v", code, err)
	}
	want := []string{
		"acquire gsbench:run:postgres:Bench:run-101",
		"run workload and restore",
		"release gsbench:run:postgres:Bench:run-101",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestDatasetLifecycleLockSerializesRunRegistrationAndDataCleanup(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	var events []string
	err := withDatasetLifecycleDatabaseLock(
		context.Background(), nil, cfg,
		func(_ context.Context, _ *Database, identity string) (func() error, error) {
			events = append(events, "acquire "+identity)
			return func() error {
				events = append(events, "release "+identity)
				return nil
			}, nil
		},
		func() error {
			events = append(events, "protected operation")
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"acquire gsbench:dataset-lifecycle:postgres:Bench",
		"protected operation",
		"release gsbench:dataset-lifecycle:postgres:Bench",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestDatasetExecutionLeaseUsesSharedLifecycleLock(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	session := &fakeRunLockSession{tryResult: true, unlockResult: true}
	release, err := acquireDatabaseSharedRunLock(
		context.Background(),
		"gsbench:dataset-lifecycle:postgres:Bench",
		func(context.Context) (runLockSession, error) { return session, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"try-shared:" + datasetLifecycleLockIdentity(cfg),
		"unlock-shared:" + datasetLifecycleLockIdentity(cfg),
	}
	if !reflect.DeepEqual(session.keys, want) {
		t.Fatalf("events=%v want=%v", session.keys, want)
	}
}

func TestAutomaticRecoveryExcludesRunsWithLiveExecutionLease(t *testing.T) {
	backend := &databaseRestoreBackend{
		cfg: BenchConfig{
			Database: DatabaseConfig{Database: "postgres"},
			Data:     DataConfig{Schema: "Bench"},
		},
		runExecutionLeaseHeld: func(
			_ context.Context,
			runID string,
		) (bool, error) {
			return runID == "live-run", nil
		},
	}
	discovery := RestoreDiscovery{
		Runs: []RestoreRun{
			{RunID: "live-run", ScenarioCodes: []ScenarioCode{101}},
			{RunID: "stale-run", ScenarioCodes: []ScenarioCode{102}},
		},
		DatabaseActions: []Action{
			{RunID: "live-run", Sequence: 1},
			{RunID: "stale-run", Sequence: 2},
		},
		LocalActions: []Action{
			{RunID: "live-run", Sequence: 3},
			{RunID: "stale-run", Sequence: 4},
		},
	}

	got, err := backend.excludeLiveExecutionRuns(
		context.Background(),
		discovery,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != 1 || got.Runs[0].RunID != "stale-run" {
		t.Fatalf("runs=%+v", got.Runs)
	}
	if len(got.DatabaseActions) != 1 ||
		got.DatabaseActions[0].RunID != "stale-run" {
		t.Fatalf("database actions=%+v", got.DatabaseActions)
	}
	if len(got.LocalActions) != 1 ||
		got.LocalActions[0].RunID != "stale-run" {
		t.Fatalf("local actions=%+v", got.LocalActions)
	}
}
