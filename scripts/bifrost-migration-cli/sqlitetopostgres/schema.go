package sqlitetopostgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

const migrationTableName = "migrations"

type sourceColumn struct {
	Name         string
	DeclaredType string
	NotNull      bool
	PrimaryKey   int
	Default      *string
}

type targetColumn struct {
	Name      string
	DataType  string
	UDTName   string
	Nullable  bool
	Default   *string
	Identity  bool
	Generated bool
	Source    *sourceColumn
}

type foreignKey struct {
	Name          string
	ChildTable    string
	ParentTable   string
	ChildColumns  []string
	ParentColumns []string
	OnUpdate      string
	OnDelete      string
	Match         string
	Validated     bool
	Deferrable    bool
}

type sourceTable struct {
	Store                  string
	Name                   string
	Columns                []sourceColumn
	PrimaryKey             []string
	ForeignKeys            []foreignKey
	UniqueColumnSets       [][]string
	UniquePredicates       []string
	CheckExpressions       []string
	AutoIncrementHighWater *int64
	Rows                   int64
}

type targetTable struct {
	Name             string
	Columns          []targetColumn
	ColumnByName     map[string]*targetColumn
	PrimaryKey       []string
	ForeignKeys      []foreignKey
	UniqueColumnSets [][]string
	UniquePredicates []string
	CheckExpressions []string
}

type sourceDatabase struct {
	store  string
	db     *sql.DB
	tx     *sql.Tx
	tables map[string]*sourceTable
}

func openSourceDatabase(ctx context.Context, store, path string) (*sourceDatabase, error) {
	db, err := openSQLite(path, true)
	if err != nil {
		return nil, fmt.Errorf("open %s snapshot: %w", store, err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelSerializable})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("begin %s snapshot transaction: %w", store, err)
	}
	source := &sourceDatabase{store: store, db: db, tx: tx}
	if err := checkSQLite(ctx, tx); err != nil {
		source.close()
		return nil, fmt.Errorf("validate %s snapshot: %w", store, err)
	}
	if source.tables, err = discoverSourceTables(ctx, tx, store); err != nil {
		source.close()
		return nil, err
	}
	if err := discoverSourceSequenceHighWater(ctx, tx, source.tables); err != nil {
		source.close()
		return nil, fmt.Errorf("discover %s sqlite sequences: %w", store, err)
	}
	return source, nil
}

func (s *sourceDatabase) close() {
	if s.tx != nil {
		_ = s.tx.Rollback()
	}
	if s.db != nil {
		_ = s.db.Close()
	}
}

func discoverSourceTables(ctx context.Context, tx *sql.Tx, store string) (map[string]*sourceTable, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT name
		FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("discover %s sqlite tables: %w", store, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read %s sqlite table: %w", store, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s sqlite tables: %w", store, err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s sqlite snapshot has no user tables", store)
	}

	tables := make(map[string]*sourceTable, len(names))
	for _, name := range names {
		table, err := discoverSourceTable(ctx, tx, store, name)
		if err != nil {
			return nil, err
		}
		tables[name] = table
	}
	for _, table := range tables {
		for i := range table.ForeignKeys {
			fk := &table.ForeignKeys[i]
			parent, ok := tables[fk.ParentTable]
			if !ok {
				return nil, fmt.Errorf("%s sqlite table %s references missing table %s", store, table.Name, fk.ParentTable)
			}
			for j := range fk.ParentColumns {
				if fk.ParentColumns[j] == "" {
					if len(parent.PrimaryKey) != len(fk.ParentColumns) {
						return nil, fmt.Errorf("%s sqlite foreign key %s has implicit parent columns but %s primary key has %d columns", store, fk.Name, parent.Name, len(parent.PrimaryKey))
					}
					fk.ParentColumns[j] = parent.PrimaryKey[j]
				}
			}
		}
	}
	return tables, nil
}

