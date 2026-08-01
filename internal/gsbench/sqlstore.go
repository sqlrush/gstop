package gsbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type dbDatasetExecutor struct {
	db                 *Database
	schema             string
	env                Environment
	capacityProvider   DatasetCapacityProvider
	physicalProvider   DatasetPhysicalProvider
	minFreeDiskPercent int
}

type datasetSQLTransaction interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Commit() error
	Rollback() error
}

type datasetTransactionBeginner interface {
	BeginDatasetTransaction(ctx context.Context) (datasetSQLTransaction, error)
}

type databaseDatasetTransactionBeginner struct {
	db *Database
}

type databaseDatasetTransaction struct {
	*sql.Tx
	cancel context.CancelFunc
}

type journalSQLTransaction interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	ScanContext(ctx context.Context, query string, args []any, dest ...any) error
	Commit() error
	Rollback() error
}

type journalTransactionBeginner interface {
	BeginJournalTransaction(ctx context.Context) (journalSQLTransaction, error)
}

func (b databaseDatasetTransactionBeginner) BeginDatasetTransaction(
	parent context.Context,
) (datasetSQLTransaction, error) {
	ctx, cancel := b.db.operationContext(parent)
	tx, err := b.db.pool.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	return &databaseDatasetTransaction{Tx: tx, cancel: cancel}, nil
}

func (t *databaseDatasetTransaction) Commit() error {
	defer t.cancel()
	return t.Tx.Commit()
}

func (t *databaseDatasetTransaction) Rollback() error {
	defer t.cancel()
	return t.Tx.Rollback()
}

func (t *databaseDatasetTransaction) ScanContext(
	ctx context.Context,
	query string,
	args []any,
	dest ...any,
) error {
	return t.Tx.QueryRowContext(ctx, query, args...).Scan(dest...)
}

func (e dbDatasetExecutor) Exec(ctx context.Context, query string, args ...any) error {
	_, err := e.db.Exec(ctx, query, args...)
	return err
}

func (e dbDatasetExecutor) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var count int
	err := e.db.Scan(
		ctx,
		"SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname=$1",
		[]any{schema},
		&count,
	)
	return count > 0, err
}

func (e dbDatasetExecutor) BatchHighWater(ctx context.Context, table string) (int64, error) {
	quotedSchema, ok := quoteDatasetSchema(e.schema)
	if !ok {
		return 0, fmt.Errorf("unsafe dataset schema %q", e.schema)
	}
	var high int64
	err := e.db.Scan(ctx, "SELECT high_water FROM "+quotedSchema+".meta_batches WHERE table_name=$1", []any{table}, &high)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return high, err
}

func (e dbDatasetExecutor) ColumnExists(ctx context.Context, schema, table, column string) (bool, error) {
	var count int
	err := e.db.Scan(ctx, `SELECT count(*) FROM pg_attribute
		WHERE attrelid=($1 || '.' || $2)::regclass
		  AND attname=$3 AND attnum>0 AND NOT attisdropped`,
		[]any{schema, table, column}, &count)
	return count == 1, err
}

func (e dbDatasetExecutor) DatasetObjectExists(
	ctx context.Context,
	object DatasetObject,
) (bool, error) {
	relkind := "r"
	if object.Kind == DatasetObjectIndex {
		relkind = "i"
	}
	var count int
	err := e.db.Scan(ctx, `SELECT count(*)
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind=$3`,
		[]any{e.schema, object.Name, relkind},
		&count,
	)
	return count == 1, err
}

func (e dbDatasetExecutor) MigrateLegacyDatasetObject(
	ctx context.Context,
	object DatasetObject,
) error {
	columns, err := e.datasetColumnNames(ctx, object.Name)
	if err != nil {
		return err
	}
	var statements []string
	switch object.Name {
	case "meta_journal":
		if err := screenLegacyJournalRows(
			ctx,
			databaseJournalDB{db: e.db},
			e.schema,
			columns,
		); err != nil {
			return err
		}
		primaryKey, primaryKeyDefinition, keyErr := e.datasetPrimaryKey(
			ctx, object.Name,
		)
		if keyErr != nil && keyErr != sql.ErrNoRows {
			return keyErr
		}
		distribution, distributionMismatch, distributionErr :=
			e.datasetDistributionMigration(ctx, object)
		if distributionErr != nil {
			return distributionErr
		}
		statements, err = legacyJournalMigrationStatements(
			e.schema,
			columns,
			primaryKey,
			primaryKeyDefinition,
			distribution,
			distributionMismatch,
			e.env.Product,
		)
	case "meta_batches":
		statements, err = legacyBatchMigrationStatements(e.schema, columns)
	case "meta_plan_cache":
		if err := screenLegacyPlanCacheRows(
			ctx,
			databaseJournalDB{db: e.db},
			e.schema,
			columns,
		); err != nil {
			return err
		}
		primaryKey, primaryKeyDefinition, keyErr := e.datasetPrimaryKey(
			ctx, object.Name,
		)
		if keyErr != nil && keyErr != sql.ErrNoRows {
			return keyErr
		}
		distribution, distributionMismatch, distributionErr :=
			e.datasetDistributionMigration(ctx, object)
		if distributionErr != nil {
			return distributionErr
		}
		statements, err = legacyPlanCacheMigrationStatements(
			e.schema,
			columns,
			primaryKey,
			primaryKeyDefinition,
			distribution,
			distributionMismatch,
		)
	}
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := e.db.Exec(ctx, statement); err != nil {
			return fmt.Errorf("execute legacy %s migration: %w", object.Name, err)
		}
	}
	if object.Distribution != "" &&
		object.Name != "meta_journal" &&
		object.Name != "meta_plan_cache" &&
		object.Name != "plan_data" {
		distribution, mismatch, distributionErr :=
			e.datasetDistributionMigration(ctx, object)
		if distributionErr != nil {
			return distributionErr
		}
		if !mismatch {
			return nil
		}
		quotedSchema, ok := quoteDatasetSchema(e.schema)
		if !ok {
			return fmt.Errorf("unsafe dataset schema %q", e.schema)
		}
		statement := "ALTER TABLE " + quotedSchema + "." + object.Name +
			" DISTRIBUTE BY " + distribution
		if _, err := e.db.Exec(ctx, statement); err != nil {
			return fmt.Errorf("converge %s distribution: %w", object.Name, err)
		}
	}
	return nil
}

func (e dbDatasetExecutor) ConvergeDatasetObject(
	ctx context.Context,
	object DatasetObject,
) error {
	if object.Name != "plan_data" {
		return nil
	}
	primaryKey, primaryKeyDefinition, err := e.datasetPrimaryKey(ctx, object.Name)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	distribution, distributionMismatch, err :=
		e.datasetDistributionMigration(ctx, object)
	if err != nil {
		return err
	}
	statements, err := legacyPlanDataConvergenceStatements(
		e.schema,
		primaryKey,
		primaryKeyDefinition,
		distribution,
		distributionMismatch,
	)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := e.db.Exec(ctx, statement); err != nil {
			return fmt.Errorf("execute plan_data convergence: %w", err)
		}
	}
	return nil
}

