package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestJournalSQLUsesCompositeKeyAndFormalActionColumns(t *testing.T) {
	insert := journalInsertStatement(`"Bench"`)
	for _, token := range []string{
		`INSERT INTO "Bench".meta_journal`,
		"COALESCE(max(j.action_id),0)+1",
		"scenario_code",
		"action_kind",
		"target_product",
		"target_node",
		"target_endpoint",
		"original_state",
		"forward_action",
		"inverse_action",
		"verify_action",
		"last_error",
		"RETURNING action_id",
	} {
		if !strings.Contains(insert, token) {
			t.Errorf("journal insert missing %q:\n%s", token, insert)
		}
	}
	if strings.Contains(insert, "run-1") {
		t.Fatal("journal insert embeds values instead of parameters")
	}
	if strings.Contains(insert, "pg_advisory_xact_lock") {
		t.Fatalf("allocation lock shares insert snapshot:\n%s", insert)
	}
	if lock := journalLockStatement(); lock !=
		"SELECT pg_advisory_xact_lock(hashtext($1))" {
		t.Fatalf("journal lock statement=%q", lock)
	}
	state := journalStateStatement(`"Bench"`)
	if !strings.Contains(state, "WHERE run_id=$3 AND action_id=$4") {
		t.Fatalf("journal state update does not use composite key:\n%s", state)
	}
}

type fakeJournalDatabase struct {
	scanQuery    string
	scanArgs     []any
	execQuery    string
	execArgs     []any
	execQueries  []string
	queryRows    [][]any
	rowsAffected int64
	rejectClaim  bool
}

type fakeJournalResult int64

func (fakeJournalResult) LastInsertId() (int64, error) {
	return 0, errors.New("unsupported")
}
func (r fakeJournalResult) RowsAffected() (int64, error) {
	return int64(r), nil
}

type fakeJournalTransaction struct {
	db *fakeJournalDatabase
}

func (t *fakeJournalTransaction) ExecContext(
	ctx context.Context,
	query string,
	args ...any,
) (sql.Result, error) {
	return t.db.Exec(ctx, query, args...)
}

func (t *fakeJournalTransaction) ScanContext(
	ctx context.Context,
	query string,
	args []any,
	dest ...any,
) error {
	return t.db.Scan(ctx, query, args, dest...)
}

func (*fakeJournalTransaction) Commit() error   { return nil }
func (*fakeJournalTransaction) Rollback() error { return nil }

func (d *fakeJournalDatabase) BeginJournalTransaction(
	context.Context,
) (journalSQLTransaction, error) {
	return &fakeJournalTransaction{db: d}, nil
}

func (d *fakeJournalDatabase) Scan(_ context.Context, query string, args []any, dest ...any) error {
	d.scanQuery = query
	d.scanArgs = append([]any(nil), args...)
	*(dest[0].(*int64)) = 7
	return nil
}

func (d *fakeJournalDatabase) Exec(_ context.Context, query string, args ...any) (sql.Result, error) {
	d.execQuery = query
	d.execArgs = append([]any(nil), args...)
	d.execQueries = append(d.execQueries, query)
	affected := d.rowsAffected
	if d.rejectClaim {
		affected = 0
	} else if affected == 0 {
		affected = 1
	}
	return fakeJournalResult(affected), nil
}

func (d *fakeJournalDatabase) Query(_ context.Context, query string, args ...any) (journalRows, error) {
	if strings.Contains(query, "SELECT DISTINCT run_id") {
		return &sliceJournalRows{rows: [][]any{{"run-1"}}}, nil
	}
	if d.queryRows != nil {
		return &sliceJournalRows{rows: d.queryRows}, nil
	}
	return &sliceJournalRows{rows: [][]any{{
		int64(7), "run-1", int64(601), "SQL_MUTATION", "GaussDB",
		"dn_1", "gsbench.plan_data", `{"value":"before"}`,
		`{"sql":"forward"}`, `{"sql":"inverse"}`,
		`{"sql":"verify","expected":"1"}`, "", "applied", "",
	}}}, nil
}

type sliceJournalRows struct {
	rows  [][]any
	index int
}

type fakeDatasetTransaction struct {
	queries    []string
	args       [][]any
	failCall   int
	committed  bool
	rolledBack bool
}

func (t *fakeDatasetTransaction) ExecContext(
	_ context.Context,
	query string,
	args ...any,
) (sql.Result, error) {
	t.queries = append(t.queries, query)
	t.args = append(t.args, append([]any(nil), args...))
	if len(t.queries) == t.failCall {
		return nil, errors.New("transaction statement failed")
	}
	return nil, nil
}

func (t *fakeDatasetTransaction) Commit() error {
	t.committed = true
	return nil
}

func (t *fakeDatasetTransaction) Rollback() error {
	t.rolledBack = true
	return nil
}

type fakeDatasetTransactionBeginner struct {
	tx *fakeDatasetTransaction
}

func (b fakeDatasetTransactionBeginner) BeginDatasetTransaction(
	context.Context,
) (datasetSQLTransaction, error) {
	return b.tx, nil
}

func (r *sliceJournalRows) Next() bool { return r.index < len(r.rows) }
func (r *sliceJournalRows) Close() error {
	return nil
}
func (r *sliceJournalRows) Err() error { return nil }
func (r *sliceJournalRows) Scan(dest ...any) error {
	values := r.rows[r.index]
	r.index++
	if len(values) != len(dest) {
		return fmt.Errorf(
			"row has %d values, scan has %d destinations",
			len(values),
			len(dest),
		)
	}
	for i, value := range values {
		target := reflect.ValueOf(dest[i]).Elem()
		source := reflect.ValueOf(value)
		if source.Type().ConvertibleTo(target.Type()) {
			source = source.Convert(target.Type())
		}
		target.Set(source)
	}
	return nil
}