func discoverSourceTable(ctx context.Context, tx *sql.Tx, store, name string) (*sourceTable, error) {
	table := &sourceTable{Store: store, Name: name}
	columns, err := tx.QueryContext(ctx, "PRAGMA table_xinfo("+quoteSQLiteIdentifier(name)+")")
	if err != nil {
		return nil, fmt.Errorf("discover %s sqlite table %s columns: %w", store, name, err)
	}
	for columns.Next() {
		var cid, notNull, primaryKey, hidden int
		var columnName, declaredType string
		var defaultValue sql.NullString
		if err := columns.Scan(&cid, &columnName, &declaredType, &notNull, &defaultValue, &primaryKey, &hidden); err != nil {
			columns.Close()
			return nil, fmt.Errorf("read %s sqlite table %s column: %w", store, name, err)
		}
		if hidden != 0 {
			continue
		}
		column := sourceColumn{
			Name: columnName, DeclaredType: declaredType, NotNull: notNull != 0, PrimaryKey: primaryKey,
		}
		if defaultValue.Valid {
			value := defaultValue.String
			column.Default = &value
		}
		table.Columns = append(table.Columns, column)
	}
	if err := columns.Close(); err != nil {
		return nil, fmt.Errorf("close %s sqlite table %s columns: %w", store, name, err)
	}
	if len(table.Columns) == 0 {
		return nil, fmt.Errorf("%s sqlite table %s has no copyable columns", store, name)
	}

	pk := append([]sourceColumn(nil), table.Columns...)
	sort.Slice(pk, func(i, j int) bool {
		if pk[i].PrimaryKey == 0 {
			return false
		}
		if pk[j].PrimaryKey == 0 {
			return true
		}
		return pk[i].PrimaryKey < pk[j].PrimaryKey
	})
	for _, column := range pk {
		if column.PrimaryKey > 0 {
			table.PrimaryKey = append(table.PrimaryKey, column.Name)
		}
	}

	fkRows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_list("+quoteSQLiteIdentifier(name)+")")
	if err != nil {
		return nil, fmt.Errorf("discover %s sqlite table %s foreign keys: %w", store, name, err)
	}
	type fkPart struct {
		sequence int
		from     string
		to       string
	}
	type fkGroup struct {
		parent   string
		onUpdate string
		onDelete string
		match    string
		parts    []fkPart
	}
	groups := make(map[int]*fkGroup)
	for fkRows.Next() {
		var id, sequence int
		var parent, from, to, onUpdate, onDelete, match string
		if err := fkRows.Scan(&id, &sequence, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			fkRows.Close()
			return nil, fmt.Errorf("read %s sqlite table %s foreign key: %w", store, name, err)
		}
		group := groups[id]
		if group == nil {
			group = &fkGroup{parent: parent, onUpdate: onUpdate, onDelete: onDelete, match: match}
			groups[id] = group
		}
		group.parts = append(group.parts, fkPart{sequence: sequence, from: from, to: to})
	}
	if err := fkRows.Close(); err != nil {
		return nil, fmt.Errorf("close %s sqlite table %s foreign keys: %w", store, name, err)
	}
	ids := make([]int, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		group := groups[id]
		sort.Slice(group.parts, func(i, j int) bool { return group.parts[i].sequence < group.parts[j].sequence })
		fk := foreignKey{
			Name: fmt.Sprintf("sqlite:%s:%d", name, id), ChildTable: name, ParentTable: group.parent,
			OnUpdate: group.onUpdate, OnDelete: group.onDelete, Match: group.match, Validated: true,
		}
		for _, part := range group.parts {
			fk.ChildColumns = append(fk.ChildColumns, part.from)
			fk.ParentColumns = append(fk.ParentColumns, part.to)
		}
		table.ForeignKeys = append(table.ForeignKeys, fk)
	}

	if err := discoverSourceUniqueIndexes(ctx, tx, table); err != nil {
		return nil, err
	}
	var createSQL sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = ?`, name).Scan(&createSQL); err != nil {
		return nil, fmt.Errorf("read %s sqlite table %s definition: %w", store, name, err)
	}
	if createSQL.Valid {
		table.CheckExpressions = extractSQLiteChecks(createSQL.String)
	}

	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteSQLiteIdentifier(name)).Scan(&table.Rows); err != nil {
		return nil, fmt.Errorf("count %s sqlite table %s: %w", store, name, err)
	}
	return table, nil
}

func discoverSourceUniqueIndexes(ctx context.Context, tx *sql.Tx, table *sourceTable) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA index_list("+quoteSQLiteIdentifier(table.Name)+")")
	if err != nil {
		return fmt.Errorf("discover %s sqlite table %s indexes: %w", table.Store, table.Name, err)
	}
	type uniqueIndex struct {
		name    string
		partial bool
	}
	var indexes []uniqueIndex
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return fmt.Errorf("read %s sqlite table %s index: %w", table.Store, table.Name, err)
		}
		if unique == 0 || origin == "pk" {
			continue
		}
		indexes = append(indexes, uniqueIndex{name: name, partial: partial != 0})
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close %s sqlite table %s indexes: %w", table.Store, table.Name, err)
	}
	for _, index := range indexes {
		columnRows, err := tx.QueryContext(ctx, "PRAGMA index_info("+quoteSQLiteIdentifier(index.name)+")")
		if err != nil {
			return fmt.Errorf("discover %s sqlite unique index %s columns: %w", table.Store, index.name, err)
		}
		var columns []string
		for columnRows.Next() {
			var sequence, cid int
			var name sql.NullString
			if err := columnRows.Scan(&sequence, &cid, &name); err != nil {
				columnRows.Close()
				return fmt.Errorf("read %s sqlite unique index %s column: %w", table.Store, index.name, err)
			}
			if cid < 0 || !name.Valid {
				columnRows.Close()
				return fmt.Errorf("%s sqlite table %s has unsupported expression unique index %s", table.Store, table.Name, index.name)
			}
			columns = append(columns, name.String)
		}
		if err := columnRows.Close(); err != nil {
			return fmt.Errorf("close %s sqlite unique index %s columns: %w", table.Store, index.name, err)
		}
		if len(columns) == 0 {
			return fmt.Errorf("%s sqlite unique index %s has no columns", table.Store, index.name)
		}
		table.UniqueColumnSets = append(table.UniqueColumnSets, columns)
		predicate := ""
		if index.partial {
			var definition sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = ?`, index.name).Scan(&definition); err != nil {
				return fmt.Errorf("read %s sqlite partial unique index %s definition: %w", table.Store, index.name, err)
			}
			if !definition.Valid {
				return fmt.Errorf("%s sqlite partial unique index %s has no SQL definition", table.Store, index.name)
			}
			where := wherePattern.FindStringIndex(definition.String)
			if where == nil {
				return fmt.Errorf("%s sqlite partial unique index %s has no WHERE predicate", table.Store, index.name)
			}
			predicate = strings.TrimSpace(definition.String[where[1]:])
		}
		table.UniquePredicates = append(table.UniquePredicates, predicate)
	}
	return nil
}