func (e dbDatasetExecutor) ValidateDatasetObject(
	ctx context.Context,
	object DatasetObject,
) error {
	return validateDatasetObjectContract(
		ctx, databaseJournalDB{db: e.db}, e.schema, object,
	)
}

func validateDatasetObjectContract(
	ctx context.Context,
	db journalDatabase,
	schema string,
	object DatasetObject,
) error {
	switch object.Kind {
	case DatasetObjectTable:
		expected, err := expectedDatasetTableShape(object.DDL)
		if err != nil {
			return err
		}
		rows, err := db.Query(ctx, `SELECT a.attname,
				pg_catalog.format_type(a.atttypid,a.atttypmod),
				a.attnotnull
			FROM pg_catalog.pg_attribute a
			JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
			JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=$1 AND c.relname=$2
			  AND a.attnum>0 AND NOT a.attisdropped
			ORDER BY a.attnum`, schema, object.Name)
		if err != nil {
			return err
		}
		defer rows.Close()
		var actual []datasetColumnShape
		for rows.Next() {
			var column datasetColumnShape
			if err := rows.Scan(&column.Name, &column.Type, &column.NotNull); err != nil {
				return err
			}
			column.Type = normalizeDatasetType(column.Type)
			actual = append(actual, column)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !equalDatasetColumns(actual, expected.Columns) {
			return fmt.Errorf("table %s columns differ: actual=%v expected=%v",
				object.Name, actual, expected.Columns)
		}
		var primaryKey string
		err = db.Scan(ctx, `SELECT pg_catalog.pg_get_constraintdef(con.oid)
			FROM pg_catalog.pg_constraint con
			JOIN pg_catalog.pg_class c ON c.oid=con.conrelid
			JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=$1 AND c.relname=$2 AND con.contype='p'`,
			[]any{schema, object.Name},
			&primaryKey,
		)
		if err != nil {
			return err
		}
		if normalizeDatasetSQL(primaryKey) != normalizeDatasetSQL(expected.PrimaryKey) {
			return fmt.Errorf("table %s primary key %q, expected %q",
				object.Name, primaryKey, expected.PrimaryKey)
		}
		if object.Distribution != "" {
			var distribution string
			if err := db.Scan(
				ctx,
				"SELECT COALESCE(getdistributekey($1),'')",
				[]any{schema + "." + object.Name},
				&distribution,
			); err != nil {
				return err
			}
			if !datasetDistributionMatches(distribution, object.Distribution) {
				return fmt.Errorf("table %s distribution %q, expected %q",
					object.Name, distribution, object.Distribution)
			}
		}
		return nil
	case DatasetObjectIndex:
		var definition string
		if err := db.Scan(ctx, `SELECT pg_catalog.pg_get_indexdef(c.oid)
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind='i'`,
			[]any{schema, object.Name},
			&definition,
		); err != nil {
			return err
		}
		if !datasetIndexMatches(definition, object.DDL) {
			return fmt.Errorf("index %s definition %q, expected %q",
				object.Name, definition, object.DDL)
		}
		return nil
	default:
		return fmt.Errorf("unsupported dataset object kind %q", object.Kind)
	}
}

func (e dbDatasetExecutor) DatasetVersion(ctx context.Context, schema string) (string, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return "", fmt.Errorf("unsafe dataset schema %q", schema)
	}
	var version string
	err := e.db.Scan(
		ctx,
		"SELECT value FROM "+quotedSchema+".meta_dataset WHERE key=$1",
		[]any{"dataset_version"},
		&version,
	)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return version, err
}

func (e dbDatasetExecutor) RecordDatasetVersion(
	ctx context.Context,
	schema, version string,
) error {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe dataset schema %q", schema)
	}
	_, err := e.db.Exec(ctx, `MERGE INTO `+quotedSchema+`.meta_dataset AS target
		USING (SELECT $1::varchar(128) AS key,$2::text AS value) AS source
		ON target.key=source.key
		WHEN MATCHED THEN UPDATE SET
			value=source.value,updated_at=current_timestamp
		WHEN NOT MATCHED THEN INSERT(key,value)
			VALUES(source.key,source.value)`,
		"dataset_version",
		version,
	)
	return err
}

