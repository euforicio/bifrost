package sqlitetopostgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// TableReport is the post-copy verification result for one discovered source
// table. Digest covers every source column and is insensitive to row order.
type TableReport struct {
	Store      string
	Table      string
	SourceRows int64
	TargetRows int64
	Digest     string
}

// Report describes a committed migration or a read-only verification run.
type Report struct {
	ConfigSHA256 string
	LogsSHA256   string
	Tables       []TableReport
}

// Migrate copies both retained SQLite snapshots into an already initialized
// PostgreSQL schema. All table copies, migration-ledger reconciliation,
// sequence resets, and verification execute in one serializable transaction.
func Migrate(ctx context.Context, snapshots Snapshots, postgresDSN, schema string) (Report, error) {
	if err := validateDestination(postgresDSN, schema); err != nil {
		return Report{}, err
	}
	sources, err := openSources(ctx, snapshots)
	if err != nil {
		return Report{}, err
	}
	defer closeSources(sources)

	conn, err := pgx.Connect(ctx, postgresDSN)
	if err != nil {
		return Report{}, fmt.Errorf("connect postgres: %w", err)
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Report{}, fmt.Errorf("begin postgres migration transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := validatePostgresRuntime(ctx, tx); err != nil {
		return Report{}, err
	}

	targets, err := discoverTargetTables(ctx, tx, schema)
	if err != nil {
		return Report{}, err
	}
	mapped, err := mapSourceTables(sources, targets)
	if err != nil {
		return Report{}, err
	}
	if err := lockTargetTables(ctx, tx, schema, mapped); err != nil {
		return Report{}, err
	}
	if err := checkTargetConstraints(ctx, tx, schema, mapped); err != nil {
		return Report{}, err
	}
	if err := requireEmptyTarget(ctx, tx, schema, mapped); err != nil {
		return Report{}, err
	}

	loadPlan, err := buildTableLoadPlan(mapped, targets)
	if err != nil {
		return Report{}, err
	}
	reports := make(map[string]TableReport, len(mapped)+1)
	for _, name := range loadPlan.Order {
		source := mapped[name]
		target := targets[name]
		sourceDB := sourceForStore(sources, source.Store)
		fingerprint, copied, err := copyTable(ctx, tx, schema, sourceDB.tx, source, target, loadPlan.DeferredColumns[name])
		if err != nil {
			return Report{}, fmt.Errorf("copy %s sqlite table %s: %w", source.Store, name, err)
		}
		if copied != source.Rows {
			return Report{}, fmt.Errorf("copy %s sqlite table %s: copied %d rows, snapshot count is %d", source.Store, name, copied, source.Rows)
		}
		reports[name] = TableReport{Store: source.Store, Table: name, SourceRows: source.Rows, Digest: fingerprint.digest()}
	}
	if err := restoreDeferredForeignKeys(ctx, tx, schema, sources, mapped, targets, loadPlan.DeferredColumns); err != nil {
		return Report{}, err
	}

	migrationReport, err := reconcileMigrationLedger(ctx, tx, schema, sources, targets[migrationTableName])
	if err != nil {
		return Report{}, err
	}
	reports[migrationTableName] = migrationReport
	if err := resetCopiedSequences(ctx, tx, schema, mapped, targets); err != nil {
		return Report{}, err
	}
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		return Report{}, fmt.Errorf("validate deferred postgres constraints: %w", err)
	}
	if err := verifyTargetTables(ctx, tx, schema, mapped, targets, reports); err != nil {
		return Report{}, err
	}
	if err := verifyTargetForeignKeys(ctx, tx, schema, mapped, targets); err != nil {
		return Report{}, err
	}
	if err := verifyCopiedSequences(ctx, tx, schema, mapped); err != nil {
		return Report{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("commit postgres migration transaction: %w", err)
	}
	return buildReport(snapshots, reports), nil
}

// Verify compares both retained SQLite snapshots with PostgreSQL in a stable
// read-only transaction. It verifies every copied value, per-table counts,
// migration-ledger metadata, and all discovered destination relationships.
func Verify(ctx context.Context, snapshots Snapshots, postgresDSN, schema string) (Report, error) {
	if err := validateDestination(postgresDSN, schema); err != nil {
		return Report{}, err
	}
	sources, err := openSources(ctx, snapshots)
	if err != nil {
		return Report{}, err
	}
	defer closeSources(sources)

	conn, err := pgx.Connect(ctx, postgresDSN)
	if err != nil {
		return Report{}, fmt.Errorf("connect postgres: %w", err)
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Report{}, fmt.Errorf("begin postgres verification transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := validatePostgresRuntime(ctx, tx); err != nil {
		return Report{}, err
	}

	targets, err := discoverTargetTables(ctx, tx, schema)
	if err != nil {
		return Report{}, err
	}
	mapped, err := mapSourceTables(sources, targets)
	if err != nil {
		return Report{}, err
	}
	if err := checkTargetConstraints(ctx, tx, schema, mapped); err != nil {
		return Report{}, err
	}
	reports := make(map[string]TableReport, len(mapped)+1)
	for name, source := range mapped {
		sourceDB := sourceForStore(sources, source.Store)
		fingerprint, count, err := fingerprintSourceTable(ctx, sourceDB.tx, source, targets[name])
		if err != nil {
			return Report{}, fmt.Errorf("fingerprint %s sqlite table %s: %w", source.Store, name, err)
		}
		if count != source.Rows {
			return Report{}, fmt.Errorf("fingerprint %s sqlite table %s: read %d rows, snapshot count is %d", source.Store, name, count, source.Rows)
		}
		reports[name] = TableReport{Store: source.Store, Table: name, SourceRows: count, Digest: fingerprint.digest()}
	}
	migrationReport, err := verifyMigrationLedger(ctx, tx, schema, sources, targets[migrationTableName])
	if err != nil {
		return Report{}, err
	}
	reports[migrationTableName] = migrationReport
	if err := verifyTargetTables(ctx, tx, schema, mapped, targets, reports); err != nil {
		return Report{}, err
	}
	if err := verifyTargetForeignKeys(ctx, tx, schema, mapped, targets); err != nil {
		return Report{}, err
	}
	if err := verifyCopiedSequences(ctx, tx, schema, mapped); err != nil {
		return Report{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("finish postgres verification transaction: %w", err)
	}
	return buildReport(snapshots, reports), nil
}

func validateDestination(postgresDSN, schema string) error {
	if strings.TrimSpace(postgresDSN) == "" {
		return fmt.Errorf("postgres dsn is required")
	}
	if schema == "" || strings.ContainsRune(schema, 0) || len(schema) > 63 {
		return fmt.Errorf("invalid postgres schema %q", schema)
	}
	return nil
}

func validatePostgresRuntime(ctx context.Context, tx pgx.Tx) error {
	var version int
	var encoding string
	if err := tx.QueryRow(ctx, `SELECT current_setting('server_version_num')::int, current_setting('server_encoding')`).Scan(&version, &encoding); err != nil {
		return fmt.Errorf("inspect postgres runtime compatibility: %w", err)
	}
	if version < 160000 {
		return fmt.Errorf("postgres server_version_num=%d is unsupported; Bifrost requires PostgreSQL 16 or later", version)
	}
	normalizedEncoding := strings.ToUpper(strings.ReplaceAll(encoding, "-", ""))
	if normalizedEncoding != "UTF8" {
		return fmt.Errorf("postgres server_encoding=%q is unsupported; Bifrost migration requires UTF8", encoding)
	}
	return nil
}

func openSources(ctx context.Context, snapshots Snapshots) ([]*sourceDatabase, error) {
	config, err := openSourceDatabase(ctx, "config", snapshots.ConfigPath)
	if err != nil {
		return nil, err
	}
	logs, err := openSourceDatabase(ctx, "logs", snapshots.LogsPath)
	if err != nil {
		config.close()
		return nil, err
	}
	return []*sourceDatabase{config, logs}, nil
}

func closeSources(sources []*sourceDatabase) {
	for _, source := range sources {
		source.close()
	}
}

func sourceForStore(sources []*sourceDatabase, store string) *sourceDatabase {
	for _, source := range sources {
		if source.store == store {
			return source
		}
	}
	return nil
}

func lockTargetTables(ctx context.Context, tx pgx.Tx, schema string, mapped map[string]*sourceTable) error {
	names := make([]string, 0, len(mapped)+1)
	for name := range mapped {
		names = append(names, name)
	}
	names = append(names, migrationTableName)
	sort.Strings(names)
	qualified := make([]string, len(names))
	for i, name := range names {
		qualified[i] = qualifiedTable(schema, name)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+strings.Join(qualified, ", ")+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock postgres migration tables: %w", err)
	}
	return nil
}

func checkTargetConstraints(ctx context.Context, tx pgx.Tx, schema string, mapped map[string]*sourceTable) error {
	names := make([]string, 0, len(mapped))
	for name := range mapped {
		names = append(names, name)
	}
	rows, err := tx.Query(ctx, `
		SELECT rel.relname, con.conname
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace ns ON ns.oid = rel.relnamespace
		WHERE ns.nspname = $1 AND rel.relname = ANY($2) AND NOT con.convalidated
		ORDER BY rel.relname, con.conname`, schema, names)
	if err != nil {
		return fmt.Errorf("inspect postgres constraints: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, constraint string
		if err := rows.Scan(&table, &constraint); err != nil {
			return fmt.Errorf("read unvalidated postgres constraint: %w", err)
		}
		return fmt.Errorf("postgres constraint %s on table %s is not validated", constraint, table)
	}
	return rows.Err()
}

func requireEmptyTarget(ctx context.Context, tx pgx.Tx, schema string, mapped map[string]*sourceTable) error {
	names := make([]string, 0, len(mapped))
	for name := range mapped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var count int64
		if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+qualifiedTable(schema, name)).Scan(&count); err != nil {
			return fmt.Errorf("count postgres table %s: %w", name, err)
		}
		if count != 0 {
			return fmt.Errorf("postgres table %s contains %d rows; refusing to overwrite or merge business data", name, count)
		}
	}
	return nil
}

type sqliteCopySource struct {
	rows        *sql.Rows
	columns     []*targetColumn
	nilColumns  map[string]struct{}
	current     []any
	err         error
	fingerprint tableFingerprint
}

func (s *sqliteCopySource) Next() bool {
	if s.err != nil || !s.rows.Next() {
		return false
	}
	raw := make([]any, len(s.columns))
	destinations := make([]any, len(raw))
	for i := range raw {
		destinations[i] = &raw[i]
	}
	if err := s.rows.Scan(destinations...); err != nil {
		s.err = err
		return false
	}
	s.current = make([]any, len(raw))
	for i := range raw {
		s.current[i], s.err = convertValue(s.columns[i], raw[i])
		if s.err != nil {
			s.err = fmt.Errorf("column %s: %w", s.columns[i].Name, s.err)
			return false
		}
	}
	if err := s.fingerprint.add(s.columns, s.current); err != nil {
		s.err = err
		return false
	}
	for i, column := range s.columns {
		if _, deferred := s.nilColumns[column.Name]; deferred {
			s.current[i] = nil
		}
	}
	return true
}

func (s *sqliteCopySource) Values() ([]any, error) { return s.current, s.err }

func (s *sqliteCopySource) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.rows.Err()
}

func copyTable(ctx context.Context, tx pgx.Tx, schema string, sourceTx *sql.Tx, source *sourceTable, target *targetTable, deferredColumns map[string]struct{}) (tableFingerprint, int64, error) {
	columns, names := mappedColumns(source, target)
	rows, err := sourceTx.QueryContext(ctx, selectSourceColumns(source.Name, names))
	if err != nil {
		return tableFingerprint{}, 0, err
	}
	defer rows.Close()
	copySource := &sqliteCopySource{rows: rows, columns: columns, nilColumns: deferredColumns}
	copied, err := tx.CopyFrom(ctx, pgx.Identifier{schema, source.Name}, names, copySource)
	if err != nil {
		return tableFingerprint{}, copied, err
	}
	if err := copySource.Err(); err != nil {
		return tableFingerprint{}, copied, err
	}
	return copySource.fingerprint, copied, nil
}

func restoreDeferredForeignKeys(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	sources []*sourceDatabase,
	mapped map[string]*sourceTable,
	targets map[string]*targetTable,
	deferred map[string]map[string]struct{},
) error {
	var names []string
	for name, columns := range deferred {
		if len(columns) > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for tableIndex, name := range names {
		source := mapped[name]
		target := targets[name]
		if len(source.PrimaryKey) == 0 {
			return fmt.Errorf("restore deferred foreign keys for %s: source table has no primary key", name)
		}
		columnNames := append([]string(nil), source.PrimaryKey...)
		for _, column := range source.Columns {
			if _, ok := deferred[name][column.Name]; ok && !containsString(columnNames, column.Name) {
				columnNames = append(columnNames, column.Name)
			}
		}
		columns := make([]*targetColumn, len(columnNames))
		quotedColumns := make([]string, len(columnNames))
		for i, columnName := range columnNames {
			columns[i] = target.ColumnByName[columnName]
			quotedColumns[i] = quotePostgresIdentifier(columnName)
		}
		temporaryName := fmt.Sprintf("bifrost_deferred_%d", tableIndex)
		if _, err := tx.Exec(ctx, "CREATE TEMP TABLE "+quotePostgresIdentifier(temporaryName)+" ON COMMIT DROP AS SELECT "+
			strings.Join(quotedColumns, ", ")+" FROM "+qualifiedTable(schema, name)+" WITH NO DATA"); err != nil {
			return fmt.Errorf("create deferred foreign key staging table for %s: %w", name, err)
		}
		sourceDB := sourceForStore(sources, source.Store)
		rows, err := sourceDB.tx.QueryContext(ctx, selectSourceColumns(name, columnNames))
		if err != nil {
			return fmt.Errorf("read deferred foreign keys for %s: %w", name, err)
		}
		copySource := &sqliteCopySource{rows: rows, columns: columns}
		copied, copyErr := tx.CopyFrom(ctx, pgx.Identifier{temporaryName}, columnNames, copySource)
		closeErr := rows.Close()
		if copyErr != nil {
			return fmt.Errorf("stage deferred foreign keys for %s: %w", name, copyErr)
		}
		if err := copySource.Err(); err != nil {
			return fmt.Errorf("stage deferred foreign keys for %s: %w", name, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close deferred foreign key source rows for %s: %w", name, closeErr)
		}
		if copied != source.Rows {
			return fmt.Errorf("stage deferred foreign keys for %s: copied %d rows, snapshot count is %d", name, copied, source.Rows)
		}
		var assignments []string
		for _, columnName := range columnNames[len(source.PrimaryKey):] {
			quoted := quotePostgresIdentifier(columnName)
			assignments = append(assignments, quoted+" = staged."+quoted)
		}
		var matches []string
		for _, columnName := range source.PrimaryKey {
			quoted := quotePostgresIdentifier(columnName)
			matches = append(matches, "target."+quoted+" = staged."+quoted)
		}
		command, err := tx.Exec(ctx, "UPDATE "+qualifiedTable(schema, name)+" AS target SET "+strings.Join(assignments, ", ")+" FROM "+
			quotePostgresIdentifier(temporaryName)+" AS staged WHERE "+strings.Join(matches, " AND "))
		if err != nil {
			return fmt.Errorf("restore deferred foreign keys for %s: %w", name, err)
		}
		if command.RowsAffected() != source.Rows {
			return fmt.Errorf("restore deferred foreign keys for %s: updated %d rows, expected %d", name, command.RowsAffected(), source.Rows)
		}
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func fingerprintSourceTable(ctx context.Context, sourceTx *sql.Tx, source *sourceTable, target *targetTable) (tableFingerprint, int64, error) {
	columns, names := mappedColumns(source, target)
	rows, err := sourceTx.QueryContext(ctx, selectSourceColumns(source.Name, names))
	if err != nil {
		return tableFingerprint{}, 0, err
	}
	defer rows.Close()
	copySource := &sqliteCopySource{rows: rows, columns: columns}
	for copySource.Next() {
		if _, err := copySource.Values(); err != nil {
			return tableFingerprint{}, 0, err
		}
	}
	if err := copySource.Err(); err != nil {
		return tableFingerprint{}, 0, err
	}
	return copySource.fingerprint, copySource.fingerprint.count, nil
}

func mappedColumns(source *sourceTable, target *targetTable) ([]*targetColumn, []string) {
	columns := make([]*targetColumn, 0, len(source.Columns))
	names := make([]string, 0, len(source.Columns))
	for i := range source.Columns {
		columns = append(columns, target.ColumnByName[source.Columns[i].Name])
		names = append(names, source.Columns[i].Name)
	}
	return columns, names
}

func selectSourceColumns(table string, columns []string) string {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteSQLiteIdentifier(column)
	}
	return "SELECT " + strings.Join(quoted, ", ") + " FROM " + quoteSQLiteIdentifier(table)
}

func verifyTargetTables(ctx context.Context, tx pgx.Tx, schema string, mapped map[string]*sourceTable, targets map[string]*targetTable, reports map[string]TableReport) error {
	names := make([]string, 0, len(mapped))
	for name := range mapped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		source := mapped[name]
		columns, columnNames := mappedColumns(source, targets[name])
		quoted := make([]string, len(columnNames))
		for i, column := range columnNames {
			quoted[i] = postgresFingerprintExpression(targets[name].ColumnByName[column])
		}
		rows, err := tx.Query(ctx, "SELECT "+strings.Join(quoted, ", ")+" FROM "+qualifiedTable(schema, name))
		if err != nil {
			return fmt.Errorf("verify postgres table %s: %w", name, err)
		}
		var fingerprint tableFingerprint
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				rows.Close()
				return fmt.Errorf("read postgres table %s row: %w", name, err)
			}
			if err := fingerprint.add(columns, values); err != nil {
				rows.Close()
				return fmt.Errorf("fingerprint postgres table %s: %w", name, err)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate postgres table %s: %w", name, err)
		}
		rows.Close()
		report := reports[name]
		report.TargetRows = fingerprint.count
		if report.SourceRows != report.TargetRows {
			return fmt.Errorf("postgres table %s row count mismatch: sqlite=%d postgres=%d", name, report.SourceRows, report.TargetRows)
		}
		if report.Digest != fingerprint.digest() {
			return fmt.Errorf("postgres table %s content fingerprint mismatch", name)
		}
		reports[name] = report
	}
	return nil
}

func verifyTargetForeignKeys(ctx context.Context, tx pgx.Tx, schema string, mapped map[string]*sourceTable, targets map[string]*targetTable) error {
	for name := range mapped {
		for _, fk := range targets[name].ForeignKeys {
			conditions := make([]string, len(fk.ChildColumns))
			nonNull := make([]string, len(fk.ChildColumns))
			for i := range fk.ChildColumns {
				conditions[i] = "parent." + quotePostgresIdentifier(fk.ParentColumns[i]) + " = child." + quotePostgresIdentifier(fk.ChildColumns[i])
				nonNull[i] = "child." + quotePostgresIdentifier(fk.ChildColumns[i]) + " IS NOT NULL"
			}
			query := "SELECT COUNT(*) FROM " + qualifiedTable(schema, fk.ChildTable) + " child WHERE " +
				strings.Join(nonNull, " AND ") + " AND NOT EXISTS (SELECT 1 FROM " + qualifiedTable(schema, fk.ParentTable) +
				" parent WHERE " + strings.Join(conditions, " AND ") + ")"
			var orphans int64
			if err := tx.QueryRow(ctx, query).Scan(&orphans); err != nil {
				return fmt.Errorf("verify postgres foreign key %s: %w", fk.Name, err)
			}
			if orphans != 0 {
				return fmt.Errorf("postgres foreign key %s has %d orphan rows", fk.Name, orphans)
			}
		}
	}
	return nil
}

type ownedSequence struct {
	Schema    string
	Name      string
	Minimum   int64
	Increment int64
}

func resetCopiedSequences(ctx context.Context, tx pgx.Tx, schema string, mapped map[string]*sourceTable, targets map[string]*targetTable) error {
	for name, source := range mapped {
		highWaterApplied := source.AutoIncrementHighWater == nil
		for _, sourceColumn := range source.Columns {
			column := targets[name].ColumnByName[sourceColumn.Name]
			sequence, err := discoverOwnedSequence(ctx, tx, schema, name, column.Name)
			if err != nil {
				return err
			}
			if sequence == nil {
				continue
			}
			next, usedHighWater, err := expectedSequenceNext(ctx, tx, schema, name, source, column.Name, sequence.Minimum)
			if err != nil {
				return err
			}
			highWaterApplied = highWaterApplied || usedHighWater
			if _, err := tx.Exec(ctx, "ALTER SEQUENCE "+qualifiedTable(sequence.Schema, sequence.Name)+" RESTART WITH "+strconv.FormatInt(next, 10)); err != nil {
				return fmt.Errorf("reset postgres sequence for %s.%s: %w", name, column.Name, err)
			}
		}
		if !highWaterApplied {
			return fmt.Errorf("%s sqlite table %s has AUTOINCREMENT high-water state but its primary key has no owned postgres sequence", source.Store, name)
		}
	}
	return nil
}

func discoverOwnedSequence(ctx context.Context, tx pgx.Tx, schema, table, column string) (*ownedSequence, error) {
	var sequence ownedSequence
	err := tx.QueryRow(ctx, `
		SELECT sequence_ns.nspname, sequence.relname, metadata.seqmin, metadata.seqincrement
		FROM pg_class relation
		JOIN pg_namespace relation_ns ON relation_ns.oid = relation.relnamespace
		JOIN pg_attribute attribute ON attribute.attrelid = relation.oid AND attribute.attname = $3
		JOIN pg_depend dependency ON dependency.refobjid = relation.oid
		  AND dependency.refobjsubid = attribute.attnum
		  AND dependency.classid = 'pg_class'::regclass
		  AND dependency.deptype IN ('a', 'i')
		JOIN pg_class sequence ON sequence.oid = dependency.objid AND sequence.relkind = 'S'
		JOIN pg_namespace sequence_ns ON sequence_ns.oid = sequence.relnamespace
		JOIN pg_sequence metadata ON metadata.seqrelid = sequence.oid
		WHERE relation_ns.nspname = $1 AND relation.relname = $2`, schema, table, column).Scan(
		&sequence.Schema, &sequence.Name, &sequence.Minimum, &sequence.Increment,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("discover postgres sequence for %s.%s: %w", table, column, err)
	}
	if sequence.Increment <= 0 {
		return nil, fmt.Errorf("postgres sequence %s for %s.%s has unsupported increment %d", qualifiedTable(sequence.Schema, sequence.Name), table, column, sequence.Increment)
	}
	return &sequence, nil
}

func expectedSequenceNext(ctx context.Context, tx pgx.Tx, schema, table string, source *sourceTable, column string, minimum int64) (int64, bool, error) {
	var maximum *int64
	if err := tx.QueryRow(ctx, "SELECT MAX("+quotePostgresIdentifier(column)+") FROM "+qualifiedTable(schema, table)).Scan(&maximum); err != nil {
		return 0, false, fmt.Errorf("read postgres sequence maximum for %s.%s: %w", table, column, err)
	}
	var lastUsed *int64
	if maximum != nil {
		value := *maximum
		lastUsed = &value
	}
	usedHighWater := false
	if len(source.PrimaryKey) == 1 && source.PrimaryKey[0] == column && source.AutoIncrementHighWater != nil {
		usedHighWater = true
		if lastUsed == nil || *source.AutoIncrementHighWater > *lastUsed {
			value := *source.AutoIncrementHighWater
			lastUsed = &value
		}
	}
	if lastUsed == nil {
		return minimum, usedHighWater, nil
	}
	if *lastUsed == math.MaxInt64 {
		return 0, usedHighWater, fmt.Errorf("postgres sequence for %s.%s cannot advance past %d", table, column, *lastUsed)
	}
	next := *lastUsed + 1
	if next < minimum {
		next = minimum
	}
	return next, usedHighWater, nil
}

func verifyCopiedSequences(ctx context.Context, tx pgx.Tx, schema string, mapped map[string]*sourceTable) error {
	for name, source := range mapped {
		highWaterVerified := source.AutoIncrementHighWater == nil
		for _, sourceColumn := range source.Columns {
			sequence, err := discoverOwnedSequence(ctx, tx, schema, name, sourceColumn.Name)
			if err != nil {
				return err
			}
			if sequence == nil {
				continue
			}
			expected, usedHighWater, err := expectedSequenceNext(ctx, tx, schema, name, source, sourceColumn.Name, sequence.Minimum)
			if err != nil {
				return err
			}
			highWaterVerified = highWaterVerified || usedHighWater
			var lastValue int64
			var called bool
			if err := tx.QueryRow(ctx, "SELECT last_value, is_called FROM "+qualifiedTable(sequence.Schema, sequence.Name)).Scan(&lastValue, &called); err != nil {
				return fmt.Errorf("read postgres sequence %s for %s.%s: %w", qualifiedTable(sequence.Schema, sequence.Name), name, sourceColumn.Name, err)
			}
			actual := lastValue
			if called {
				if lastValue > math.MaxInt64-sequence.Increment {
					return fmt.Errorf("postgres sequence %s next value overflows int64", qualifiedTable(sequence.Schema, sequence.Name))
				}
				actual += sequence.Increment
			}
			if actual != expected {
				return fmt.Errorf("postgres sequence mismatch for %s.%s: expected next=%d actual next=%d", name, sourceColumn.Name, expected, actual)
			}
		}
		if !highWaterVerified {
			return fmt.Errorf("%s sqlite table %s has AUTOINCREMENT high-water state but its primary key has no owned postgres sequence", source.Store, name)
		}
	}
	return nil
}

func postgresFingerprintExpression(column *targetColumn) string {
	expression := quotePostgresIdentifier(column.Name)
	if column.DataType == "json" || column.DataType == "jsonb" {
		return expression + "::text"
	}
	return expression
}

type migrationRecord struct {
	id        string
	appliedAt any
	status    any
}

func collectMigrationRecords(ctx context.Context, sources []*sourceDatabase, target *targetTable) (map[string]migrationRecord, error) {
	idColumn := target.ColumnByName["id"]
	appliedColumn := target.ColumnByName["applied_at"]
	statusColumn := target.ColumnByName["status"]
	if idColumn == nil || appliedColumn == nil || statusColumn == nil {
		return nil, fmt.Errorf("postgres migrations table must contain id, applied_at, and status columns")
	}
	records := make(map[string]migrationRecord)
	for _, source := range sources {
		table := source.tables[migrationTableName]
		for _, required := range []string{"id", "applied_at", "status"} {
			found := false
			for _, column := range table.Columns {
				if column.Name == required {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("%s sqlite migrations table is missing %s; run the same Bifrost release against SQLite before migration", source.store, required)
			}
		}
		rows, err := source.tx.QueryContext(ctx, `SELECT id, applied_at, status FROM migrations ORDER BY id`)
		if err != nil {
			return nil, fmt.Errorf("read %s sqlite migration ledger: %w", source.store, err)
		}
		for rows.Next() {
			var idRaw, appliedRaw, statusRaw any
			if err := rows.Scan(&idRaw, &appliedRaw, &statusRaw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s sqlite migration ledger: %w", source.store, err)
			}
			id := stringValue(idRaw)
			appliedAt, err := convertValue(appliedColumn, appliedRaw)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("convert %s migration %s applied_at: %w", source.store, id, err)
			}
			status, err := convertValue(statusColumn, statusRaw)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("convert %s migration %s status: %w", source.store, id, err)
			}
			record := migrationRecord{id: id, appliedAt: appliedAt, status: status}
			if prior, exists := records[id]; exists {
				priorTime, _ := canonicalValue(appliedColumn, prior.appliedAt)
				currentTime, _ := canonicalValue(appliedColumn, appliedAt)
				if string(priorTime) != string(currentTime) || stringValue(prior.status) != stringValue(status) {
					rows.Close()
					return nil, fmt.Errorf("migration id %s has conflicting metadata in config and logs sqlite stores", id)
				}
				continue
			}
			records[id] = record
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate %s sqlite migration ledger: %w", source.store, err)
		}
		rows.Close()
	}
	return records, nil
}

func reconcileMigrationLedger(ctx context.Context, tx pgx.Tx, schema string, sources []*sourceDatabase, target *targetTable) (TableReport, error) {
	records, err := collectMigrationRecords(ctx, sources, target)
	if err != nil {
		return TableReport{}, err
	}
	for id, record := range records {
		command, err := tx.Exec(ctx, "UPDATE "+qualifiedTable(schema, migrationTableName)+" SET applied_at = $2, status = $3 WHERE id = $1", id, record.appliedAt, record.status)
		if err != nil {
			return TableReport{}, fmt.Errorf("reconcile postgres migration %s: %w", id, err)
		}
		if command.RowsAffected() != 1 {
			return TableReport{}, fmt.Errorf("postgres schema is missing sqlite migration id %s; initialize it with the same Bifrost release", id)
		}
	}
	return verifyMigrationRecords(ctx, tx, schema, records, target)
}

func verifyMigrationLedger(ctx context.Context, tx pgx.Tx, schema string, sources []*sourceDatabase, target *targetTable) (TableReport, error) {
	records, err := collectMigrationRecords(ctx, sources, target)
	if err != nil {
		return TableReport{}, err
	}
	return verifyMigrationRecords(ctx, tx, schema, records, target)
}

func verifyMigrationRecords(ctx context.Context, tx pgx.Tx, schema string, records map[string]migrationRecord, target *targetTable) (TableReport, error) {
	appliedColumn := target.ColumnByName["applied_at"]
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var appliedAt, status any
		if err := tx.QueryRow(ctx, "SELECT applied_at, status FROM "+qualifiedTable(schema, migrationTableName)+" WHERE id = $1", id).Scan(&appliedAt, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return TableReport{}, fmt.Errorf("postgres schema is missing sqlite migration id %s", id)
			}
			return TableReport{}, fmt.Errorf("verify postgres migration %s: %w", id, err)
		}
		expectedTime, err := canonicalValue(appliedColumn, records[id].appliedAt)
		if err != nil {
			return TableReport{}, err
		}
		actualTime, err := canonicalValue(appliedColumn, appliedAt)
		if err != nil {
			return TableReport{}, err
		}
		if string(expectedTime) != string(actualTime) || stringValue(records[id].status) != stringValue(status) {
			return TableReport{}, fmt.Errorf("postgres migration %s metadata does not match sqlite", id)
		}
	}
	var targetCount int64
	if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+qualifiedTable(schema, migrationTableName)).Scan(&targetCount); err != nil {
		return TableReport{}, fmt.Errorf("count postgres migrations: %w", err)
	}
	return TableReport{Store: "config+logs", Table: migrationTableName, SourceRows: int64(len(records)), TargetRows: targetCount, Digest: "metadata-verified"}, nil
}

func buildReport(snapshots Snapshots, reports map[string]TableReport) Report {
	names := make([]string, 0, len(reports))
	for name := range reports {
		names = append(names, name)
	}
	sort.Strings(names)
	result := Report{ConfigSHA256: snapshots.ConfigSHA256, LogsSHA256: snapshots.LogsSHA256}
	for _, name := range names {
		result.Tables = append(result.Tables, reports[name])
	}
	return result
}
