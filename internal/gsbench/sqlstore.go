package gsbench

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"syscall"
)

type dbDatasetExecutor struct {
	db                *Database
	schema            string
	capacityPath      string
	reservedFreeBytes int64
}

func (e dbDatasetExecutor) Exec(ctx context.Context, query string, args ...any) error {
	_, err := e.db.Exec(ctx, query, args...)
	return err
}

func (e dbDatasetExecutor) BatchHighWater(ctx context.Context, table string) (int64, error) {
	var high int64
	err := e.db.Scan(ctx, "SELECT high_water FROM "+e.schema+".meta_batches WHERE table_name=$1", []any{table}, &high)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return high, err
}

func (e dbDatasetExecutor) CheckCapacity(context.Context) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(e.capacityPath, &stat); err != nil {
		return err
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	if free <= e.reservedFreeBytes {
		return fmt.Errorf("free bytes %d reached reserved threshold %d", free, e.reservedFreeBytes)
	}
	return nil
}

type sqlJournalStore struct {
	db     *Database
	schema string
}

func NewSQLJournal(db *Database, schema string) *Journal {
	store := &sqlJournalStore{db: db, schema: schema}
	return NewJournal(store, dbMutationExecutor{db: db})
}

func (s *sqlJournalStore) InsertPlanned(ctx context.Context, m Mutation) (JournalEntry, error) {
	query := "INSERT INTO " + s.schema + `.meta_journal(run_id,scenario,kind,target,original_value,forward_sql,inverse_sql,verify_sql,verify_value,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`
	var id int64
	err := s.db.Scan(ctx, query, []any{m.RunID, m.Scenario, m.Kind, m.Target, m.Original, m.ForwardSQL, m.InverseSQL, m.VerifySQL, m.VerifyValue, string(MutationPlanned)}, &id)
	return JournalEntry{ID: id, Mutation: m, State: MutationPlanned}, err
}

func (s *sqlJournalStore) SetState(ctx context.Context, id int64, state MutationState, detail string) error {
	_, err := s.db.Exec(ctx, "UPDATE "+s.schema+".meta_journal SET state=$1,error_text=$2,updated_at=current_timestamp WHERE id=$3", string(state), detail, id)
	return err
}

func (s *sqlJournalStore) Pending(ctx context.Context, runID string) ([]JournalEntry, error) {
	rows, err := s.db.Query(ctx, "SELECT id,run_id,scenario,kind,target,original_value,forward_sql,inverse_sql,verify_sql,verify_value,state,COALESCE(error_text,'') FROM "+s.schema+".meta_journal WHERE run_id=$1 AND state<>$2 ORDER BY id", runID, string(MutationRestored))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []JournalEntry
	for rows.Next() {
		var entry JournalEntry
		var state string
		if err := rows.Scan(&entry.ID, &entry.RunID, &entry.Scenario, &entry.Kind, &entry.Target, &entry.Original, &entry.ForwardSQL, &entry.InverseSQL, &entry.VerifySQL, &entry.VerifyValue, &state, &entry.Error); err != nil {
			return nil, err
		}
		entry.State = MutationState(state)
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *sqlJournalStore) StaleRuns(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, "SELECT DISTINCT run_id FROM "+s.schema+".meta_journal WHERE state<>$1 ORDER BY run_id", string(MutationRestored))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []string
	for rows.Next() {
		var run string
		if err := rows.Scan(&run); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

type dbMutationExecutor struct{ db *Database }

func (e dbMutationExecutor) Exec(ctx context.Context, query string) error {
	_, err := e.db.Exec(ctx, query)
	return err
}
func (e dbMutationExecutor) Verify(ctx context.Context, query, expected string) error {
	var actual string
	if err := e.db.Scan(ctx, query, nil, &actual); err != nil {
		return err
	}
	if !databaseValuesEqual(actual, expected) {
		return fmt.Errorf("got %q, want %q", actual, expected)
	}
	return nil
}
func databaseValuesEqual(actual, expected string) bool {
	normalize := func(v string) string {
		v = strings.ToLower(strings.TrimSpace(v))
		switch v {
		case "t", "on", "1":
			return "true"
		case "f", "off", "0":
			return "false"
		}
		return v
	}
	return normalize(actual) == normalize(expected)
}