func (e dbDatasetExecutor) datasetColumnNames(
	ctx context.Context,
	table string,
) (map[string]bool, error) {
	rows, err := e.db.Query(ctx, `SELECT a.attname
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname=$2
		  AND a.attnum>0 AND NOT a.attisdropped
		ORDER BY a.attnum`, e.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func (e dbDatasetExecutor) datasetPrimaryKey(
	ctx context.Context,
	table string,
) (string, string, error) {
	var name, definition string
	err := e.db.Scan(ctx, `SELECT con.conname
			,pg_catalog.pg_get_constraintdef(con.oid)
		FROM pg_catalog.pg_constraint con
		JOIN pg_catalog.pg_class c ON c.oid=con.conrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname=$2 AND con.contype='p'`,
		[]any{e.schema, table},
		&name,
		&definition,
	)
	return name, definition, err
}

func (e dbDatasetExecutor) datasetDistributionMigration(
	ctx context.Context,
	object DatasetObject,
) (string, bool, error) {
	if object.Distribution == "" {
		return "", false, nil
	}
	var actual string
	if err := e.db.Scan(
		ctx,
		"SELECT COALESCE(getdistributekey($1),'')",
		[]any{e.schema + "." + object.Name},
		&actual,
	); err != nil {
		return "", false, err
	}
	return object.Distribution,
		!datasetDistributionMatches(actual, object.Distribution),
		nil
}

func (e dbDatasetExecutor) ApplyDatasetBatch(
	ctx context.Context,
	schema string,
	batch TableBatch,
	start, end int64,
	version string,
) error {
	return executeAtomicDatasetBatch(
		ctx,
		databaseDatasetTransactionBeginner{db: e.db},
		schema,
		batch,
		start,
		end,
		version,
	)
}

func executeAtomicDatasetBatch(
	ctx context.Context,
	beginner datasetTransactionBeginner,
	schema string,
	batch TableBatch,
	start, end int64,
	version string,
) error {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe dataset schema %q", schema)
	}
	tx, err := beginner.BeginDatasetTransaction(ctx)
	if err != nil {
		return fmt.Errorf("begin dataset batch transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, batch.InsertSQL, start, end); err != nil {
		return fmt.Errorf("insert %s batch: %w", batch.Table, err)
	}
	upsert := `MERGE INTO ` + quotedSchema + `.meta_batches AS target
		USING (
			SELECT $1::varchar(128) AS table_name,
				$2::bigint AS high_water,
				$3::bigint AS target_rows,
				$4::bigint AS estimated_row_bytes,
				$5::varchar(32) AS dataset_version
		) AS source
		ON target.table_name=source.table_name
		WHEN MATCHED THEN UPDATE SET
			high_water=source.high_water,
			target_rows=source.target_rows,
			estimated_row_bytes=source.estimated_row_bytes,
			dataset_version=source.dataset_version,
			updated_at=current_timestamp
		WHEN NOT MATCHED THEN INSERT(
			table_name,high_water,target_rows,estimated_row_bytes,dataset_version
		) VALUES(
			source.table_name,source.high_water,source.target_rows,
			source.estimated_row_bytes,source.dataset_version
		)`
	if _, err := tx.ExecContext(
		ctx,
		upsert,
		batch.Table,
		end,
		batch.Rows,
		batch.EstimatedRowBytes,
		version,
	); err != nil {
		return fmt.Errorf("upsert %s high-water mark: %w", batch.Table, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s dataset batch: %w", batch.Table, err)
	}
	committed = true
	return nil
}

type datasetColumnShape struct {
	Name    string
	Type    string
	NotNull bool
}

type datasetTableShape struct {
	Columns    []datasetColumnShape
	PrimaryKey string
}

var datasetWhitespaceRE = regexp.MustCompile(`\s+`)

func expectedDatasetTableShape(ddl string) (datasetTableShape, error) {
	open := strings.Index(ddl, "(")
	close := matchingDatasetParenthesis(ddl, open)
	if open < 0 || close <= open {
		return datasetTableShape{}, fmt.Errorf("invalid dataset table DDL %q", ddl)
	}
	var shape datasetTableShape
	for _, definition := range splitDatasetDefinitions(ddl[open+1 : close]) {
		definition = strings.TrimSpace(definition)
		upper := strings.ToUpper(definition)
		if strings.HasPrefix(upper, "PRIMARY KEY") {
			shape.PrimaryKey = definition
			continue
		}
		fields := strings.Fields(definition)
		if len(fields) < 2 {
			return datasetTableShape{}, fmt.Errorf("invalid dataset column %q", definition)
		}
		typeEnd := len(fields)
		for i := 2; i < len(fields); i++ {
			switch strings.ToUpper(fields[i]) {
			case "NOT", "NULL", "DEFAULT", "PRIMARY", "CONSTRAINT", "UNIQUE", "CHECK":
				typeEnd = i
				i = len(fields)
			}
		}
		column := datasetColumnShape{
			Name: strings.Trim(fields[0], `"`),
			Type: normalizeDatasetType(strings.Join(fields[1:typeEnd], " ")),
			NotNull: strings.Contains(upper, "NOT NULL") ||
				strings.Contains(upper, "PRIMARY KEY"),
		}
		shape.Columns = append(shape.Columns, column)
		if strings.Contains(upper, "PRIMARY KEY") {
			shape.PrimaryKey = "PRIMARY KEY (" + column.Name + ")"
		}
	}
	if shape.PrimaryKey == "" {
		return datasetTableShape{}, fmt.Errorf("dataset table DDL has no primary key")
	}
	return shape, nil
}

func matchingDatasetParenthesis(value string, open int) int {
	if open < 0 || open >= len(value) || value[open] != '(' {
		return -1
	}
	depth := 0
	for i := open; i < len(value); i++ {
		switch value[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func splitDatasetDefinitions(body string) []string {
	var definitions []string
	start := 0
	depth := 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				definitions = append(definitions, body[start:i])
				start = i + 1
			}
		}
	}
	definitions = append(definitions, body[start:])
	return definitions
}

func normalizeDatasetType(value string) string {
	value = strings.ToLower(datasetWhitespaceRE.ReplaceAllString(strings.TrimSpace(value), " "))
	replacements := []struct{ old, new string }{
		{"character varying", "varchar"},
		{"timestamp without time zone", "timestamp"},
		{"numeric", "numeric"},
		{"int8", "bigint"},
		{"int4", "integer"},
	}
	for _, replacement := range replacements {
		value = strings.ReplaceAll(value, replacement.old, replacement.new)
	}
	return strings.ReplaceAll(value, " ", "")
}

func normalizeDatasetSQL(value string) string {
	value = strings.ToLower(datasetWhitespaceRE.ReplaceAllString(strings.TrimSpace(value), " "))
	value = strings.ReplaceAll(value, `"`, "")
	value = strings.ReplaceAll(value, " ", "")
	return value
}

func equalDatasetColumns(actual, expected []datasetColumnShape) bool {
	if len(actual) != len(expected) {
		return false
	}
	expectedByName := make(map[string]datasetColumnShape, len(expected))
	for _, column := range expected {
		if _, exists := expectedByName[column.Name]; exists {
			return false
		}
		expectedByName[column.Name] = column
	}
	seen := make(map[string]bool, len(actual))
	for _, column := range actual {
		expectedColumn, exists := expectedByName[column.Name]
		if seen[column.Name] || !exists ||
			expectedColumn.NotNull != column.NotNull ||
			!datasetColumnTypesEquivalent(column.Type, expectedColumn.Type) {
			return false
		}
		seen[column.Name] = true
	}
	return true
}

func datasetColumnTypesEquivalent(actual, expected string) bool {
	return actual == expected ||
		(expected == "date" && actual == "timestamp(0)withouttimezone")
}

func datasetDistributionMatches(actual, expected string) bool {
	actualStrategy, actualKeys, actualOK := canonicalDatasetDistribution(actual)
	expectedStrategy, expectedKeys, expectedOK := canonicalDatasetDistribution(expected)
	if !actualOK || !expectedOK ||
		actualStrategy != expectedStrategy ||
		len(actualKeys) != len(expectedKeys) {
		return false
	}
	for i := range actualKeys {
		if actualKeys[i] != expectedKeys[i] {
			return false
		}
	}
	return true
}

func canonicalDatasetDistribution(
	value string,
) (strategy string, keys []string, ok bool) {
	value = normalizeDatasetSQL(value)
	hasDistributionClause := strings.HasPrefix(value, "distributeby")
	if hasDistributionClause {
		value = strings.TrimPrefix(value, "distributeby")
	}
	if value == "replication" {
		return "replication", nil, true
	}

	keyList := value
	if strings.HasPrefix(value, "hash(") && strings.HasSuffix(value, ")") {
		keyList = value[len("hash(") : len(value)-1]
	} else if hasDistributionClause || strings.ContainsAny(value, "()") {
		return "", nil, false
	}
	if keyList == "" {
		return "", nil, false
	}
	keys = strings.Split(keyList, ",")
	for _, key := range keys {
		if !identifierRE.MatchString(key) {
			return "", nil, false
		}
	}
	return "hash", keys, true
}

func datasetIndexMatches(actual, expected string) bool {
	indexParts := func(definition string) (table, columns string) {
		normalized := strings.ReplaceAll(definition, `"`, "")
		upper := strings.ToUpper(normalized)
		on := strings.Index(upper, " ON ")
		if on < 0 {
			return "", ""
		}
		rest := strings.TrimSpace(normalized[on+4:])
		open := strings.LastIndex(rest, "(")
		close := strings.LastIndex(rest, ")")
		if open < 0 || close <= open {
			return "", ""
		}
		target := strings.TrimSpace(rest[:open])
		if using := strings.Index(strings.ToUpper(target), " USING "); using >= 0 {
			target = strings.TrimSpace(target[:using])
		}
		return normalizeDatasetSQL(target), normalizeDatasetSQL(rest[open+1 : close])
	}
	actualTable, actualColumns := indexParts(actual)
	expectedTable, expectedColumns := indexParts(expected)
	return actualTable == expectedTable && actualColumns == expectedColumns
}

func legacyJournalMigrationStatements(
	schema string,
	columns map[string]bool,
	primaryKeyName string,
	primaryKeyDefinition string,
	distribution string,
	distributionMismatch bool,
	targetProduct Product,
) ([]string, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil, fmt.Errorf("unsafe dataset schema %q", schema)
	}
	table := quotedSchema + ".meta_journal"
	productLiteral, err := legacyJournalProductLiteral(targetProduct)
	if err != nil {
		return nil, err
	}
	required := []struct {
		name        string
		declaration string
		expression  string
		notNull     bool
	}{
		{"action_id", "bigint", legacyJournalExpression(columns, "id", "0"), true},
		{"scenario_code", "integer", "NULL", true},
		{"action_kind", "varchar(64)", "NULL", true},
		{"target_product", "varchar(32)", productLiteral, true},
		{"target_node", "varchar(128)", "NULL", false},
		{"target_endpoint", "varchar(256)", legacyJournalExpression(columns, "target", "NULL"), false},
		{"original_state", "text", legacyJournalExpression(columns, "original_value", "NULL"), false},
		{"forward_action", "text", legacyJournalExpression(columns, "forward_sql", "''"), true},
		{"inverse_action", "text", legacyJournalExpression(columns, "inverse_sql", "''"), true},
		{"verify_action", "text", legacyJournalExpression(columns, "verify_sql", "NULL"), false},
		{"verify_value", "text", "NULL", false},
		{"last_error", "text", legacyJournalExpression(columns, "error_text", "NULL"), false},
		{"created_at", "timestamp", "current_timestamp", true},
		{"updated_at", "timestamp", "current_timestamp", true},
	}
	var statements []string
	for _, field := range required {
		if !columns[field.name] {
			statements = append(statements,
				"ALTER TABLE "+table+" ADD COLUMN "+field.name+" "+field.declaration)
		}
		if field.expression != "NULL" {
			statements = append(statements,
				"UPDATE "+table+" SET "+field.name+"="+field.expression+
					" WHERE "+field.name+" IS NULL")
		}
	}
	if columns["scenario"] {
		statements = append(statements,
			"UPDATE "+table+" SET scenario_code="+
				legacyJournalScenarioExpression(columns)+
				" WHERE scenario_code IS NULL OR scenario_code=0")
	} else {
		statements = append(statements,
			"UPDATE "+table+
				" SET scenario_code=NULL WHERE scenario_code=0")
	}
	formalKinds := []string{
		string(ActionSQLMutation),
		string(ActionSessionSet),
		string(ActionSessionTransaction),
		string(ActionGUCFileChange),
		string(ActionNetworkQDisc),
		string(ActionNetworkFirewall),
		string(ActionProcessState),
		string(ActionNodeRole),
		string(ActionCloudFaultJob),
		string(ActionDataBaseline),
	}
	quotedKinds := make([]string, len(formalKinds))
	for index, kind := range formalKinds {
		quotedKinds[index] = "'" + kind + "'"
	}
	statements = append(statements,
		"UPDATE "+table+" SET action_kind='SQL_MUTATION'"+
			" WHERE action_kind IS NULL OR action_kind NOT IN ("+
			strings.Join(quotedKinds, ",")+")")
	productWhere := "target_product IS NULL OR target_product=''"
	if targetProduct.known() {
		productWhere += " OR target_product='unknown'"
	}
	statements = append(statements,
		"UPDATE "+table+" SET target_product="+productLiteral+
			" WHERE "+productWhere)
	for _, field := range required {
		if field.notNull {
			statements = append(statements,
				"ALTER TABLE "+table+" ALTER COLUMN "+field.name+" SET NOT NULL")
		}
	}
	if normalizeDatasetSQL(primaryKeyDefinition) !=
		normalizeDatasetSQL("PRIMARY KEY (run_id,action_id)") {
		if primaryKeyName != "" {
			quotedKey, valid := quoteDatasetIdentifier(primaryKeyName)
			if !valid {
				return nil, fmt.Errorf("unsafe primary-key name %q", primaryKeyName)
			}
			statements = append(statements,
				"ALTER TABLE "+table+" DROP CONSTRAINT "+quotedKey)
		}
		statements = append(statements,
			"ALTER TABLE "+table+" ADD PRIMARY KEY (run_id,action_id)")
	}
	for _, legacy := range []string{
		"id", "scenario", "kind", "target", "original_value",
		"forward_sql", "inverse_sql", "verify_sql", "error_text",
	} {
		if columns[legacy] {
			statements = append(statements,
				"ALTER TABLE "+table+" DROP COLUMN "+legacy)
		}
	}
	if distributionMismatch {
		statements = append(statements,
			"ALTER TABLE "+table+" DISTRIBUTE BY "+distribution)
	}
	return statements, nil
}

func legacyPlanDataConvergenceStatements(
	schema, primaryKeyName, primaryKeyDefinition, distribution string,
	distributionMismatch bool,
) ([]string, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil, fmt.Errorf("unsafe dataset schema %q", schema)
	}
	table := quotedSchema + ".plan_data"
	var statements []string
	for _, column := range []string{
		"dist_key", "stats_target_key", "stats_ndistinct_key",
		"stats_corr_a", "stats_corr_b", "index_unusable_key",
		"index_drop_key", "index_shape_lead", "index_shape_tail",
	} {
		statements = append(statements,
			"ALTER TABLE "+table+" ALTER COLUMN "+column+" SET NOT NULL")
	}
	if normalizeDatasetSQL(primaryKeyDefinition) !=
		normalizeDatasetSQL("PRIMARY KEY (dist_key,id)") {
		if primaryKeyName != "" {
			quotedKey, valid := quoteDatasetIdentifier(primaryKeyName)
			if !valid {
				return nil, fmt.Errorf("unsafe primary-key name %q", primaryKeyName)
			}
			statements = append(statements,
				"ALTER TABLE "+table+" DROP CONSTRAINT "+quotedKey)
		}
		statements = append(statements,
			"ALTER TABLE "+table+" ADD PRIMARY KEY (dist_key,id)")
	}
	if distributionMismatch {
		statements = append(statements,
			"ALTER TABLE "+table+" DISTRIBUTE BY "+distribution)
	}
	return statements, nil
}

func legacyPlanCacheMigrationStatements(
	schema string,
	columns map[string]bool,
	primaryKeyName string,
	primaryKeyDefinition string,
	distribution string,
	distributionMismatch bool,
) ([]string, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil, fmt.Errorf("unsafe dataset schema %q", schema)
	}
	if !columns["scenario"] && !columns["scenario_code"] {
		return nil, fmt.Errorf(
			"meta_plan_cache has no scenario identity column",
		)
	}
	table := quotedSchema + ".meta_plan_cache"
	var statements []string
	if !columns["scenario_code"] {
		statements = append(
			statements,
			"ALTER TABLE "+table+" ADD COLUMN scenario_code integer",
		)
	}
	if columns["scenario"] {
		statements = append(
			statements,
			"UPDATE "+table+" SET scenario_code="+
				legacyJournalScenarioExpression(columns),
		)
	}
	statements = append(
		statements,
		"ALTER TABLE "+table+
			" ALTER COLUMN scenario_code SET NOT NULL",
	)
	if distributionMismatch {
		if normalizeDatasetSQL(distribution) != "hash(signature)" {
			return nil, fmt.Errorf(
				"unsafe meta_plan_cache distribution %q",
				distribution,
			)
		}
		statements = append(
			statements,
			"ALTER TABLE "+table+" DISTRIBUTE BY "+distribution,
		)
	}
	if normalizeDatasetSQL(primaryKeyDefinition) !=
		normalizeDatasetSQL("PRIMARY KEY (signature,scenario_code)") {
		if primaryKeyName != "" {
			quotedKey, valid := quoteDatasetIdentifier(primaryKeyName)
			if !valid {
				return nil, fmt.Errorf(
					"unsafe primary-key name %q",
					primaryKeyName,
				)
			}
			statements = append(
				statements,
				"ALTER TABLE "+table+" DROP CONSTRAINT "+quotedKey+
					", ADD PRIMARY KEY (signature,scenario_code)",
			)
		} else {
			statements = append(
				statements,
				"ALTER TABLE "+table+
					" ADD PRIMARY KEY (signature,scenario_code)",
			)
		}
	}
	if columns["scenario"] {
		statements = append(
			statements,
			"ALTER TABLE "+table+" DROP COLUMN scenario",
		)
	}
	return statements, nil
}