func discoverSourceSequenceHighWater(ctx context.Context, tx *sql.Tx, tables map[string]*sourceTable) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'sqlite_sequence')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var sequence int64
		if err := rows.Scan(&name, &sequence); err != nil {
			return err
		}
		table := tables[name]
		if table == nil {
			return fmt.Errorf("sqlite_sequence references missing table %s", name)
		}
		if sequence < 0 {
			return fmt.Errorf("sqlite_sequence for table %s is negative: %d", name, sequence)
		}
		value := sequence
		table.AutoIncrementHighWater = &value
	}
	return rows.Err()
}

func discoverTargetTables(ctx context.Context, tx pgx.Tx, schema string) (map[string]*targetTable, error) {
	rows, err := tx.Query(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = $1 AND table_type = 'BASE TABLE'
		ORDER BY table_name`, schema)
	if err != nil {
		return nil, fmt.Errorf("discover postgres tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read postgres table: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate postgres tables: %w", err)
	}
	rows.Close()
	if len(names) == 0 {
		return nil, fmt.Errorf("postgres schema %s has no tables; initialize it with the same Bifrost release before migration", schema)
	}

	tables := make(map[string]*targetTable, len(names))
	for _, name := range names {
		table := &targetTable{Name: name, ColumnByName: make(map[string]*targetColumn)}
		columnRows, err := tx.Query(ctx, `
			SELECT column_name, data_type, udt_name, is_nullable = 'YES', column_default,
			       is_identity = 'YES', is_generated <> 'NEVER'
			FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2
			ORDER BY ordinal_position`, schema, name)
		if err != nil {
			return nil, fmt.Errorf("discover postgres table %s columns: %w", name, err)
		}
		for columnRows.Next() {
			var column targetColumn
			if err := columnRows.Scan(&column.Name, &column.DataType, &column.UDTName, &column.Nullable, &column.Default, &column.Identity, &column.Generated); err != nil {
				columnRows.Close()
				return nil, fmt.Errorf("read postgres table %s column: %w", name, err)
			}
			table.Columns = append(table.Columns, column)
		}
		if err := columnRows.Err(); err != nil {
			columnRows.Close()
			return nil, fmt.Errorf("iterate postgres table %s columns: %w", name, err)
		}
		columnRows.Close()
		for i := range table.Columns {
			table.ColumnByName[table.Columns[i].Name] = &table.Columns[i]
		}
		tables[name] = table
	}

	pkRows, err := tx.Query(ctx, `
		SELECT rel.relname, att.attname
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace ns ON ns.oid = rel.relnamespace
		JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS key(attnum, ord) ON true
		JOIN pg_attribute att ON att.attrelid = rel.oid AND att.attnum = key.attnum
		WHERE ns.nspname = $1 AND con.contype = 'p'
		ORDER BY rel.relname, key.ord`, schema)
	if err != nil {
		return nil, fmt.Errorf("discover postgres primary keys: %w", err)
	}
	for pkRows.Next() {
		var tableName, columnName string
		if err := pkRows.Scan(&tableName, &columnName); err != nil {
			pkRows.Close()
			return nil, fmt.Errorf("read postgres primary key: %w", err)
		}
		tables[tableName].PrimaryKey = append(tables[tableName].PrimaryKey, columnName)
	}
	if err := pkRows.Err(); err != nil {
		pkRows.Close()
		return nil, fmt.Errorf("iterate postgres primary keys: %w", err)
	}
	pkRows.Close()

	fkRows, err := tx.Query(ctx, `
		SELECT con.conname, child.relname, parent.relname,
		       array_agg(child_att.attname ORDER BY keys.ord),
		       array_agg(parent_att.attname ORDER BY keys.ord),
		       CASE con.confupdtype
		         WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT' WHEN 'c' THEN 'CASCADE'
		         WHEN 'n' THEN 'SET NULL' WHEN 'd' THEN 'SET DEFAULT' END,
		       CASE con.confdeltype
		         WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT' WHEN 'c' THEN 'CASCADE'
		         WHEN 'n' THEN 'SET NULL' WHEN 'd' THEN 'SET DEFAULT' END,
		       CASE con.confmatchtype
		         WHEN 's' THEN 'NONE' WHEN 'f' THEN 'FULL' WHEN 'p' THEN 'PARTIAL' END,
		       con.convalidated, con.condeferrable
		FROM pg_constraint con
		JOIN pg_class child ON child.oid = con.conrelid
		JOIN pg_namespace ns ON ns.oid = child.relnamespace
		JOIN pg_class parent ON parent.oid = con.confrelid
		JOIN LATERAL unnest(con.conkey, con.confkey) WITH ORDINALITY AS keys(child_num, parent_num, ord) ON true
		JOIN pg_attribute child_att ON child_att.attrelid = child.oid AND child_att.attnum = keys.child_num
		JOIN pg_attribute parent_att ON parent_att.attrelid = parent.oid AND parent_att.attnum = keys.parent_num
		WHERE ns.nspname = $1 AND con.contype = 'f'
		GROUP BY con.conname, child.relname, parent.relname, con.confupdtype, con.confdeltype,
		         con.confmatchtype, con.convalidated, con.condeferrable
		ORDER BY child.relname, con.conname`, schema)
	if err != nil {
		return nil, fmt.Errorf("discover postgres foreign keys: %w", err)
	}
	for fkRows.Next() {
		var fk foreignKey
		if err := fkRows.Scan(
			&fk.Name, &fk.ChildTable, &fk.ParentTable, &fk.ChildColumns, &fk.ParentColumns,
			&fk.OnUpdate, &fk.OnDelete, &fk.Match, &fk.Validated, &fk.Deferrable,
		); err != nil {
			fkRows.Close()
			return nil, fmt.Errorf("read postgres foreign key: %w", err)
		}
		if table := tables[fk.ChildTable]; table != nil {
			table.ForeignKeys = append(table.ForeignKeys, fk)
		}
	}
	if err := fkRows.Err(); err != nil {
		fkRows.Close()
		return nil, fmt.Errorf("iterate postgres foreign keys: %w", err)
	}
	fkRows.Close()

	uniqueRows, err := tx.Query(ctx, `
		SELECT rel.relname, idx.relname,
		       array_agg(att.attname ORDER BY key.ord),
		       pg_get_expr(ind.indpred, ind.indrelid), ind.indexprs IS NOT NULL, ind.indnullsnotdistinct
		FROM pg_index ind
		JOIN pg_class rel ON rel.oid = ind.indrelid
		JOIN pg_namespace ns ON ns.oid = rel.relnamespace
		JOIN pg_class idx ON idx.oid = ind.indexrelid
		JOIN LATERAL unnest(ind.indkey) WITH ORDINALITY AS key(attnum, ord)
		  ON key.ord <= ind.indnkeyatts
		LEFT JOIN pg_attribute att ON att.attrelid = rel.oid AND att.attnum = key.attnum
		WHERE ns.nspname = $1 AND ind.indisunique AND NOT ind.indisprimary
		GROUP BY rel.relname, idx.relname, ind.indrelid, ind.indpred, ind.indexprs, ind.indnullsnotdistinct
		ORDER BY rel.relname, idx.relname`, schema)
	if err != nil {
		return nil, fmt.Errorf("discover postgres unique indexes: %w", err)
	}
	for uniqueRows.Next() {
		var tableName, indexName string
		var columns []*string
		var predicate *string
		var expression, nullsNotDistinct bool
		if err := uniqueRows.Scan(&tableName, &indexName, &columns, &predicate, &expression, &nullsNotDistinct); err != nil {
			uniqueRows.Close()
			return nil, fmt.Errorf("read postgres unique index: %w", err)
		}
		if expression || nullsNotDistinct {
			uniqueRows.Close()
			return nil, fmt.Errorf("postgres table %s has unsupported unique index %s (expression=%t nulls_not_distinct=%t)", tableName, indexName, expression, nullsNotDistinct)
		}
		set := make([]string, len(columns))
		for i, column := range columns {
			if column == nil {
				uniqueRows.Close()
				return nil, fmt.Errorf("postgres unique index %s on %s contains an expression", indexName, tableName)
			}
			set[i] = *column
		}
		if table := tables[tableName]; table != nil {
			table.UniqueColumnSets = append(table.UniqueColumnSets, set)
			if predicate == nil {
				table.UniquePredicates = append(table.UniquePredicates, "")
			} else {
				table.UniquePredicates = append(table.UniquePredicates, *predicate)
			}
		}
	}
	if err := uniqueRows.Err(); err != nil {
		uniqueRows.Close()
		return nil, fmt.Errorf("iterate postgres unique indexes: %w", err)
	}
	uniqueRows.Close()

	checkRows, err := tx.Query(ctx, `
		SELECT rel.relname, con.conname, pg_get_expr(con.conbin, con.conrelid), con.convalidated
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace ns ON ns.oid = rel.relnamespace
		WHERE ns.nspname = $1 AND con.contype = 'c'
		ORDER BY rel.relname, con.conname`, schema)
	if err != nil {
		return nil, fmt.Errorf("discover postgres check constraints: %w", err)
	}
	for checkRows.Next() {
		var tableName, constraintName, expression string
		var validated bool
		if err := checkRows.Scan(&tableName, &constraintName, &expression, &validated); err != nil {
			checkRows.Close()
			return nil, fmt.Errorf("read postgres check constraint: %w", err)
		}
		if !validated {
			checkRows.Close()
			return nil, fmt.Errorf("postgres check constraint %s on %s is not validated", constraintName, tableName)
		}
		if table := tables[tableName]; table != nil {
			table.CheckExpressions = append(table.CheckExpressions, expression)
		}
	}
	if err := checkRows.Err(); err != nil {
		checkRows.Close()
		return nil, fmt.Errorf("iterate postgres check constraints: %w", err)
	}
	checkRows.Close()
	return tables, nil
}

func mapSourceTables(sources []*sourceDatabase, targets map[string]*targetTable) (map[string]*sourceTable, error) {
	combined := make(map[string]*sourceTable)
	for _, source := range sources {
		for name, table := range source.tables {
			if name == migrationTableName {
				continue
			}
			if prior, exists := combined[name]; exists {
				return nil, fmt.Errorf("sqlite table %s exists in both %s and %s stores", name, prior.Store, table.Store)
			}
			target := targets[name]
			if target == nil {
				return nil, fmt.Errorf("%s sqlite table %s is absent from the postgres schema", table.Store, name)
			}
			if err := removeRedundantSQLiteColumns(source.tx, table, target); err != nil {
				return nil, err
			}
			for i := range table.Columns {
				sourceColumn := &table.Columns[i]
				targetColumn := target.ColumnByName[sourceColumn.Name]
				if targetColumn == nil {
					return nil, fmt.Errorf("%s sqlite column %s.%s is absent from the postgres schema", table.Store, name, sourceColumn.Name)
				}
				targetColumn.Source = sourceColumn
			}
			for i := range target.Columns {
				column := &target.Columns[i]
				if column.Source != nil || column.Nullable || column.Default != nil || column.Identity || column.Generated {
					continue
				}
				return nil, fmt.Errorf("postgres column %s.%s is required but absent from the %s sqlite source", name, column.Name, table.Store)
			}
			if !equalStrings(table.PrimaryKey, target.PrimaryKey) {
				return nil, fmt.Errorf("primary key mismatch for %s table %s: sqlite=(%s) postgres=(%s)",
					table.Store, name, strings.Join(table.PrimaryKey, ","), strings.Join(target.PrimaryKey, ","))
			}
			if err := validateTableContract(table, target); err != nil {
				return nil, err
			}
			combined[name] = table
		}
	}
	if len(combined) == 0 {
		return nil, fmt.Errorf("sqlite snapshots contain no business tables")
	}
	if targets[migrationTableName] == nil {
		return nil, fmt.Errorf("postgres schema is missing the %s table", migrationTableName)
	}
	for _, source := range sources {
		migrationTable := source.tables[migrationTableName]
		if migrationTable == nil {
			return nil, fmt.Errorf("%s sqlite snapshot is missing the %s table; run the same Bifrost release against SQLite before migration", source.store, migrationTableName)
		}
		if err := validateTableContract(migrationTable, targets[migrationTableName]); err != nil {
			return nil, err
		}
	}
	if err := verifyForeignKeyShapes(combined, targets); err != nil {
		return nil, err
	}
	return combined, nil
}

func removeRedundantSQLiteColumns(tx *sql.Tx, source *sourceTable, target *targetTable) error {
	for i := 0; i < len(source.Columns); i++ {
		column := source.Columns[i]
		if target.ColumnByName[column.Name] != nil {
			continue
		}
		if source.Name != "config_client" || column.Name != "enable_litellm_fallbacks" || target.ColumnByName["compat_convert_text_to_chat"] == nil {
			continue
		}
		var mismatches int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM config_client WHERE enable_litellm_fallbacks IS NOT compat_convert_text_to_chat`).Scan(&mismatches); err != nil {
			return fmt.Errorf("validate redundant config_client.enable_litellm_fallbacks: %w", err)
		}
		if mismatches != 0 {
			return fmt.Errorf("config sqlite column config_client.enable_litellm_fallbacks is absent from postgres and differs from compat_convert_text_to_chat in %d rows", mismatches)
		}
		source.Columns = append(source.Columns[:i], source.Columns[i+1:]...)
		i--
	}
	return nil
}

