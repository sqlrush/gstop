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
	if strings.Join(session.keys, ",") != strings.Join(want, ",") || session.closed != 1 {
		t.Fatalf("keys=%v closed=%d", session.keys, session.closed)
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
}

func TestDatabaseRunLockReturnsReleaseErrorsAndStillCloses(t *testing.T) {
	session := &fakeRunLockSession{
		tryResult: true, unlockErr: errors.New("unlock failed"),
	}
	release, err := acquireDatabaseRunLock(
		context.Background(),
		"gsbench:init:postgres:gsbench_e2e_20260801",
		func(context.Context) (runLockSession, error) { return session, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err == nil || !strings.Contains(err.Error(), "unlock failed") {
		t.Fatalf("release error=%v", err)
	}
	if session.closed != 1 {
		t.Fatalf("release close count=%d", session.closed)
	}
}