func screenLegacyPlanCacheRows(
	ctx context.Context,
	db journalDatabase,
	schema string,
	columns map[string]bool,
) error {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe plan cache schema %q", schema)
	}
	table := quotedSchema + ".meta_plan_cache"
	if columns["scenario"] {
		expression := legacyJournalScenarioExpression(columns)
		var invalid int64
		if err := db.Scan(
			ctx,
			"SELECT count(*) FROM "+table+
				" WHERE ("+expression+") IS NULL",
			nil,
			&invalid,
		); err != nil {
			return fmt.Errorf("screen legacy plan cache scenarios: %w", err)
		}
		if invalid != 0 {
			return fmt.Errorf(
				"legacy plan cache contains %d unknown scenario rows",
				invalid,
			)
		}
		var collisions int64
		if err := db.Scan(
			ctx,
			"SELECT count(*) FROM ("+
				"SELECT signature,("+expression+") AS mapped_scenario_code "+
				"FROM "+table+" GROUP BY signature,("+expression+") "+
				"HAVING count(*)>1) AS conflicts",
			nil,
			&collisions,
		); err != nil {
			return fmt.Errorf("screen legacy plan cache collisions: %w", err)
		}
		if collisions != 0 {
			return fmt.Errorf(
				"legacy plan cache contains %d scenario-code collisions",
				collisions,
			)
		}
		return nil
	}
	if !columns["scenario_code"] {
		return fmt.Errorf("meta_plan_cache has no scenario identity column")
	}
	codes := legacyJournalScenarioCodes()
	validCodes := make(map[ScenarioCode]bool, len(codes))
	for _, code := range codes {
		validCodes[code] = true
	}
	sortedCodes := make([]int, 0, len(validCodes))
	for code := range validCodes {
		sortedCodes = append(sortedCodes, int(code))
	}
	sort.Ints(sortedCodes)
	valid := make([]string, len(sortedCodes))
	for index, code := range sortedCodes {
		valid[index] = fmt.Sprint(code)
	}
	var invalid int64
	if err := db.Scan(
		ctx,
		"SELECT count(*) FROM "+table+
			" WHERE scenario_code IS NULL OR scenario_code=0"+
			" OR scenario_code NOT IN ("+strings.Join(valid, ",")+")",
		nil,
		&invalid,
	); err != nil {
		return fmt.Errorf("screen formal plan cache scenarios: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf(
			"formal plan cache contains %d invalid scenario code rows",
			invalid,
		)
	}
	return nil
}