func TestSQLActionStoreUsesParameterizedJSONAndScansTypedAction(t *testing.T) {
	db := &fakeJournalDatabase{rowsAffected: 1}
	store := newSQLJournalStore(db, "gsbench")
	if store.schema != `"gsbench"` {
		t.Fatalf("journal schema=%q", store.schema)
	}
	entry, err := store.InsertPlanned(context.Background(), Action{
		RunID:         "run-1",
		ScenarioCode:  601,
		Kind:          ActionSQLMutation,
		TargetProduct: ProductGaussDB,
		Node:          "dn_1",
		Target:        "gsbench.plan_data",
		Original:      []byte(`{"value":"before"}`),
		Forward:       []byte(`{"sql":"forward"}`),
		Inverse:       []byte(`{"sql":"inverse"}`),
		Verify:        []byte(`{"sql":"verify","expected":"1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Sequence != 7 || len(db.scanArgs) != 13 || db.scanArgs[0] != "run-1" {
		t.Fatalf("planned entry=%+v args=%v", entry, db.scanArgs)
	}
	wantPayloadArgs := map[int]any{
		6: `{"value":"before"}`,
		7: `{"sql":"forward"}`,
		8: `{"sql":"inverse"}`,
		9: `{"sql":"verify","expected":"1"}`,
	}
	for index, want := range wantPayloadArgs {
		if db.scanArgs[index] != want {
			t.Errorf("insert arg %d = %#v, want %#v", index+1, db.scanArgs[index], want)
		}
	}
	pending, err := store.Pending(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Sequence != 7 ||
		pending[0].ScenarioCode != 601 || pending[0].TargetProduct != ProductGaussDB {
		t.Fatalf("pending=%+v", pending)
	}
	if string(pending[0].Forward) != `{"sql":"forward"}` ||
		string(pending[0].Verify) != `{"sql":"verify","expected":"1"}` {
		t.Fatalf("pending payloads forward=%s verify=%s",
			pending[0].Forward, pending[0].Verify)
	}
	if err := store.SetState(context.Background(), "run-1", 7, MutationRestored, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(db.execArgs, []any{string(MutationRestored), nil, "run-1", int64(7)}) {
		t.Fatalf("state args=%v", db.execArgs)
	}
	runs, err := store.StaleRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runs, []string{"run-1"}) {
		t.Fatalf("stale runs=%v", runs)
	}
}

func TestSQLActionStoreClaimsRestoreWithCompareAndSet(t *testing.T) {
	action := validSQLJournalAction()
	action.Sequence = 7
	action.State = MutationApplied
	db := &fakeJournalDatabase{rowsAffected: 1}
	store := newSQLJournalStore(db, "gsbench")

	claimed, err := store.ClaimAction(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("first compare-and-set claim was not acquired")
	}
	for _, token := range []string{
		"SET state=$1",
		"WHERE run_id=$3 AND action_id=$4 AND state=$5",
	} {
		if !strings.Contains(db.execQuery, token) {
			t.Fatalf("claim SQL missing %q:\n%s", token, db.execQuery)
		}
	}
	if !reflect.DeepEqual(
		db.execArgs,
		[]any{
			string(MutationRestoring),
			nil,
			action.RunID,
			action.Sequence,
			string(MutationApplied),
		},
	) {
		t.Fatalf("claim args=%v", db.execArgs)
	}

	db.rejectClaim = true
	claimed, err = store.ClaimAction(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("stale compare-and-set claim was acquired")
	}
}

func TestSQLActionStoreRejectsUnsafeSchemaBeforeDatabaseUse(t *testing.T) {
	db := &fakeJournalDatabase{}
	store := newSQLJournalStore(db, `bench";DROP SCHEMA public`)
	_, err := store.InsertPlanned(context.Background(), Action{
		RunID:        "run-1",
		ScenarioCode: 601,
		Kind:         ActionSQLMutation,
		Target:       "gsbench.plan_data",
		Forward:      []byte(`{"sql":"SELECT 1"}`),
		Inverse:      []byte(`{"sql":"SELECT 1"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe journal schema") {
		t.Fatalf("InsertPlanned() error = %v", err)
	}
	if db.scanQuery != "" || db.execQuery != "" {
		t.Fatalf("unsafe schema reached database: scan=%q exec=%q",
			db.scanQuery, db.execQuery)
	}
}

func TestSQLActionStoreRejectsUnknownProduct(t *testing.T) {
	db := &fakeJournalDatabase{}
	store := newSQLJournalStore(db, "gsbench")
	_, err := store.InsertPlanned(context.Background(), Action{
		RunID: "run-1", ScenarioCode: 601, Kind: ActionSQLMutation,
		TargetProduct: ProductUnknown, Target: "gsbench.plan_data",
		Forward: []byte(`{"sql":"SELECT 1"}`),
		Inverse: []byte(`{"sql":"SELECT 1"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "target product") {
		t.Fatalf("InsertPlanned() error = %v", err)
	}
	if db.scanQuery != "" || db.execQuery != "" {
		t.Fatalf("unknown product reached database: scan=%q exec=%q",
			db.scanQuery, db.execQuery)
	}
}

func TestNewSQLJournalUsesDetectedDatabaseProduct(t *testing.T) {
	db := &Database{}
	db.setTargetProduct(ProductOpenGauss)
	journal := NewSQLJournal(db, "gsbench")
	if journal.targetProduct != ProductOpenGauss {
		t.Fatalf("journal target product = %q", journal.targetProduct)
	}
}

func TestSQLActionStorePersistsEmptyJSONInverseForNonPersistentAction(t *testing.T) {
	db := &fakeJournalDatabase{}
	store := newSQLJournalStore(db, "gsbench")
	_, err := store.InsertPlanned(context.Background(), Action{
		RunID:         "run-1",
		ScenarioCode:  401,
		Kind:          ActionSessionSet,
		TargetProduct: ProductGaussDB,
		Target:        "session.statement_timeout",
		Forward:       []byte(`{"name":"statement_timeout","value":"1s"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if db.scanArgs[8] != "{}" {
		t.Fatalf("inverse action arg = %#v, want empty JSON object", db.scanArgs[8])
	}
}

type fakeSQLActionDatabase struct {
	executed []string
	actual   string
}

func (d *fakeSQLActionDatabase) Exec(
	_ context.Context,
	query string,
	_ ...any,
) (sql.Result, error) {
	d.executed = append(d.executed, query)
	return nil, nil
}

func (d *fakeSQLActionDatabase) Scan(
	_ context.Context,
	_ string,
	_ []any,
	dest ...any,
) error {
	*(dest[0].(*string)) = d.actual
	return nil
}

func TestSQLActionExecutorUsesTypedPayloadsForApplyRestoreAndVerify(t *testing.T) {
	db := &fakeSQLActionDatabase{actual: "on"}
	executor := dbActionExecutor{db: db}
	action := Action{
		Kind:    ActionSQLMutation,
		Forward: []byte(`{"sql":"ALTER SYSTEM SET enable_thread_pool TO on"}`),
		Inverse: []byte(`{"sql":"ALTER SYSTEM SET enable_thread_pool TO off"}`),
		Verify:  []byte(`{"sql":"SHOW enable_thread_pool","expected":"off"}`),
	}
	if err := executor.Apply(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := executor.Restore(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	db.actual = "off"
	if err := executor.VerifyRestored(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(db.executed, []string{
		"ALTER SYSTEM SET enable_thread_pool TO on",
		"ALTER SYSTEM SET enable_thread_pool TO off",
	}) {
		t.Fatalf("executed = %v", db.executed)
	}
}

func TestSQLActionExecutorVerifiesRestoredPlanIndexesByCanonicalShape(
	t *testing.T,
) {
	tests := []struct {
		name         string
		scenario     string
		correctShape string
		wrongShape   string
	}{
		{
			name:         "605 dropped index",
			scenario:     "planchange_index_drop",
			correctShape: `CREATE INDEX plan_index_drop_idx ON "gsbench".plan_data USING btree (index_drop_key, dist_key, id)`,
			wrongShape:   `CREATE INDEX plan_index_drop_idx ON "gsbench".plan_data USING btree (index_drop_key)`,
		},
		{
			name:         "606 good-shape index",
			scenario:     "planchange_index_shape",
			correctShape: `CREATE INDEX plan_index_shape_good_idx ON "gsbench".plan_data USING btree (index_shape_lead, index_shape_tail, dist_key, id)`,
			wrongShape:   `CREATE INDEX plan_index_shape_good_idx ON "gsbench".plan_data USING btree (index_shape_lead, index_shape_tail)`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutations, err := PlanMutationSet("run-1", "gsbench", test.scenario)
			if err != nil {
				t.Fatal(err)
			}
			action := SQLAction(mutations[0])
			db := &fakeSQLActionDatabase{actual: test.correctShape}
			executor := dbActionExecutor{db: db}
			if err := executor.VerifyRestored(context.Background(), action); err != nil {
				t.Fatalf("canonical index shape rejected: %v", err)
			}
			db.actual = test.wrongShape
			if err := executor.VerifyRestored(context.Background(), action); err == nil ||
				!strings.Contains(err.Error(), "index definition") {
				t.Fatalf("wrong-shaped same-name index accepted: %v", err)
			}
		})
	}
}

func TestDatasetIndexMatchesRejectsEqualColumnSemanticDifferences(t *testing.T) {
	expected := `CREATE INDEX plan_index_drop_idx ON "gsbench".plan_data (index_drop_key, dist_key, id)`
	if !datasetIndexMatches(
		`CREATE INDEX plan_index_drop_idx ON gsbench.plan_data USING btree (index_drop_key, dist_key, id) TABLESPACE gsbench_ts`,
		expected,
	) {
		t.Fatal("explicit default btree/tablespace definition did not match canonical index")
	}
	for _, actual := range []string{
		`CREATE UNIQUE INDEX plan_index_drop_idx ON gsbench.plan_data USING btree (index_drop_key, dist_key, id)`,
		`CREATE INDEX plan_index_drop_idx ON gsbench.plan_data USING hash (index_drop_key, dist_key, id)`,
		`CREATE INDEX plan_index_drop_idx ON gsbench.plan_data USING btree (index_drop_key, dist_key, id) WHERE index_drop_key > 0`,
		`CREATE INDEX plan_index_drop_idx ON gsbench.plan_data USING btree (index_drop_key, dist_key, id) WITH (fillfactor=70)`,
	} {
		if datasetIndexMatches(actual, expected) {
			t.Fatalf("semantically different same-column index accepted: %s", actual)
		}
	}
}

func TestSQLActionExecutorRejectsNonSQLActionKind(t *testing.T) {
	db := &fakeSQLActionDatabase{}
	executor := dbActionExecutor{db: db}
	err := executor.Apply(context.Background(), Action{
		Kind:    ActionNetworkFirewall,
		Forward: []byte(`{"sql":"SELECT 1"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "NETWORK_FIREWALL") {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(db.executed) != 0 {
		t.Fatalf("non-SQL action executed statements %v", db.executed)
	}
}

func TestSQLActionExecutorRestoresDataBaselineThroughSameEngine(t *testing.T) {
	db := &fakeSQLActionDatabase{actual: "restored"}
	executor := dbActionExecutor{db: db}
	action := Action{
		Kind:    ActionDataBaseline,
		Forward: []byte(`{"sql":"UPDATE gsbench.targets SET value='fault'"}`),
		Inverse: []byte(`{"sql":"UPDATE gsbench.targets SET value='restored'"}`),
		Verify: []byte(
			`{"sql":"SELECT value FROM gsbench.targets","expected":"restored"}`,
		),
	}
	if err := executor.Preflight(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := executor.Restore(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := executor.VerifyRestored(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(db.executed, []string{
		"UPDATE gsbench.targets SET value='restored'",
	}) {
		t.Fatalf("executed=%v", db.executed)
	}
}

type concurrentJournalDatabase struct {
	allocation sync.Mutex
	eventsMu   sync.Mutex
	events     []string
	committed  int64
}

type concurrentJournalTransaction struct {
	db       *concurrentJournalDatabase
	locked   bool
	actionID int64
}

func (d *concurrentJournalDatabase) record(event string) {
	d.eventsMu.Lock()
	defer d.eventsMu.Unlock()
	d.events = append(d.events, event)
}

func (d *concurrentJournalDatabase) BeginJournalTransaction(
	context.Context,
) (journalSQLTransaction, error) {
	return &concurrentJournalTransaction{db: d}, nil
}

func (*concurrentJournalDatabase) Scan(
	context.Context,
	string,
	[]any,
	...any,
) error {
	return errors.New("scan must execute through journal transaction")
}

func (*concurrentJournalDatabase) Exec(
	context.Context,
	string,
	...any,
) (sql.Result, error) {
	return nil, errors.New("exec must execute through journal transaction")
}

func (*concurrentJournalDatabase) Query(
	context.Context,
	string,
	...any,
) (journalRows, error) {
	return nil, errors.New("unexpected query")
}

func (t *concurrentJournalTransaction) ExecContext(
	_ context.Context,
	query string,
	_ ...any,
) (sql.Result, error) {
	if query != journalLockStatement() {
		return nil, fmt.Errorf("first transaction statement=%q", query)
	}
	t.db.allocation.Lock()
	t.locked = true
	t.db.record("lock")
	return nil, nil
}

func (t *concurrentJournalTransaction) ScanContext(
	_ context.Context,
	query string,
	_ []any,
	dest ...any,
) error {
	if !t.locked {
		return errors.New("allocation scanned before advisory lock")
	}
	if strings.Contains(query, "pg_advisory_xact_lock") {
		return errors.New("insert reused lock statement snapshot")
	}
	t.actionID = t.db.committed + 1
	*(dest[0].(*int64)) = t.actionID
	t.db.record(fmt.Sprintf("scan:%d", t.actionID))
	return nil
}

func (t *concurrentJournalTransaction) Commit() error {
	t.db.committed = t.actionID
	t.db.record(fmt.Sprintf("commit:%d", t.actionID))
	t.locked = false
	t.db.allocation.Unlock()
	return nil
}

func (t *concurrentJournalTransaction) Rollback() error {
	if t.locked {
		t.locked = false
		t.db.allocation.Unlock()
	}
	return nil
}

func TestConcurrentSameRunJournalAllocationLocksBeforeFreshInsertSnapshot(t *testing.T) {
	db := &concurrentJournalDatabase{}
	store := newSQLJournalStore(db, "gsbench")
	start := make(chan struct{})
	results := make(chan int64, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			entry, err := store.InsertPlanned(context.Background(), Action{
				RunID:         "same-run",
				ScenarioCode:  601,
				Kind:          ActionSQLMutation,
				TargetProduct: ProductGaussDB,
				Target:        "gsbench.plan_data",
				Forward:       []byte(`{"sql":"forward"}`),
				Inverse:       []byte(`{"sql":"inverse"}`),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- entry.Sequence
		}()
	}
	close(start)
	var ids []int64
	for len(ids) < 2 {
		select {
		case err := <-errs:
			t.Fatal(err)
		case id := <-results:
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if !reflect.DeepEqual(ids, []int64{1, 2}) {
		t.Fatalf("action IDs=%v", ids)
	}
	if !reflect.DeepEqual(db.events, []string{
		"lock", "scan:1", "commit:1",
		"lock", "scan:2", "commit:2",
	}) {
		t.Fatalf("transaction interleaving=%v", db.events)
	}
}

func TestAtomicDatasetBatchRollsBackWhenHighWaterUpsertFails(t *testing.T) {
	tx := &fakeDatasetTransaction{failCall: 2}
	err := executeAtomicDatasetBatch(
		context.Background(),
		fakeDatasetTransactionBeginner{tx: tx},
		"gsbench",
		TableBatch{
			Table:             "customers",
			Rows:              10_000,
			EstimatedRowBytes: 320,
			InsertSQL:         `INSERT INTO "gsbench".customers SELECT g FROM generate_series($1,$2) g`,
		},
		1,
		100,
		datasetVersion,
	)
	if err == nil {
		t.Fatal("expected metadata upsert failure")
	}
	if !tx.rolledBack || tx.committed {
		t.Fatalf("transaction committed=%v rolled_back=%v", tx.committed, tx.rolledBack)
	}
	if len(tx.queries) != 2 || !strings.Contains(tx.queries[1], "MERGE INTO") ||
		!strings.Contains(tx.queries[1], `"gsbench".meta_batches`) {
		t.Fatalf("transaction queries=%v", tx.queries)
	}
}

func TestAtomicDatasetBatchCommitsInsertAndHighWaterTogether(t *testing.T) {
	tx := &fakeDatasetTransaction{}
	err := executeAtomicDatasetBatch(
		context.Background(),
		fakeDatasetTransactionBeginner{tx: tx},
		"gsbench",
		TableBatch{
			Table:             "customers",
			Rows:              10_000,
			EstimatedRowBytes: 320,
			InsertSQL:         `INSERT INTO "gsbench".customers SELECT g FROM generate_series($1,$2) g`,
		},
		1,
		100,
		datasetVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("transaction committed=%v rolled_back=%v", tx.committed, tx.rolledBack)
	}
	if !reflect.DeepEqual(tx.args[0], []any{int64(1), int64(100)}) {
		t.Fatalf("insert args=%v", tx.args[0])
	}
}

func TestLegacyJournalMigrationConvergesFormalCompositeKeyContract(t *testing.T) {
	statements, err := legacyJournalMigrationStatements(
		"gsbench",
		map[string]bool{
			"id": true, "scenario": true, "kind": true, "target": true,
			"original_value": true, "forward_sql": true, "inverse_sql": true,
			"verify_sql": true, "error_text": true,
		},
		"meta_journal_pkey",
		"PRIMARY KEY (id)",
		"HASH (run_id)",
		true,
		ProductGaussDB,
	)
	if err != nil {
		t.Fatal(err)
	}
	sqlText := strings.Join(statements, "\n")
	for _, token := range []string{
		`ALTER TABLE "gsbench".meta_journal ADD COLUMN action_id bigint`,
		"action_id=id",
		"scenario_code=CASE scenario",
		"action_kind='SQL_MUTATION'",
		"WHEN 'plan_stats_target' THEN 601",
		"WHEN 'thread_pool' THEN 402",
		"WHEN 'vacuum_pressure' THEN 801",
		"target_product='GaussDB'",
		"target_endpoint=target",
		"original_state=original_value",
		"forward_action=forward_sql",
		"inverse_action=inverse_sql",
		"verify_action=verify_sql",
		"last_error=error_text",
		`DROP CONSTRAINT "meta_journal_pkey"`,
		"ADD PRIMARY KEY (run_id,action_id)",
		"DISTRIBUTE BY HASH (run_id)",
	} {
		if !strings.Contains(sqlText, token) {
			t.Errorf("journal migration missing %q:\n%s", token, sqlText)
		}
	}
	for _, definition := range DefaultScenarioCatalog().Definitions() {
		token := fmt.Sprintf(
			"WHEN '%s' THEN %d",
			definition.Name,
			definition.Code,
		)
		if !strings.Contains(sqlText, token) {
			t.Errorf("journal migration missing catalog scenario %q", token)
		}
	}
	if strings.Contains(sqlText, "ELSE 0") {
		t.Fatalf("journal migration silently maps unknown scenarios to zero:\n%s", sqlText)
	}
}

func TestFormalJournalMigrationDoesNotDuplicateColumnsOrKeys(t *testing.T) {
	columns := map[string]bool{
		"run_id": true, "action_id": true, "scenario_code": true,
		"action_kind": true, "target_product": true, "target_node": true,
		"target_endpoint": true, "original_state": true,
		"forward_action": true, "inverse_action": true, "verify_action": true,
		"verify_value": true, "state": true, "last_error": true,
		"created_at": true, "updated_at": true,
	}
	statements, err := legacyJournalMigrationStatements(
		"gsbench",
		columns,
		"meta_journal_pkey",
		"PRIMARY KEY (run_id, action_id)",
		"HASH (run_id)",
		false,
		ProductGaussDB,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		upper := strings.ToUpper(statement)
		if strings.Contains(upper, "ADD COLUMN") ||
			strings.Contains(upper, "DROP CONSTRAINT") ||
			strings.Contains(upper, "ADD PRIMARY KEY") ||
			strings.Contains(upper, "DISTRIBUTE BY") {
			t.Fatalf("formal migration is not idempotent: %s", statement)
		}
	}
}

func TestFormalJournalMigrationFailsClosedWhenLegacyScenarioNameWasLost(t *testing.T) {
	columns := map[string]bool{
		"run_id": true, "action_id": true, "scenario_code": true,
		"action_kind": true, "target_product": true, "target_node": true,
		"target_endpoint": true, "original_state": true,
		"forward_action": true, "inverse_action": true, "verify_action": true,
		"verify_value": true, "state": true, "last_error": true,
		"created_at": true, "updated_at": true,
	}
	statements, err := legacyJournalMigrationStatements(
		"gsbench",
		columns,
		"meta_journal_pkey",
		"PRIMARY KEY (run_id, action_id)",
		"HASH (run_id)",
		false,
		ProductGaussDB,
	)
	if err != nil {
		t.Fatal(err)
	}
	sqlText := strings.Join(statements, "\n")
	if !strings.Contains(
		sqlText,
		"SET scenario_code=NULL WHERE scenario_code=0",
	) {
		t.Fatalf("migration can silently retain scenario_code=0:\n%s", sqlText)
	}
}

func TestLegacyPlanCacheMigrationConvergesScenarioCodeContract(t *testing.T) {
	statements, err := legacyPlanCacheMigrationStatements(
		"gsbench",
		map[string]bool{
			"signature": true, "scenario": true, "sql_text": true,
			"plan_text": true, "updated_at": true,
		},
		"meta_plan_cache_pkey",
		"PRIMARY KEY (signature, scenario)",
		"HASH (signature)",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	sqlText := strings.Join(statements, "\n")
	for _, token := range []string{
		`ALTER TABLE "gsbench".meta_plan_cache ADD COLUMN scenario_code integer`,
		"scenario_code=CASE scenario",
		"WHEN 'memory_workmem_sort' THEN 201",
		"WHEN 'plan_stats_target' THEN 601",
		"ELSE NULL END",
		"ALTER COLUMN scenario_code SET NOT NULL",
		`DROP CONSTRAINT "meta_plan_cache_pkey"`,
		"ADD PRIMARY KEY (signature,scenario_code)",
		"DROP COLUMN scenario",
		"DISTRIBUTE BY HASH (signature)",
	} {
		if !strings.Contains(sqlText, token) {
			t.Errorf("plan cache migration missing %q:\n%s", token, sqlText)
		}
	}
	if strings.Contains(sqlText, "ELSE 0") {
		t.Fatalf("plan cache migration silently maps scenarios to zero:\n%s", sqlText)
	}
	update := strings.Index(sqlText, "scenario_code=CASE scenario")
	notNull := strings.Index(sqlText, "ALTER COLUMN scenario_code SET NOT NULL")
	dropScenario := strings.Index(sqlText, "DROP COLUMN scenario")
	if update < 0 || notNull < update || dropScenario < notNull {
		t.Fatalf("plan cache migration order is unsafe:\n%s", sqlText)
	}
}

func TestFormalPlanCacheMigrationIsIdempotent(t *testing.T) {
	statements, err := legacyPlanCacheMigrationStatements(
		"gsbench",
		map[string]bool{
			"signature": true, "scenario_code": true, "sql_text": true,
			"plan_text": true, "updated_at": true,
		},
		"meta_plan_cache_pkey",
		"PRIMARY KEY (signature, scenario_code)",
		"HASH (signature)",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	sqlText := strings.Join(statements, "\n")
	for _, forbidden := range []string{
		"ADD COLUMN", "DROP CONSTRAINT", "ADD PRIMARY KEY",
		"DROP COLUMN", "DISTRIBUTE BY",
	} {
		if strings.Contains(sqlText, forbidden) {
			t.Fatalf(
				"formal plan cache migration contains %q:\n%s",
				forbidden,
				sqlText,
			)
		}
	}
}

func TestFormalPlanCacheMigrationClearsZeroScenarioCodeBeforeNotNull(t *testing.T) {
	statements, err := legacyPlanCacheMigrationStatements(
		"gsbench",
		map[string]bool{
			"signature": true, "scenario_code": true, "sql_text": true,
			"plan_text": true, "updated_at": true,
		},
		"meta_plan_cache_pkey",
		"PRIMARY KEY (signature, scenario_code)",
		"HASH (signature)",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	sqlText := strings.Join(statements, "\n")
	zero := strings.Index(
		sqlText,
		`UPDATE "gsbench".meta_plan_cache SET scenario_code=NULL WHERE scenario_code=0`,
	)
	notNull := strings.Index(sqlText, "ALTER COLUMN scenario_code SET NOT NULL")
	if zero < 0 || notNull < zero {
		t.Fatalf("formal plan-cache migration does not clear zero before NOT NULL:\n%s", sqlText)
	}
}

type fakePlanCacheScreenDatabase struct {
	invalidRows   int64
	collisionRows int64
	queries       []string
	execCalls     int
}

func (d *fakePlanCacheScreenDatabase) Scan(
	_ context.Context,
	query string,
	_ []any,
	dest ...any,
) error {
	d.queries = append(d.queries, query)
	value := d.invalidRows
	if strings.Contains(query, "HAVING count(*)>1") {
		value = d.collisionRows
	}
	*(dest[0].(*int64)) = value
	return nil
}

func (d *fakePlanCacheScreenDatabase) Exec(
	context.Context,
	string,
	...any,
) (sql.Result, error) {
	d.execCalls++
	return nil, errors.New("destructive DDL reached plan-cache preflight")
}

func (*fakePlanCacheScreenDatabase) Query(
	context.Context,
	string,
	...any,
) (journalRows, error) {
	return nil, errors.New("unexpected plan-cache query")
}

func TestLegacyPlanCacheScreenRejectsUnknownScenarioBeforeDDL(t *testing.T) {
	db := &fakePlanCacheScreenDatabase{invalidRows: 1}
	err := screenLegacyPlanCacheRows(
		context.Background(),
		db,
		"gsbench",
		map[string]bool{"scenario": true},
	)
	if err == nil || !strings.Contains(err.Error(), "unknown scenario") {
		t.Fatalf("screenLegacyPlanCacheRows() error=%v", err)
	}
	if db.execCalls != 0 || len(db.queries) != 1 {
		t.Fatalf("screen queries=%v exec_calls=%d", db.queries, db.execCalls)
	}
	if !strings.Contains(db.queries[0], "ELSE NULL END") ||
		strings.Contains(db.queries[0], "ELSE 0") {
		t.Fatalf("unknown-scenario screen is not fail-closed: %s", db.queries[0])
	}
}

func TestLegacyPlanCacheScreenRejectsAliasCollisionBeforeDDL(t *testing.T) {
	db := &fakePlanCacheScreenDatabase{collisionRows: 1}
	err := screenLegacyPlanCacheRows(
		context.Background(),
		db,
		"gsbench",
		map[string]bool{"scenario": true},
	)
	if err == nil || !strings.Contains(err.Error(), "scenario-code collision") {
		t.Fatalf("screenLegacyPlanCacheRows() error=%v", err)
	}
	if db.execCalls != 0 || len(db.queries) != 2 ||
		!strings.Contains(db.queries[1], "HAVING count(*)>1") {
		t.Fatalf("screen queries=%v exec_calls=%d", db.queries, db.execCalls)
	}
}

func TestFormalPlanCacheScreenRejectsUnmappableCodeBeforeDDL(t *testing.T) {
	db := &fakePlanCacheScreenDatabase{invalidRows: 1}
	err := screenLegacyPlanCacheRows(
		context.Background(),
		db,
		"gsbench",
		map[string]bool{"scenario_code": true},
	)
	if err == nil || !strings.Contains(err.Error(), "invalid scenario code") {
		t.Fatalf("screenLegacyPlanCacheRows() error=%v", err)
	}
	if db.execCalls != 0 || len(db.queries) != 1 ||
		!strings.Contains(db.queries[0], "scenario_code NOT IN") {
		t.Fatalf("screen queries=%v exec_calls=%d", db.queries, db.execCalls)
	}
}

func TestPlanCacheMigrationRejectsRowsWithoutScenarioIdentity(t *testing.T) {
	db := &fakePlanCacheScreenDatabase{}
	err := screenLegacyPlanCacheRows(
		context.Background(),
		db,
		"gsbench",
		map[string]bool{"signature": true},
	)
	if err == nil || !strings.Contains(err.Error(), "scenario identity") {
		t.Fatalf("screenLegacyPlanCacheRows() error=%v", err)
	}
	if db.execCalls != 0 || len(db.queries) != 0 {
		t.Fatalf("screen queries=%v exec_calls=%d", db.queries, db.execCalls)
	}
}

func TestMigratedLegacyJournalRowsLoadAndRestoreThroughTypedEngine(t *testing.T) {
	db := &fakeJournalDatabase{queryRows: [][]any{
		{
			int64(1), "run-legacy", int64(601), "SQL_MUTATION", "GaussDB",
			"", "gsbench.plan_data", "", "ANALYZE gsbench.plan_data",
			"ANALYZE gsbench.plan_data", "", "", "applied", "",
		},
		{
			int64(2), "run-legacy", int64(402), "SQL_MUTATION", "GaussDB",
			"", "enable_thread_pool", "off",
			"ALTER SYSTEM SET enable_thread_pool TO on",
			"ALTER SYSTEM SET enable_thread_pool TO off", "", "", "applied", "",
		},
		{
			int64(3), "run-legacy", int64(801), "SQL_MUTATION", "GaussDB",
			"", "gsbench.vacuum_targets", "",
			"UPDATE gsbench.vacuum_targets SET version=1",
			"UPDATE gsbench.vacuum_targets SET version=0", "", "", "applied", "",
		},
	}}
	store := newSQLJournalStore(db, "gsbench")
	pending, err := store.Pending(context.Background(), "run-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %+v", pending)
	}
	for _, action := range pending {
		if action.Kind != ActionSQLMutation || !action.LegacySQL {
			t.Fatalf("loaded legacy action = %+v", action)
		}
	}
	db.execQueries = nil
	journal := NewJournal(store, dbActionExecutor{db: db}, ProductGaussDB)
	sortActionsReverse(pending)
	if err := journal.restoreCoordinatorActions(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	var restoredSQL []string
	for _, query := range db.execQueries {
		if !strings.HasPrefix(query, "UPDATE \"gsbench\".meta_journal") {
			restoredSQL = append(restoredSQL, query)
		}
	}
	if !reflect.DeepEqual(restoredSQL, []string{
		"UPDATE gsbench.vacuum_targets SET version=0",
		"ALTER SYSTEM SET enable_thread_pool TO off",
		"ANALYZE gsbench.plan_data",
	}) {
		t.Fatalf("restored SQL = %v", restoredSQL)
	}
}

func TestPendingRejectsUnscreenedLegacySecretWithoutEchoingValue(t *testing.T) {
	const secret = "pending-secret"
	db := &fakeJournalDatabase{queryRows: [][]any{{
		int64(1), "run-legacy", int64(402), "SQL_MUTATION", "unknown",
		"", "enable_thread_pool", "",
		"ALTER SYSTEM SET enable_thread_pool TO on",
		"ALTER ROLE bench PASSWORD '" + secret + "'", "", "", "applied", "",
	}}}
	store := newSQLJournalStore(db, "gsbench")
	_, err := store.Pending(context.Background(), "run-legacy")
	if err == nil || !strings.Contains(err.Error(), "credential material") {
		t.Fatalf("Pending() error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("pending validation leaked secret: %v", err)
	}
}

func TestLegacyJournalSecretScreenFailsWithoutEchoingValue(t *testing.T) {
	const secret = "migration-secret"
	db := &fakeJournalDatabase{queryRows: [][]any{{
		"", "ALTER ROLE bench PASSWORD '" + secret + "'", "SELECT 1", "",
		"", "",
	}}}
	err := screenLegacyJournalRows(
		context.Background(),
		db,
		"gsbench",
		map[string]bool{
			"original_value": true,
			"forward_sql":    true,
			"inverse_sql":    true,
			"verify_sql":     true,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "credential material") {
		t.Fatalf("screenLegacyJournalRows() error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("migration error leaked secret: %v", err)
	}
}

func TestLegacyJournalSecretScreenIncludesTargetAndLastError(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		target string
		detail string
	}{
		{
			name:   "target",
			secret: "target-password",
			target: "postgres://bench:target-password@db.example/bench",
		},
		{
			name:   "last error",
			secret: "error-token",
			detail: "Authorization: Bearer error-token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &fakeJournalDatabase{queryRows: [][]any{{
				"", "SELECT 1", "SELECT 1", "", tt.target, tt.detail,
			}}}
			err := screenLegacyJournalRows(
				context.Background(),
				db,
				"gsbench",
				map[string]bool{
					"original_value": true,
					"forward_sql":    true,
					"inverse_sql":    true,
					"verify_sql":     true,
					"target":         true,
					"error_text":     true,
				},
			)
			if err == nil || !strings.Contains(err.Error(), "credential material") {
				t.Fatalf("screenLegacyJournalRows() error = %v", err)
			}
			if strings.Contains(err.Error(), tt.secret) {
				t.Fatalf("migration error leaked secret: %v", err)
			}
		})
	}
}

func TestSQLStoreSanitizesDirectCallerLastErrorAndStateDetail(t *testing.T) {
	const secret = "store-secret"
	db := &fakeJournalDatabase{}
	store := newSQLJournalStore(db, "gsbench")
	_, err := store.InsertPlanned(context.Background(), Action{
		RunID: "run-1", ScenarioCode: 601, Kind: ActionSQLMutation,
		TargetProduct: ProductGaussDB, Target: "gsbench.plan_data",
		Forward:   []byte(`{"sql":"SELECT 1"}`),
		Inverse:   []byte(`{"sql":"SELECT 1"}`),
		LastError: "Bearer " + secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	if detail, _ := db.scanArgs[12].(string); !strings.Contains(detail, "redacted") ||
		strings.Contains(detail, secret) {
		t.Fatalf("inserted last error = %#v", db.scanArgs[12])
	}
	if err := store.SetState(
		context.Background(),
		"run-1",
		7,
		MutationRestoreFailed,
		"password="+secret,
	); err != nil {
		t.Fatal(err)
	}
	if detail, _ := db.execArgs[1].(string); !strings.Contains(detail, "redacted") ||
		strings.Contains(detail, secret) {
		t.Fatalf("state detail = %#v", db.execArgs[1])
	}
}

func TestLegacyPlanDataMigrationConvergesConstraintsAndLayout(t *testing.T) {
	statements, err := legacyPlanDataConvergenceStatements(
		"gsbench",
		"plan_data_pkey",
		"PRIMARY KEY (id)",
		"HASH (dist_key)",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	sqlText := strings.Join(statements, "\n")
	for _, column := range []string{
		"dist_key", "stats_target_key", "stats_ndistinct_key",
		"stats_corr_a", "stats_corr_b", "index_unusable_key",
		"index_drop_key", "index_shape_lead", "index_shape_tail",
	} {
		if !strings.Contains(sqlText, "ALTER COLUMN "+column+" SET NOT NULL") {
			t.Errorf("plan_data convergence missing NOT NULL for %s", column)
		}
	}
	for _, token := range []string{
		`DROP CONSTRAINT "plan_data_pkey"`,
		"ADD PRIMARY KEY (dist_key,id)",
		"DISTRIBUTE BY HASH (dist_key)",
	} {
		if !strings.Contains(sqlText, token) {
			t.Errorf("plan_data convergence missing %q:\n%s", token, sqlText)
		}
	}
}

func TestExpectedDatasetTableShapeStopsAtBalancedTableParenthesis(t *testing.T) {
	for _, suffix := range []string{
		"",
		" DISTRIBUTE BY HASH (dist_key)",
		" DISTRIBUTE BY REPLICATION",
	} {
		shape, err := expectedDatasetTableShape(
			`CREATE TABLE "gsbench".sample (` +
				`dist_key bigint NOT NULL,id bigint NOT NULL,` +
				`PRIMARY KEY (dist_key, id))` + suffix,
		)
		if err != nil {
			t.Fatal(err)
		}
		if shape.PrimaryKey != "PRIMARY KEY (dist_key, id)" {
			t.Fatalf("suffix=%q primary key=%q", suffix, shape.PrimaryKey)
		}
	}
}

type catalogValidationDatabase struct {
	rows         *sliceJournalRows
	primaryKey   string
	distribution string
}

func (d *catalogValidationDatabase) Query(
	context.Context,
	string,
	...any,
) (journalRows, error) {
	d.rows.index = 0
	return d.rows, nil
}

func (d *catalogValidationDatabase) Scan(
	_ context.Context,
	query string,
	_ []any,
	dest ...any,
) error {
	switch {
	case strings.Contains(query, "pg_get_constraintdef"):
		*(dest[0].(*string)) = d.primaryKey
	case strings.Contains(query, "getdistributekey"):
		*(dest[0].(*string)) = d.distribution
	default:
		return errors.New("unexpected catalog validation scan")
	}
	return nil
}

func (*catalogValidationDatabase) Exec(
	context.Context,
	string,
	...any,
) (sql.Result, error) {
	return nil, errors.New("unexpected catalog validation exec")
}

func TestRealDatasetShapeValidatorAcceptsCentralHashAndReplicationContracts(t *testing.T) {
	tests := []struct {
		name         string
		ddl          string
		distribution string
	}{
		{
			name: "central",
			ddl: `CREATE TABLE "Bench".sample (
				id bigint NOT NULL,
				PRIMARY KEY (id)
			)`,
		},
		{
			name: "hash",
			ddl: `CREATE TABLE "Bench".sample (
				dist_key bigint NOT NULL,
				id bigint NOT NULL,
				PRIMARY KEY (dist_key, id)
			) DISTRIBUTE BY HASH (dist_key)`,
			distribution: "dist_key",
		},
		{
			name: "replication",
			ddl: `CREATE TABLE "Bench".sample (
				id integer NOT NULL,
				PRIMARY KEY (id)
			) DISTRIBUTE BY REPLICATION`,
			distribution: "REPLICATION",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object, err := parseDatasetObject(tt.ddl)
			if err != nil {
				t.Fatal(err)
			}
			shape, err := expectedDatasetTableShape(tt.ddl)
			if err != nil {
				t.Fatal(err)
			}
			rows := make([][]any, len(shape.Columns))
			for i, column := range shape.Columns {
				rows[i] = []any{column.Name, column.Type, column.NotNull}
			}
			db := &catalogValidationDatabase{
				rows:         &sliceJournalRows{rows: rows},
				primaryKey:   shape.PrimaryKey,
				distribution: tt.distribution,
			}
			if err := validateDatasetObjectContract(
				context.Background(), db, "Bench", object,
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRealDatasetShapeValidatorAcceptsPostMigrationPhysicalColumnOrder(t *testing.T) {
	var ddl string
	for _, statement := range DatasetDialectFor(Environment{}).TableDDL("Bench") {
		if strings.HasPrefix(statement, `CREATE TABLE "Bench".meta_journal `) {
			ddl = statement
			break
		}
	}
	if ddl == "" {
		t.Fatal("formal meta_journal DDL is missing")
	}
	object, err := parseDatasetObject(ddl)
	if err != nil {
		t.Fatal(err)
	}
	shape, err := expectedDatasetTableShape(ddl)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]datasetColumnShape{}
	for _, column := range shape.Columns {
		byName[column.Name] = column
	}
	physicalOrder := []string{
		"run_id", "state", "created_at", "updated_at", "action_id",
		"scenario_code", "action_kind", "target_product", "target_node",
		"target_endpoint", "original_state", "forward_action",
		"inverse_action", "verify_action", "verify_value", "last_error",
	}
	rows := make([][]any, 0, len(physicalOrder))
	for _, name := range physicalOrder {
		column := byName[name]
		rows = append(rows, []any{column.Name, column.Type, column.NotNull})
	}
	db := &catalogValidationDatabase{
		rows:       &sliceJournalRows{rows: rows},
		primaryKey: shape.PrimaryKey,
	}
	if err := validateDatasetObjectContract(
		context.Background(), db, "Bench", object,
	); err != nil {
		t.Fatal(err)
	}
}

func TestDatasetColumnShapeComparisonStillRejectsWrongNamedContract(t *testing.T) {
	expected := []datasetColumnShape{
		{Name: "run_id", Type: "varchar(96)", NotNull: true},
		{Name: "action_id", Type: "bigint", NotNull: true},
	}
	actual := []datasetColumnShape{
		{Name: "action_id", Type: "integer", NotNull: true},
		{Name: "run_id", Type: "varchar(96)", NotNull: true},
	}
	if equalDatasetColumns(actual, expected) {
		t.Fatal("wrong type passed exact named-column comparison")
	}
}

func TestDatasetColumnShapeComparisonAcceptsGaussDBORADateAlias(t *testing.T) {
	expected := []datasetColumnShape{{
		Name: "sale_date", Type: "date", NotNull: true,
	}}
	actual := []datasetColumnShape{{
		Name: "sale_date", Type: "timestamp(0)withouttimezone", NotNull: true,
	}}
	if !equalDatasetColumns(actual, expected) {
		t.Fatal("GaussDB ORA DATE catalog alias was rejected")
	}
}

func TestDatasetColumnShapeComparisonRejectsOtherTimestampForDate(t *testing.T) {
	expected := []datasetColumnShape{{
		Name: "sale_date", Type: "date", NotNull: true,
	}}
	actual := []datasetColumnShape{{
		Name: "sale_date", Type: "timestamp", NotNull: true,
	}}
	if equalDatasetColumns(actual, expected) {
		t.Fatal("non-ORA timestamp unexpectedly matched date")
	}
}

func TestDatasetDistributionMatchesExactCanonicalStrategyAndKeys(t *testing.T) {
	tests := []struct {
		name     string
		actual   string
		expected string
		want     bool
	}{
		{
			name:   "catalog bare hash key",
			actual: `"dist_key"`, expected: "HASH (dist_key)", want: true,
		},
		{
			name:     "full hash clause",
			actual:   `DISTRIBUTE BY HASH ("dist_key")`,
			expected: "HASH (dist_key)", want: true,
		},
		{
			name:     "exact multi key",
			actual:   `HASH ("dist_key", "tenant_id")`,
			expected: "HASH (dist_key, tenant_id)", want: true,
		},
		{
			name:     "replication clause",
			actual:   "DISTRIBUTE BY REPLICATION",
			expected: "REPLICATION", want: true,
		},
		{
			name:     "longer key",
			actual:   "dist_key_extra",
			expected: "HASH (dist_key)", want: false,
		},
		{
			name:     "unexpected second bare key",
			actual:   "dist_key, tenant_id",
			expected: "HASH (dist_key)", want: false,
		},
		{
			name:     "unexpected second hash key",
			actual:   "HASH (dist_key, tenant_id)",
			expected: "HASH (dist_key)", want: false,
		},
		{
			name:     "strategy mismatch",
			actual:   "REPLICATION",
			expected: "HASH (dist_key)", want: false,
		},
		{
			name:     "unrecognized strategy",
			actual:   "DISTRIBUTE BY RANDOM",
			expected: "HASH (random)", want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := datasetDistributionMatches(tt.actual, tt.expected); got != tt.want {
				t.Fatalf(
					"datasetDistributionMatches(%q, %q)=%v want=%v",
					tt.actual, tt.expected, got, tt.want,
				)
			}
		})
	}
}

var (
	_ DatasetExecutor             = dbDatasetExecutor{}
	_ DatasetObjectCatalog        = dbDatasetExecutor{}
	_ DatasetVersionCatalog       = dbDatasetExecutor{}
	_ DatasetAtomicBatchExecutor  = dbDatasetExecutor{}
	_ DatasetPostMigrationCatalog = dbDatasetExecutor{}
)

func TestLegacySQLStatementsSplitsMultiCommand(t *testing.T) {
	got, err := legacySQLStatements("ALTER TABLE gsbench.plan_data ALTER COLUMN x SET STATISTICS -1; ANALYZE gsbench.plan_data(x)")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ALTER TABLE gsbench.plan_data ALTER COLUMN x SET STATISTICS -1",
		"ANALYZE gsbench.plan_data(x)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacySQLStatements()=%q want=%q", got, want)
	}
}

func TestLegacySQLStatementsHandlesQuotesDollarQuotesAndComments(t *testing.T) {
	query := `DO $body$
BEGIN
	RAISE NOTICE 'a;b';
	PERFORM 1;
END
$body$;
SELECT "semi;colon" /* outer ; /* nested ; */ still comment */
FROM metrics -- ignored ;
WHERE value='x;y'`
	got, err := legacySQLStatements(query)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`DO $body$
BEGIN
	RAISE NOTICE 'a;b';
	PERFORM 1;
END
$body$`,
		`SELECT "semi;colon" /* outer ; /* nested ; */ still comment */
FROM metrics -- ignored ;
WHERE value='x;y'`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacySQLStatements()=%q want=%q", got, want)
	}
}

func TestLegacySQLExecutorUsesExplicitProvenanceForMultipleStatements(t *testing.T) {
	db := &fakeSQLActionDatabase{}
	executor := dbActionExecutor{db: db}
	action := Action{
		Kind:      ActionSQLMutation,
		LegacySQL: true,
		Forward:   []byte(`{"sql":"SELECT 1; SELECT 2"}`),
		Inverse:   []byte(`{"sql":"SELECT 3"}`),
	}
	if err := executor.Preflight(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := executor.Apply(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(db.executed, []string{"SELECT 1", "SELECT 2"}) {
		t.Fatalf("executed = %v", db.executed)
	}
}