func verifyForeignKeyShapes(sources map[string]*sourceTable, targets map[string]*targetTable) error {
	for _, source := range sources {
		matchedTargets := make(map[string]struct{})
		for _, sourceFK := range source.ForeignKeys {
			matched := false
			for _, targetFK := range targets[source.Name].ForeignKeys {
				if targetFK.ParentTable == sourceFK.ParentTable &&
					equalStrings(targetFK.ChildColumns, sourceFK.ChildColumns) &&
					equalStrings(targetFK.ParentColumns, sourceFK.ParentColumns) &&
					targetFK.OnUpdate == sourceFK.OnUpdate && targetFK.OnDelete == sourceFK.OnDelete &&
					targetFK.Match == sourceFK.Match {
					matched = true
					matchedTargets[targetFK.Name] = struct{}{}
					if !targetFK.Validated {
						return fmt.Errorf("postgres foreign key %s on %s is not validated", targetFK.Name, source.Name)
					}
					break
				}
			}
			if !matched {
				return fmt.Errorf("postgres schema is missing sqlite relationship %s(%s) -> %s(%s) ON UPDATE %s ON DELETE %s MATCH %s",
					source.Name, strings.Join(sourceFK.ChildColumns, ","), sourceFK.ParentTable,
					strings.Join(sourceFK.ParentColumns, ","), sourceFK.OnUpdate, sourceFK.OnDelete, sourceFK.Match)
			}
		}
		for _, targetFK := range targets[source.Name].ForeignKeys {
			if _, matched := matchedTargets[targetFK.Name]; !matched {
				return fmt.Errorf("postgres schema has relationship absent from %s sqlite: %s(%s) -> %s(%s) ON UPDATE %s ON DELETE %s MATCH %s",
					source.Store, source.Name, strings.Join(targetFK.ChildColumns, ","), targetFK.ParentTable,
					strings.Join(targetFK.ParentColumns, ","), targetFK.OnUpdate, targetFK.OnDelete, targetFK.Match)
			}
		}
	}
	return nil
}