func legacyJournalExpression(columns map[string]bool, legacy, fallback string) string {
	if columns[legacy] {
		return legacy
	}
	return fallback
}

func legacyJournalScenarioExpression(columns map[string]bool) string {
	if !columns["scenario"] {
		return "NULL"
	}
	codes := legacyJournalScenarioCodes()
	names := make([]string, 0, len(codes))
	for name := range codes {
		names = append(names, name)
	}
	sort.Strings(names)
	var expression strings.Builder
	expression.WriteString("CASE scenario")
	for _, name := range names {
		expression.WriteString("\n\t\tWHEN '")
		expression.WriteString(strings.ReplaceAll(name, "'", "''"))
		expression.WriteString("' THEN ")
		expression.WriteString(fmt.Sprint(codes[name]))
	}
	expression.WriteString("\n\t\tELSE NULL END")
	return expression.String()
}

func legacyJournalScenarioCodes() map[string]ScenarioCode {
	codes := map[string]ScenarioCode{}
	catalog := DefaultScenarioCatalog()
	for _, definition := range catalog.Definitions() {
		codes[definition.Name] = definition.Code
	}
	for legacy, canonical := range map[string]string{
		"plan_stats_target":    "planchange_stats_target",
		"plan_index_unusable":  "planchange_index_unusable",
		"plan_stats_ndistinct": "planchange_stats_ndistinct",
		"plan_stats_extended":  "planchange_stats_extended",
		"plan_index_drop":      "planchange_index_drop",
		"plan_index_shape":     "planchange_index_shape",
	} {
		definition, err := catalog.Resolve(canonical)
		if err != nil {
			panic(err)
		}
		codes[legacy] = definition.Code
	}
	return codes
}

func legacyJournalProductLiteral(product Product) (string, error) {
	if product == "" || product == ProductUnknown {
		return "'unknown'", nil
	}
	if !product.known() {
		return "", fmt.Errorf("cannot migrate journal for unknown product %q", product)
	}
	return "'" + strings.ReplaceAll(string(product), "'", "''") + "'", nil
}

func legacyBatchMigrationStatements(
	schema string,
	columns map[string]bool,
) ([]string, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil, fmt.Errorf("unsafe dataset schema %q", schema)
	}
	table := quotedSchema + ".meta_batches"
	var statements []string
	if !columns["target_rows"] {
		statements = append(statements,
			"ALTER TABLE "+table+" ADD COLUMN target_rows bigint",
			"UPDATE "+table+" SET target_rows=high_water WHERE target_rows IS NULL",
			"ALTER TABLE "+table+" ALTER COLUMN target_rows SET NOT NULL")
	}
	if !columns["estimated_row_bytes"] {
		statements = append(statements,
			"ALTER TABLE "+table+" ADD COLUMN estimated_row_bytes bigint",
			"UPDATE "+table+" SET estimated_row_bytes=0 WHERE estimated_row_bytes IS NULL",
			"ALTER TABLE "+table+" ALTER COLUMN estimated_row_bytes SET NOT NULL")
	}
	return statements, nil
}

func quoteDatasetIdentifier(identifier string) (string, bool) {
	if !identifierRE.MatchString(identifier) {
		return "", false
	}
	return `"` + identifier + `"`, true
}

