package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	pq "gitcode.com/opengauss/openGauss-connector-go-pq"
)

type advisoryLockOperation string

const (
	advisoryTryLock       advisoryLockOperation = "try"
	advisoryUnlock        advisoryLockOperation = "unlock"
	advisoryTryLockShared advisoryLockOperation = "try_shared"
	advisoryUnlockShared  advisoryLockOperation = "unlock_shared"
)

type advisoryLockSession interface {
	TryLock(context.Context, string) (bool, error)
	Unlock(context.Context, string) (bool, error)
	Scan(context.Context, string, []any, ...any) error
	Exec(context.Context, string, ...any) error
	Close() error
	Discard() error
}

type advisoryLockSessionOpener func(
	context.Context,
	*Database,
	string,
) (advisoryLockSession, error)

type sqlAdvisoryLockSession struct {
	db       *Database
	pool     *sql.DB
	conn     *sql.Conn
	once     sync.Once
	closeErr error
}

func sessionAdvisoryLockQuery(
	operation advisoryLockOperation,
	key string,
) (string, error) {
	var function string
	switch operation {
	case advisoryTryLock:
		function = "pg_try_advisory_lock"
	case advisoryUnlock:
		function = "pg_advisory_unlock"
	case advisoryTryLockShared:
		function = "pg_try_advisory_lock_shared"
	case advisoryUnlockShared:
		function = "pg_advisory_unlock_shared"
	default:
		return "", fmt.Errorf(
			"unsupported advisory lock operation %q",
			operation,
		)
	}
	return "SELECT " + function + "(hashtext(" +
		pq.QuoteLiteral(key) + "))", nil
}

func newSQLAdvisoryLockSession(
	ctx context.Context,
	db *Database,
	pool *sql.DB,
) (advisoryLockSession, error) {
	if db == nil || pool == nil {
		return nil, fmt.Errorf("database advisory lock connection is unavailable")
	}
	opCtx, cancel := db.operationContext(ctx)
	defer cancel()
	conn, err := pool.Conn(opCtx)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("open database advisory lock session: %w", err),
			normalizeConnectionCloseError(pool.Close()),
		)
	}
	return &sqlAdvisoryLockSession{db: db, pool: pool, conn: conn}, nil
}

func openAdvisoryLockSession(
	ctx context.Context,
	db *Database,
	applicationName string,
) (advisoryLockSession, error) {
	if db == nil {
		return nil, fmt.Errorf("database advisory lock connection is unavailable")
	}
	pool, err := sql.Open(
		benchDriverName,
		db.cfg.DSN(db.cfg.Database.Database, applicationName),
	)
	if err != nil {
		return nil, fmt.Errorf("open database advisory lock pool: %w", err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	return newSQLAdvisoryLockSession(ctx, db, pool)
}

func (s *sqlAdvisoryLockSession) TryLock(
	ctx context.Context,
	key string,
) (bool, error) {
	return s.queryLock(ctx, advisoryTryLock, key)
}

func (s *sqlAdvisoryLockSession) Unlock(
	ctx context.Context,
	key string,
) (bool, error) {
	return s.queryLock(ctx, advisoryUnlock, key)
}

func (s *sqlAdvisoryLockSession) TryLockShared(
	ctx context.Context,
	key string,
) (bool, error) {
	return s.queryLock(ctx, advisoryTryLockShared, key)
}

func (s *sqlAdvisoryLockSession) UnlockShared(
	ctx context.Context,
	key string,
) (bool, error) {
	return s.queryLock(ctx, advisoryUnlockShared, key)
}

func (s *sqlAdvisoryLockSession) queryLock(
	ctx context.Context,
	operation advisoryLockOperation,
	key string,
) (bool, error) {
	query, err := sessionAdvisoryLockQuery(operation, key)
	if err != nil {
		return false, err
	}
	opCtx, cancel := s.db.operationContext(ctx)
	defer cancel()
	var result bool
	err = s.conn.QueryRowContext(opCtx, query).Scan(&result)
	return result, err
}

func (s *sqlAdvisoryLockSession) Scan(
	ctx context.Context,
	query string,
	args []any,
	dest ...any,
) error {
	opCtx, cancel := s.db.operationContext(ctx)
	defer cancel()
	return s.conn.QueryRowContext(opCtx, query, args...).Scan(dest...)
}

func (s *sqlAdvisoryLockSession) Exec(
	ctx context.Context,
	query string,
	args ...any,
) error {
	opCtx, cancel := s.db.operationContext(ctx)
	defer cancel()
	_, err := s.conn.ExecContext(opCtx, query, args...)
	return err
}

func (s *sqlAdvisoryLockSession) Close() error {
	return s.finish(false)
}

func (s *sqlAdvisoryLockSession) Discard() error {
	return s.finish(true)
}

func (s *sqlAdvisoryLockSession) finish(discard bool) error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		var errs []error
		if s.conn != nil {
			if discard {
				errs = append(errs, discardSessionConnection(s.conn))
			}
			errs = append(
				errs,
				normalizeConnectionCloseError(s.conn.Close()),
			)
			s.conn = nil
		}
		if s.pool != nil {
			errs = append(
				errs,
				normalizeConnectionCloseError(s.pool.Close()),
			)
			s.pool = nil
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

func isRetryableAdvisoryLockError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	var sqlStateErr interface{ SQLState() string }
	if errors.As(err, &sqlStateErr) {
		state := strings.ToUpper(strings.TrimSpace(sqlStateErr.SQLState()))
		if strings.HasPrefix(state, "08") {
			return true
		}
		switch state {
		case "53200", "53300", "57P01":
			return true
		}
	}
	return strings.Contains(
		strings.ToLower(err.Error()),
		"memory is temporarily unavailable",
	)
}