func validateTableContract(source *sourceTable, target *targetTable) error {
	for i := range source.Columns {
		column := &source.Columns[i]
		targetColumn := target.ColumnByName[column.Name]
		if targetColumn == nil {
			continue
		}
		if targetColumn.Generated {
			return fmt.Errorf("postgres column %s.%s is generated but exists in the %s sqlite source", source.Name, column.Name, source.Store)
		}
		if !compatibleColumnType(column.DeclaredType, targetColumn.DataType) {
			return fmt.Errorf("column type mismatch for %s table %s.%s: sqlite=%q postgres=%q", source.Store, source.Name, column.Name, column.DeclaredType, targetColumn.DataType)
		}
		sourceNotNull := column.NotNull || column.PrimaryKey > 0
		if sourceNotNull == targetColumn.Nullable {
			return fmt.Errorf("column nullability mismatch for %s table %s.%s: sqlite_not_null=%t postgres_nullable=%t", source.Store, source.Name, column.Name, sourceNotNull, targetColumn.Nullable)
		}
		if !compatibleDefault(source.Name, column, targetColumn) {
			return fmt.Errorf("column default mismatch for %s table %s.%s: sqlite=%s postgres=%s", source.Store, source.Name, column.Name, printableDefault(column.Default), printableDefault(targetColumn.Default))
		}
	}
	if !equalUniqueDefinitions(source.UniqueColumnSets, source.UniquePredicates, target.UniqueColumnSets, target.UniquePredicates) {
		return fmt.Errorf("unique constraint mismatch for %s table %s: sqlite=%v postgres=%v", source.Store, source.Name, canonicalUniqueDefinitions(source.UniqueColumnSets, source.UniquePredicates), canonicalUniqueDefinitions(target.UniqueColumnSets, target.UniquePredicates))
	}
	if !equalChecks(source.CheckExpressions, target.CheckExpressions) {
		return fmt.Errorf("check constraint mismatch for %s table %s: sqlite=%v postgres=%v", source.Store, source.Name, canonicalChecks(source.CheckExpressions), canonicalChecks(target.CheckExpressions))
	}
	return nil
}