func sortedDatasetColumnNames(columns map[string]bool) []string {
	names := make([]string, 0, len(columns))
	for name := range columns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type sqlJournalStore struct {
	db          journalDatabase
	schema      string
	schemaValid bool
}

func NewSQLJournal(db *Database, schema string) *Journal {
	return NewSQLJournalWithValidation(db, schema, true)
}

func NewSQLJournalWithValidation(
	db *Database,
	schema string,
	validationEnabled bool,
) *Journal {
	store := newSQLJournalStore(databaseJournalDB{db: db}, schema)
	return NewJournalWithValidation(
		store,
		dbActionExecutor{db: db},
		validationEnabled,
		db.journalTargetProduct(),
	)
}

type journalRows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

type journalDatabase interface {
	Scan(ctx context.Context, query string, args []any, dest ...any) error
	Exec(ctx context.Context, query string, args ...any) (sql.Result, error)
	Query(ctx context.Context, query string, args ...any) (journalRows, error)
}

type databaseJournalDB struct {
	db *Database
}

func (d databaseJournalDB) BeginJournalTransaction(
	parent context.Context,
) (journalSQLTransaction, error) {
	ctx, cancel := d.db.operationContext(parent)
	tx, err := d.db.pool.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	return &databaseDatasetTransaction{Tx: tx, cancel: cancel}, nil
}

func (d databaseJournalDB) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	return d.db.Scan(ctx, query, args, dest...)
}

func (d databaseJournalDB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.db.Exec(ctx, query, args...)
}

func (d databaseJournalDB) Query(ctx context.Context, query string, args ...any) (journalRows, error) {
	return d.db.Query(ctx, query, args...)
}

func newSQLJournalStore(db journalDatabase, schema string) *sqlJournalStore {
	quotedSchema, ok := quoteDatasetSchema(schema)
	return &sqlJournalStore{db: db, schema: quotedSchema, schemaValid: ok}
}

