package gsbench

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var errDatabaseRunLockBusy = errors.New("database run lock is busy")

type runLockSession interface {
	TryLock(context.Context, string) (bool, error)
	Unlock(context.Context, string) (bool, error)
	Close() error
	Discard() error
}

func AcquireDatabaseRunLock(
	ctx context.Context,
	db *Database,
	identity string,
) (func() error, error) {
	if db == nil || db.pool == nil {
		return nil, fmt.Errorf("database advisory lock connection is unavailable")
	}
	openSession := db.openAdvisorySession
	if openSession == nil {
		openSession = openAdvisoryLockSession
	}
	return acquireDatabaseRunLock(
		ctx,
		identity,
		func(openCtx context.Context) (runLockSession, error) {
			return openSession(
				openCtx,
				db,
				db.cfg.Database.ApplicationName+"/advisory-lock",
			)
		},
	)
}

func DatabaseRunLockHeld(
	ctx context.Context,
	db *Database,
	identity string,
) (bool, error) {
	if db == nil || db.pool == nil {
		return false, fmt.Errorf("database advisory lock connection is unavailable")
	}
	openSession := db.openAdvisorySession
	if openSession == nil {
		openSession = openAdvisoryLockSession
	}
	return probeDatabaseRunLock(
		ctx,
		identity,
		func(openCtx context.Context) (runLockSession, error) {
			return openSession(
				openCtx,
				db,
				db.cfg.Database.ApplicationName+"/advisory-lock",
			)
		},
	)
}

func probeDatabaseRunLock(
	ctx context.Context,
	identity string,
	open func(context.Context) (runLockSession, error),
) (bool, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return false, fmt.Errorf("database advisory lock identity is required")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if open == nil {
		return false, fmt.Errorf("database advisory lock session opener is required")
	}
	session, err := open(ctx)
	if err != nil {
		return false, fmt.Errorf("open database advisory lock probe session: %w", err)
	}
	acquired, err := session.TryLock(ctx, identity)
	if err != nil {
		return false, errors.Join(
			fmt.Errorf("probe database advisory lock: %w", err),
			session.Discard(),
		)
	}
	if !acquired {
		return true, session.Close()
	}
	released, unlockErr := session.Unlock(context.Background(), identity)
	if unlockErr == nil && !released {
		unlockErr = fmt.Errorf("database advisory lock %q was not held", identity)
	}
	if unlockErr != nil {
		return false, errors.Join(
			fmt.Errorf("release database advisory lock probe: %w", unlockErr),
			session.Discard(),
		)
	}
	return false, session.Close()
}

func acquireDatabaseRunLock(
	ctx context.Context,
	identity string,
	open func(context.Context) (runLockSession, error),
) (func() error, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil, fmt.Errorf("database advisory lock identity is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if open == nil {
		return nil, fmt.Errorf("database advisory lock session opener is required")
	}
	session, err := open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open database advisory lock session: %w", err)
	}
	acquired, err := session.TryLock(ctx, identity)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("acquire database advisory lock: %w", err),
			session.Discard(),
		)
	}
	if !acquired {
		return nil, errors.Join(
			fmt.Errorf("operation already running for lock %q", identity),
			errDatabaseRunLockBusy,
			session.Close(),
		)
	}

	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			released, unlockErr := session.Unlock(context.Background(), identity)
			if unlockErr == nil && !released {
				unlockErr = fmt.Errorf("database advisory lock %q was not held", identity)
			}
			if unlockErr != nil {
				releaseErr = errors.Join(unlockErr, session.Discard())
				return
			}
			releaseErr = session.Close()
		})
		return releaseErr
	}
	return release, nil
}