func compatibleColumnType(sqliteType, postgresType string) bool {
	source := strings.ToLower(strings.TrimSpace(sqliteType))
	target := strings.ToLower(strings.TrimSpace(postgresType))
	switch {
	case strings.Contains(source, "bool"):
		return target == "boolean"
	case strings.Contains(source, "datetime") || strings.Contains(source, "timestamp"):
		return target == "timestamp with time zone" || target == "timestamp without time zone"
	case strings.TrimSpace(source) == "date":
		return target == "date"
	case strings.Contains(source, "int"):
		return target == "smallint" || target == "integer" || target == "bigint"
	case strings.Contains(source, "char") || strings.Contains(source, "clob") || strings.Contains(source, "text") || strings.Contains(source, "json"):
		return target == "character" || target == "character varying" || target == "text" ||
			target == "json" || target == "jsonb" || target == "array" || target == "user-defined"
	case source == "" || strings.Contains(source, "blob") || strings.Contains(source, "bytea"):
		return target == "bytea"
	case strings.Contains(source, "real") || strings.Contains(source, "floa") || strings.Contains(source, "doub"):
		return target == "real" || target == "double precision" || target == "numeric" || target == "decimal"
	default:
		return target == "boolean" || target == "smallint" || target == "integer" || target == "bigint" ||
			target == "real" || target == "double precision" || target == "numeric" || target == "decimal"
	}
}