func (s *sqlJournalStore) InsertPlanned(ctx context.Context, action Action) (Action, error) {
	if err := s.validateSchema(); err != nil {
		return Action{}, err
	}
	action, err := normalizeJournalAction(action)
	if err != nil {
		return Action{}, err
	}
	beginner, ok := s.db.(journalTransactionBeginner)
	if !ok {
		return Action{}, fmt.Errorf(
			"journal database does not support explicit transactions")
	}
	tx, err := beginner.BeginJournalTransaction(ctx)
	if err != nil {
		return Action{}, fmt.Errorf("begin journal allocation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(
		ctx, journalLockStatement(), action.RunID,
	); err != nil {
		return Action{}, fmt.Errorf(
			"lock journal run %s: %w", action.RunID, err,
		)
	}
	var actionID int64
	err = tx.ScanContext(ctx, journalInsertStatement(s.schema), []any{
		action.RunID,
		int(action.ScenarioCode),
		string(action.Kind),
		string(action.TargetProduct),
		nullableString(action.Node),
		nullableString(action.Target),
		nullableActionPayload(action.Original),
		string(action.Forward),
		journalInverseActionPayload(action),
		nullableActionPayload(action.Verify),
		nil,
		string(MutationPlanned),
		nullableString(action.LastError),
	}, &actionID)
	if err != nil {
		return Action{}, err
	}
	if err := tx.Commit(); err != nil {
		return Action{}, fmt.Errorf("commit journal allocation: %w", err)
	}
	committed = true
	action.Sequence = actionID
	action.State = MutationPlanned
	return action, nil
}

func (s *sqlJournalStore) SetState(ctx context.Context, runID string, actionID int64, state MutationState, detail string) error {
	if err := s.validateSchema(); err != nil {
		return err
	}
	if strings.TrimSpace(runID) == "" || actionID <= 0 {
		return fmt.Errorf("journal run ID and positive action ID are required")
	}
	_, err := s.db.Exec(
		ctx,
		journalStateStatement(s.schema),
		string(state),
		nullableString(journalSafeErrorText(detail)),
		runID,
		actionID,
	)
	return err
}

func (s *sqlJournalStore) ClaimAction(
	ctx context.Context,
	action Action,
) (bool, error) {
	if err := s.validateSchema(); err != nil {
		return false, err
	}
	if strings.TrimSpace(action.RunID) == "" || action.Sequence <= 0 {
		return false, fmt.Errorf(
			"journal run ID and positive action ID are required",
		)
	}
	result, err := s.db.Exec(
		ctx,
		journalClaimStatement(s.schema),
		string(MutationRestoring),
		nil,
		action.RunID,
		action.Sequence,
		string(action.State),
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read journal claim result: %w", err)
	}
	return affected == 1, nil
}

func (s *sqlJournalStore) Pending(ctx context.Context, runID string) ([]Action, error) {
	if err := s.validateSchema(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("journal run ID is required")
	}
	rows, err := s.db.Query(ctx, journalPendingStatement(s.schema), runID, string(MutationRestored))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var actions []Action
	for rows.Next() {
		var action Action
		var (
			kind, targetProduct                       string
			original, forward, inverse, verify        string
			legacyVerifyExpected, state, lastErrorRaw string
		)
		if err := rows.Scan(
			&action.Sequence,
			&action.RunID,
			&action.ScenarioCode,
			&kind,
			&targetProduct,
			&action.Node,
			&action.Target,
			&original,
			&forward,
			&inverse,
			&verify,
			&legacyVerifyExpected,
			&state,
			&lastErrorRaw,
		); err != nil {
			return nil, err
		}
		action.Kind = ActionKind(kind)
		action.TargetProduct = Product(targetProduct)
		action.LegacySQL = storedJournalPayloadIsLegacy(
			original, forward, inverse, verify,
		)
		if action.LegacySQL && !action.Kind.valid() {
			action.Kind = ActionSQLMutation
		}
		if action.LegacySQL && action.TargetProduct == "" {
			action.TargetProduct = ProductUnknown
		}
		action.Original = storedActionValuePayload(original)
		action.Forward = storedSQLActionPayload(forward, "")
		action.Inverse = storedSQLActionPayload(inverse, "")
		action.Verify = storedSQLActionPayload(verify, legacyVerifyExpected)
		action.State = MutationState(state)
		action.LastError = lastErrorRaw
		if err := validateLoadedJournalAction(action); err != nil {
			return nil, fmt.Errorf(
				"validate journal action %s/%d: %w",
				action.RunID,
				action.Sequence,
				err,
			)
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

func (s *sqlJournalStore) StaleRuns(ctx context.Context) ([]string, error) {
	if err := s.validateSchema(); err != nil {
		return nil, err
	}
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

func (s *sqlJournalStore) validateSchema() error {
	if !s.schemaValid {
		return fmt.Errorf("unsafe journal schema")
	}
	return nil
}

func journalInsertStatement(schema string) string {
	table := schema + ".meta_journal"
	return `INSERT INTO ` + table + `(
		run_id,action_id,scenario_code,action_kind,target_product,
		target_node,target_endpoint,original_state,forward_action,
		inverse_action,verify_action,verify_value,state,last_error
	)
	SELECT $1,COALESCE(max(j.action_id),0)+1,
		$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13
	FROM ` + table + ` j
	WHERE j.run_id=$1
	RETURNING action_id`
}

func journalLockStatement() string {
	return "SELECT pg_advisory_xact_lock(hashtext($1))"
}

func journalStateStatement(schema string) string {
	return "UPDATE " + schema + ".meta_journal SET state=$1,last_error=$2,updated_at=current_timestamp WHERE run_id=$3 AND action_id=$4"
}

func journalClaimStatement(schema string) string {
	return "UPDATE " + schema +
		".meta_journal SET state=$1,last_error=$2," +
		"updated_at=current_timestamp " +
		"WHERE run_id=$3 AND action_id=$4 AND state=$5"
}

func journalPendingStatement(schema string) string {
	return `SELECT action_id,run_id,scenario_code,action_kind,target_product,
		COALESCE(target_node,''),COALESCE(target_endpoint,''),
		COALESCE(original_state,''),forward_action,inverse_action,
		COALESCE(verify_action,''),COALESCE(verify_value,''),
		state,COALESCE(last_error,'')
		FROM ` + schema + `.meta_journal
		WHERE run_id=$1 AND state<>$2
		ORDER BY action_id`
}

func normalizeJournalAction(action Action) (Action, error) {
	action.LastError = journalSafeErrorText(action.LastError)
	if err := action.Validate(); err != nil {
		return Action{}, err
	}
	return action, nil
}

func journalScenarioCode(name string) ScenarioCode {
	if definition, err := DefaultScenarioCatalog().Resolve(name); err == nil {
		return definition.Code
	}
	return legacyJournalScenarioCodes()[name]
}

func journalScenarioName(code ScenarioCode) string {
	if definition, err := DefaultScenarioCatalog().LookupCode(code); err == nil {
		return definition.Name
	}
	return fmt.Sprint(code)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableActionPayload(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func journalInverseActionPayload(action Action) any {
	if len(action.Inverse) == 0 {
		return "{}"
	}
	return string(action.Inverse)
}

func storedActionValuePayload(value string) json.RawMessage {
	if value == "" {
		return nil
	}
	if json.Valid([]byte(value)) {
		var object map[string]any
		if json.Unmarshal([]byte(value), &object) == nil {
			return json.RawMessage(value)
		}
	}
	return marshalActionPayload(struct {
		Value string `json:"value"`
	}{Value: value})
}

func storedJournalPayloadIsLegacy(values ...string) bool {
	for _, value := range values {
		if value == "" {
			continue
		}
		var object map[string]any
		if !json.Valid([]byte(value)) ||
			json.Unmarshal([]byte(value), &object) != nil {
			return true
		}
	}
	return false
}

func validateLoadedJournalAction(action Action) error {
	validated := action
	if action.LegacySQL && action.TargetProduct == ProductUnknown {
		validated.TargetProduct = ProductOpenGauss
	}
	return validated.Validate()
}

func storedSQLActionPayload(value, legacyExpected string) json.RawMessage {
	if value == "" {
		return nil
	}
	if json.Valid([]byte(value)) {
		var object map[string]any
		if json.Unmarshal([]byte(value), &object) == nil {
			return json.RawMessage(value)
		}
	}
	if legacyExpected != "" {
		return marshalActionPayload(struct {
			SQL      string `json:"sql"`
			Expected string `json:"expected"`
		}{SQL: value, Expected: legacyExpected})
	}
	return marshalActionPayload(struct {
		SQL string `json:"sql"`
	}{SQL: value})
}

func screenLegacyJournalRows(
	ctx context.Context,
	db journalDatabase,
	schema string,
	columns map[string]bool,
) error {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe journal schema %q", schema)
	}
	expressions := []string{
		legacyJournalScreenExpression(columns, "original_state", "original_value"),
		legacyJournalScreenExpression(columns, "forward_action", "forward_sql"),
		legacyJournalScreenExpression(columns, "inverse_action", "inverse_sql"),
		legacyJournalScreenExpression(columns, "verify_action", "verify_sql"),
		legacyJournalScreenExpression(columns, "target_endpoint", "target"),
		legacyJournalScreenExpression(columns, "last_error", "error_text"),
	}
	rows, err := db.Query(
		ctx,
		"SELECT "+strings.Join(expressions, ",")+" FROM "+
			quotedSchema+".meta_journal",
	)
	if err != nil {
		return fmt.Errorf("screen legacy journal payloads: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var values [6]string
		if err := rows.Scan(
			&values[0],
			&values[1],
			&values[2],
			&values[3],
			&values[4],
			&values[5],
		); err != nil {
			return fmt.Errorf("screen legacy journal payloads: %w", err)
		}
		for _, value := range values {
			if storedJournalValueContainsSecret(value) {
				return fmt.Errorf(
					"legacy journal contains forbidden credential material",
				)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("screen legacy journal payloads: %w", err)
	}
	return nil
}

func legacyJournalScreenExpression(
	columns map[string]bool,
	formal, legacy string,
) string {
	var parts []string
	if columns[formal] {
		parts = append(parts, "COALESCE("+formal+",'')")
	}
	if columns[legacy] {
		parts = append(parts, "COALESCE("+legacy+",'')")
	}
	if len(parts) == 0 {
		return "''"
	}
	return strings.Join(parts, " || chr(10) || ")
}

func storedJournalValueContainsSecret(value string) bool {
	if value == "" {
		return false
	}
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		if object, ok := decoded.(map[string]any); ok {
			return secretActionPayloadKey(object) != "" ||
				actionPayloadContainsCredentialMaterial(object)
		}
	}
	return actionPayloadContainsCredentialMaterial(value)
}

type actionSQLDatabase interface {
	Exec(context.Context, string, ...any) (sql.Result, error)
	Scan(context.Context, string, []any, ...any) error
}

type actionSQLSessionDatabase interface {
	ExecSession(context.Context, ...string) error
}

type dbActionExecutor struct{ db actionSQLDatabase }

type sqlActionPayload struct {
	SQL        string   `json:"sql,omitempty"`
	SessionSQL []string `json:"session_sql,omitempty"`
	Expected   string   `json:"expected,omitempty"`
}

func (e dbActionExecutor) Preflight(_ context.Context, action Action) error {
	if err := validateSQLActionKind(action); err != nil {
		return err
	}
	if err := preflightSQLActionPayload(
		"forward", action.Forward, action.LegacySQL,
	); err != nil {
		return err
	}
	if action.Kind.persistent() {
		if err := preflightSQLActionPayload(
			"inverse", action.Inverse, action.LegacySQL,
		); err != nil {
			return err
		}
	}
	if len(action.Verify) != 0 {
		if err := preflightSQLActionPayload(
			"verify", action.Verify, false,
		); err != nil {
			return err
		}
	}
	return nil
}

func (e dbActionExecutor) Apply(ctx context.Context, action Action) error {
	if err := validateSQLActionKind(action); err != nil {
		return err
	}
	return e.execute(ctx, action, action.Forward)
}

func (e dbActionExecutor) Restore(ctx context.Context, action Action) error {
	if err := validateSQLActionKind(action); err != nil {
		return err
	}
	return e.execute(ctx, action, action.Inverse)
}

func (e dbActionExecutor) VerifyRestored(
	ctx context.Context,
	action Action,
) error {
	if err := validateSQLActionKind(action); err != nil {
		return err
	}
	if len(action.Verify) == 0 {
		return nil
	}
	payload, err := decodeSQLActionPayload(action.Verify)
	if err != nil {
		return fmt.Errorf("decode SQL verify action: %w", err)
	}
	if len(payload.SessionSQL) != 0 {
		return fmt.Errorf("SQL verify action does not support session statements")
	}
	var actual string
	if err := e.db.Scan(ctx, payload.SQL, nil, &actual); err != nil {
		return err
	}
	if !databaseValuesEqual(actual, payload.Expected) {
		return fmt.Errorf("got %q, want %q", actual, payload.Expected)
	}
	return nil
}

func validateSQLActionKind(action Action) error {
	if action.Kind != ActionSQLMutation && action.Kind != ActionDataBaseline {
		return fmt.Errorf("SQL executor does not support action kind %q", action.Kind)
	}
	return nil
}

func (e dbActionExecutor) execute(
	ctx context.Context,
	action Action,
	raw json.RawMessage,
) error {
	payload, err := decodeSQLActionPayload(raw)
	if err != nil {
		return fmt.Errorf("decode SQL action: %w", err)
	}
	if len(payload.SessionSQL) != 0 {
		if action.LegacySQL {
			return fmt.Errorf("legacy SQL action does not support session statements")
		}
		database, ok := e.db.(actionSQLSessionDatabase)
		if !ok {
			return fmt.Errorf("SQL action database does not support session execution")
		}
		return database.ExecSession(ctx, payload.SessionSQL...)
	}
	if !action.LegacySQL {
		_, err := e.db.Exec(ctx, payload.SQL)
		return err
	}
	statements, err := legacySQLStatements(payload.SQL)
	if err != nil {
		return fmt.Errorf("scan legacy SQL action: %w", err)
	}
	for _, statement := range statements {
		if _, err := e.db.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func preflightSQLActionPayload(
	name string,
	raw json.RawMessage,
	allowLegacyMultiple bool,
) error {
	payload, err := decodeSQLActionPayload(raw)
	if err != nil {
		return fmt.Errorf("%s SQL payload: %w", name, err)
	}
	if len(payload.SessionSQL) != 0 {
		if allowLegacyMultiple {
			return fmt.Errorf(
				"%s SQL payload cannot use session statements with legacy provenance",
				name,
			)
		}
		for _, statement := range payload.SessionSQL {
			statements, err := legacySQLStatements(statement)
			if err != nil {
				return fmt.Errorf("%s SQL payload: %w", name, err)
			}
			if len(statements) != 1 {
				return fmt.Errorf(
					"%s SQL session payload statements must each contain one executable statement",
					name,
				)
			}
		}
		return nil
	}
	statements, err := legacySQLStatements(payload.SQL)
	if err != nil {
		return fmt.Errorf("%s SQL payload: %w", name, err)
	}
	if len(statements) == 0 {
		return fmt.Errorf("%s SQL payload has no executable statement", name)
	}
	if !allowLegacyMultiple && len(statements) != 1 {
		return fmt.Errorf(
			"%s SQL payload must contain one executable statement",
			name,
		)
	}
	return nil
}

func decodeSQLActionPayload(raw json.RawMessage) (sqlActionPayload, error) {
	var payload sqlActionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return sqlActionPayload{}, err
	}
	payload.SQL = strings.TrimSpace(payload.SQL)
	for index := range payload.SessionSQL {
		payload.SessionSQL[index] = strings.TrimSpace(payload.SessionSQL[index])
		if payload.SessionSQL[index] == "" {
			return sqlActionPayload{}, fmt.Errorf(
				"session SQL statement %d is empty",
				index+1,
			)
		}
	}
	if payload.SQL == "" && len(payload.SessionSQL) == 0 {
		return sqlActionPayload{}, fmt.Errorf("SQL or session SQL is required")
	}
	if payload.SQL != "" && len(payload.SessionSQL) != 0 {
		return sqlActionPayload{}, fmt.Errorf(
			"SQL and session SQL are mutually exclusive",
		)
	}
	return payload, nil
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

// legacySQLStatements is used only for rows explicitly tagged as LegacySQL.
// New typed actions are preflighted as one statement and executed unchanged.
func legacySQLStatements(query string) ([]string, error) {
	var statements []string
	start := 0
	hasCode := false
	for index := 0; index < len(query); {
		switch query[index] {
		case '\'':
			hasCode = true
			next, err := scanSQLQuoted(query, index, '\'')
			if err != nil {
				return nil, err
			}
			index = next
		case '"':
			hasCode = true
			next, err := scanSQLQuoted(query, index, '"')
			if err != nil {
				return nil, err
			}
			index = next
		case '$':
			delimiter := sqlDollarQuoteDelimiter(query, index)
			if delimiter == "" {
				hasCode = true
				index++
				continue
			}
			hasCode = true
			contentStart := index + len(delimiter)
			closeOffset := strings.Index(query[contentStart:], delimiter)
			if closeOffset < 0 {
				return nil, fmt.Errorf("unterminated dollar-quoted string")
			}
			index = contentStart + closeOffset + len(delimiter)
		case '-':
			if index+1 < len(query) && query[index+1] == '-' {
				index += 2
				for index < len(query) && query[index] != '\n' {
					index++
				}
				continue
			}
			hasCode = true
			index++
		case '/':
			if index+1 < len(query) && query[index+1] == '*' {
				next, err := scanSQLBlockComment(query, index)
				if err != nil {
					return nil, err
				}
				index = next
				continue
			}
			hasCode = true
			index++
		case ';':
			if hasCode {
				statement := strings.TrimSpace(query[start:index])
				statements = append(statements, statement)
			}
			start = index + 1
			hasCode = false
			index++
		default:
			if query[index] != ' ' && query[index] != '\t' &&
				query[index] != '\r' && query[index] != '\n' {
				hasCode = true
			}
			index++
		}
	}
	if hasCode {
		statements = append(statements, strings.TrimSpace(query[start:]))
	}
	return statements, nil
}

func scanSQLQuoted(query string, start int, quote byte) (int, error) {
	for index := start + 1; index < len(query); index++ {
		if query[index] == '\\' && quote == '\'' && index+1 < len(query) {
			index++
			continue
		}
		if query[index] != quote {
			continue
		}
		if index+1 < len(query) && query[index+1] == quote {
			index++
			continue
		}
		return index + 1, nil
	}
	return 0, fmt.Errorf("unterminated quoted string")
}

func sqlDollarQuoteDelimiter(query string, start int) string {
	if start >= len(query) || query[start] != '$' {
		return ""
	}
	for index := start + 1; index < len(query); index++ {
		if query[index] == '$' {
			if index == start+1 {
				return "$$"
			}
			return query[start : index+1]
		}
		if (query[index] < 'A' || query[index] > 'Z') &&
			(query[index] < 'a' || query[index] > 'z') &&
			(query[index] < '0' || query[index] > '9') &&
			query[index] != '_' {
			return ""
		}
		if index == start+1 && query[index] >= '0' && query[index] <= '9' {
			return ""
		}
	}
	return ""
}

func scanSQLBlockComment(query string, start int) (int, error) {
	depth := 1
	for index := start + 2; index < len(query); {
		switch {
		case index+1 < len(query) &&
			query[index] == '/' && query[index+1] == '*':
			depth++
			index += 2
		case index+1 < len(query) &&
			query[index] == '*' && query[index+1] == '/':
			depth--
			index += 2
			if depth == 0 {
				return index, nil
			}
		default:
			index++
		}
	}
	return 0, fmt.Errorf("unterminated block comment")
}
