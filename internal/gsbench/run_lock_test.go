package gsbench

import (
	"context"
	"errors"
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