func compatibleDefault(table string, source *sourceColumn, target *targetColumn) bool {
	if target.Identity || isSequenceDefault(target.Default) {
		integerSource := strings.Contains(strings.ToLower(source.DeclaredType), "int") && source.Default == nil
		return integerSource && (source.PrimaryKey > 0 || table == "logs" && source.Name == "inc_number")
	}
	sourceAbsent := source.Default == nil || normalizeSQLExpression(*source.Default) == "null"
	targetAbsent := target.Default == nil || normalizeSQLExpression(*target.Default) == "null"
	if sourceAbsent || targetAbsent {
		return sourceAbsent && targetAbsent
	}
	return normalizeSQLExpression(*source.Default) == normalizeSQLExpression(*target.Default)
}

func isSequenceDefault(value *string) bool {
	return value != nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(*value)), "nextval(")
}

func printableDefault(value *string) string {
	if value == nil {
		return "<none>"
	}
	return fmt.Sprintf("%q", *value)
}

var postgresCastPattern = regexp.MustCompile(`(?i)::(?:character varying|timestamp with(?:out)? time zone|double precision|[a-z_][a-z0-9_]*)(?:\[\])?`)
var wherePattern = regexp.MustCompile(`(?i)\bwhere\b`)

func normalizeSQLExpression(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = postgresCastPattern.ReplaceAllString(value, "")
	for len(value) >= 2 && value[0] == '(' && value[len(value)-1] == ')' && balancedParentheses(value[1:len(value)-1]) {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	value = strings.Join(strings.Fields(value), " ")
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		literal := strings.ReplaceAll(value[1:len(value)-1], `""`, `"`)
		value = "'" + strings.ReplaceAll(literal, "'", "''") + "'"
	}
	switch value {
	case "true":
		return "1"
	case "false":
		return "0"
	default:
		return value
	}
}

