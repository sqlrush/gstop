package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type runLockSession interface {
	TryLock(context.Context, string) (bool, error)
	Unlock(context.Context, string) (bool, error)
	Close() error
	Discard() error
}

type sqlRunLockSession struct {
	db   *Database
	conn *sql.Conn
}

func (s *sqlRunLockSession) TryLock(ctx context.Context, key string) (bool, error) {
	opCtx, cancel := s.db.operationContext(ctx)
	defer cancel()
	var acquired bool
	err := s.conn.QueryRowContext(
		opCtx,
		"SELECT pg_try_advisory_lock(hashtext($1))",
		key,
	).Scan(&acquired)
	return acquired, err
}

func (s *sqlRunLockSession) Unlock(ctx context.Context, key string) (bool, error) {
	opCtx, cancel := s.db.operationContext(ctx)
	defer cancel()
	var released bool
	err := s.conn.QueryRowContext(
		opCtx,
		"SELECT pg_advisory_unlock(hashtext($1))",
		key,
	).Scan(&released)
	return released, err
}

func (s *sqlRunLockSession) Close() error { return s.conn.Close() }

func (s *sqlRunLockSession) Discard() error {
	// Mark the physical connection bad before closing the sql.Conn wrapper, so
	// a session whose lock state is uncertain can never return to the pool.
	discardErr := discardSessionConnection(s.conn)
	closeErr := normalizeConnectionCloseError(s.conn.Close())
	return errors.Join(discardErr, closeErr)
}

func AcquireDatabaseRunLock(
	ctx context.Context,
	db *Database,
	identity string,
) (func() error, error) {
	if db == nil || db.pool == nil {
		return nil, fmt.Errorf("database advisory lock connection is unavailable")
	}
	return acquireDatabaseRunLock(
		ctx,
		identity,
		func(openCtx context.Context) (runLockSession, error) {
			opCtx, cancel := db.operationContext(openCtx)
			defer cancel()
			conn, err := db.pool.Conn(opCtx)
			if err != nil {
				return nil, err
			}
			return &sqlRunLockSession{db: db, conn: conn}, nil
		},
	)
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