func balancedParentheses(value string) bool {
	depth := 0
	quote := byte(0)
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if quote != 0 {
			if ch == quote {
				if i+1 < len(value) && value[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && quote == 0
}

func canonicalUniqueDefinitions(sets [][]string, predicates []string) []string {
	columns := canonicalColumnSetsWithOrder(sets)
	result := make([]string, len(columns))
	for i := range columns {
		predicate := ""
		if i < len(predicates) {
			predicate = normalizePredicate(predicates[i])
		}
		result[i] = columns[i] + "|" + predicate
	}
	sort.Strings(result)
	return result
}

func canonicalColumnSetsWithOrder(sets [][]string) []string {
	result := make([]string, len(sets))
	for i, set := range sets {
		columns := append([]string(nil), set...)
		sort.Strings(columns)
		result[i] = strings.Join(columns, ",")
	}
	return result
}

func equalUniqueDefinitions(a [][]string, aPredicates []string, b [][]string, bPredicates []string) bool {
	return equalStrings(canonicalUniqueDefinitions(a, aPredicates), canonicalUniqueDefinitions(b, bPredicates))
}

func normalizePredicate(value string) string {
	value = normalizeSQLExpression(value)
	var result strings.Builder
	quote := byte(0)
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if quote != 0 {
			result.WriteByte(ch)
			if ch == quote {
				if i+1 < len(value) && value[i+1] == quote {
					result.WriteByte(value[i+1])
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			result.WriteByte(ch)
			continue
		}
		if ch == '!' && i+1 < len(value) && value[i+1] == '=' {
			result.WriteString("<>")
			i++
			continue
		}
		if ch != '(' && ch != ')' {
			result.WriteByte(ch)
		}
	}
	return strings.Join(strings.Fields(result.String()), " ")
}

func canonicalChecks(checks []string) []string {
	result := make([]string, len(checks))
	for i, check := range checks {
		result[i] = normalizeSQLExpression(check)
	}
	sort.Strings(result)
	return result
}

func equalChecks(a, b []string) bool {
	return equalStrings(canonicalChecks(a), canonicalChecks(b))
}

func extractSQLiteChecks(createSQL string) []string {
	lower := strings.ToLower(createSQL)
	var checks []string
	quote := byte(0)
	for i := 0; i < len(createSQL); i++ {
		ch := createSQL[i]
		if quote != 0 {
			if ch == quote {
				if i+1 < len(createSQL) && createSQL[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			continue
		}
		if i+5 > len(createSQL) || lower[i:i+5] != "check" ||
			(i > 0 && (lower[i-1] == '_' || lower[i-1] >= 'a' && lower[i-1] <= 'z')) {
			continue
		}
		j := i + 5
		for j < len(createSQL) && (createSQL[j] == ' ' || createSQL[j] == '\t' || createSQL[j] == '\n' || createSQL[j] == '\r') {
			j++
		}
		if j >= len(createSQL) || createSQL[j] != '(' {
			continue
		}
		start := j + 1
		depth := 1
		innerQuote := byte(0)
		for j = start; j < len(createSQL); j++ {
			current := createSQL[j]
			if innerQuote != 0 {
				if current == innerQuote {
					if j+1 < len(createSQL) && createSQL[j+1] == innerQuote {
						j++
						continue
					}
					innerQuote = 0
				}
				continue
			}
			if current == '\'' || current == '"' || current == '`' {
				innerQuote = current
				continue
			}
			if current == '(' {
				depth++
			} else if current == ')' {
				depth--
				if depth == 0 {
					checks = append(checks, strings.TrimSpace(createSQL[start:j]))
					i = j
					break
				}
			}
		}
	}
	return checks
}

type tableLoadPlan struct {
	Order           []string
	DeferredColumns map[string]map[string]struct{}
}

func buildTableLoadPlan(sources map[string]*sourceTable, targets map[string]*targetTable) (tableLoadPlan, error) {
	allDependencies := make(map[string]map[string]struct{}, len(sources))
	for name := range sources {
		allDependencies[name] = make(map[string]struct{})
		for _, fk := range targets[name].ForeignKeys {
			if _, copied := sources[fk.ParentTable]; copied {
				allDependencies[name][fk.ParentTable] = struct{}{}
			}
		}
	}

	deferred := make(map[string]map[string]struct{})
	for name := range sources {
		for _, fk := range targets[name].ForeignKeys {
			if _, copied := sources[fk.ParentTable]; !copied {
				continue
			}
			if fk.ParentTable != name && !dependencyPathExists(allDependencies, fk.ParentTable, name, nil) {
				continue
			}
			for _, columnName := range fk.ChildColumns {
				column := targets[name].ColumnByName[columnName]
				if column == nil || !column.Nullable {
					return tableLoadPlan{}, fmt.Errorf("postgres foreign key cycle requires nullable column %s.%s for two-phase loading", name, columnName)
				}
				if deferred[name] == nil {
					deferred[name] = make(map[string]struct{})
				}
				deferred[name][columnName] = struct{}{}
			}
		}
	}

	dependencies := make(map[string]map[string]struct{}, len(sources))
	for name := range sources {
		dependencies[name] = make(map[string]struct{})
		for _, fk := range targets[name].ForeignKeys {
			if columns := deferred[name]; foreignKeyUsesDeferredColumn(fk, columns) {
				continue
			}
			if _, copied := sources[fk.ParentTable]; copied {
				dependencies[name][fk.ParentTable] = struct{}{}
			}
		}
	}
	plan := tableLoadPlan{DeferredColumns: deferred}
	for len(plan.Order) < len(sources) {
		var ready []string
		for name, deps := range dependencies {
			if len(deps) == 0 {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			var blocked []string
			for name, deps := range dependencies {
				var names []string
				for dep := range deps {
					names = append(names, dep)
				}
				sort.Strings(names)
				blocked = append(blocked, name+" -> "+strings.Join(names, ","))
			}
			sort.Strings(blocked)
			return tableLoadPlan{}, fmt.Errorf("postgres foreign key cycle cannot be loaded safely: %s", strings.Join(blocked, "; "))
		}
		sort.Strings(ready)
		for _, name := range ready {
			plan.Order = append(plan.Order, name)
			delete(dependencies, name)
			for _, deps := range dependencies {
				delete(deps, name)
			}
		}
	}
	return plan, nil
}

func dependencyPathExists(dependencies map[string]map[string]struct{}, current, target string, visited map[string]struct{}) bool {
	if current == target {
		return true
	}
	if visited == nil {
		visited = make(map[string]struct{})
	}
	if _, seen := visited[current]; seen {
		return false
	}
	visited[current] = struct{}{}
	for next := range dependencies[current] {
		if dependencyPathExists(dependencies, next, target, visited) {
			return true
		}
	}
	return false
}

func foreignKeyUsesDeferredColumn(fk foreignKey, columns map[string]struct{}) bool {
	if len(columns) == 0 {
		return false
	}
	for _, column := range fk.ChildColumns {
		if _, deferred := columns[column]; deferred {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func quoteSQLiteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func quotePostgresIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func qualifiedTable(schema, table string) string {
	return quotePostgresIdentifier(schema) + "." + quotePostgresIdentifier(table)
}
