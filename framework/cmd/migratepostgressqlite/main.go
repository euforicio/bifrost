// migratepostgressqlite creates Bifrost's canonical SQLite config/log stores
// from one read-only PostgreSQL snapshot, preserving every application table.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/logstore"
)

type column struct{ name, kind string }

const timestampFormat = "2006-01-02 15:04:05.000000000"

type table struct {
	name    string
	columns []column
	target  *sql.DB
}
type result struct {
	Tables              int      `json:"tables"`
	Rows                int64    `json:"rows"`
	Fingerprint         string   `json:"fingerprint"`
	AddedColumnDefaults []string `json:"added_column_defaults,omitempty"`
}

func main() {
	configPath := flag.String("config", "", "new SQLite config path")
	logsPath := flag.String("logs", "", "new SQLite logs path")
	flag.Parse()
	if flag.NArg() != 0 || *configPath == "" || *logsPath == "" || os.Getenv("BIFROST_DATABASE_URL") == "" {
		fmt.Fprintln(os.Stderr, "-config, -logs and BIFROST_DATABASE_URL are required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	report, err := migrate(ctx, *configPath, *logsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", err)
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(report); err != nil {
		os.Exit(1)
	}
}
func migrate(ctx context.Context, configPath, logsPath string) (report result, err error) {
	var owned []string
	complete := false
	defer func() {
		if !complete {
			for _, path := range owned {
				for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
					_ = os.Remove(path + suffix)
				}
			}
		}
	}()
	for _, path := range []string{configPath, logsPath} {
		if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "?#") || strings.Contains(path, "://") || strings.HasPrefix(path, "file:") || path == ":memory:" {
			return report, errors.New("new SQLite filesystem paths required")
		}
		file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return report, e
		}
		owned = append(owned, path)
		if e = file.Close(); e != nil {
			return report, e
		}
	}
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	configStore, err := configstore.NewConfigStore(ctx, &configstore.Config{Enabled: true, Type: configstore.ConfigStoreTypeSQLite, Config: &configstore.SQLiteConfig{Path: configPath}}, logger)
	if err != nil {
		return report, err
	}
	defer configStore.Close(ctx)
	logsStore, err := logstore.NewLogStore(ctx, &logstore.Config{Enabled: true, Type: logstore.LogStoreTypeSQLite, Config: &logstore.SQLiteConfig{Path: logsPath}}, logger)
	if err != nil {
		return report, err
	}
	defer logsStore.Close(ctx)
	configDB, err := configStore.ScopedDB(ctx).DB()
	if err != nil {
		return report, err
	}
	logsDB, err := logsStore.(*logstore.RDBLogStore).ScopedDB(ctx).DB()
	if err != nil {
		return report, err
	}
	targets := map[string]*sql.DB{}
	for _, db := range []*sql.DB{configDB, logsDB} {
		db.SetMaxOpenConns(1)
		rows, e := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='migrations'")
		if e != nil {
			return report, e
		}
		for rows.Next() {
			var name string
			if e = rows.Scan(&name); e != nil {
				rows.Close()
				return report, e
			}
			if targets[name] != nil {
				rows.Close()
				return report, fmt.Errorf("ambiguous destination table %s", name)
			}
			targets[name] = db
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return report, e
		}
	}
	source, err := sql.Open("pgx", os.Getenv("BIFROST_DATABASE_URL"))
	if err != nil {
		return report, errors.New("invalid PostgreSQL source configuration")
	}
	defer source.Close()
	snapshot, err := source.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return report, err
	}
	defer snapshot.Rollback()
	// Canonical SQLite construction runs migrations before row copying. Require
	// their source equivalents to have run, so copying cannot skip a data migration.
	if err = verifyMigrationHistory(ctx, snapshot, configDB, logsDB); err != nil {
		return report, err
	}
	rows, err := snapshot.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' AND table_name!='migrations' ORDER BY table_name")
	if err != nil {
		return report, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return report, err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, err
	}
	if len(names) != len(targets) {
		return report, fmt.Errorf("source has %d application tables; canonical SQLite has %d", len(names), len(targets))
	}
	var tables []table
	for _, name := range names {
		db := targets[name]
		if db == nil {
			return report, fmt.Errorf("no canonical SQLite table %s", name)
		}
		t := table{name: name, target: db}
		rows, e := snapshot.QueryContext(ctx, "SELECT column_name,udt_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 ORDER BY ordinal_position", name)
		if e != nil {
			return report, e
		}
		for rows.Next() {
			var c column
			if e = rows.Scan(&c.name, &c.kind); e != nil {
				rows.Close()
				return report, e
			}
			t.columns = append(t.columns, c)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return report, e
		}
		columns, e := sqliteColumns(ctx, db, name)
		if e != nil {
			return report, e
		}
		sourceColumns := map[string]bool{}
		for _, c := range t.columns {
			sourceColumns[c.name] = true
		}
		for column, optional := range columns {
			if sourceColumns[column] {
				continue
			}
			if !optional {
				return report, fmt.Errorf("new required SQLite column %s.%s has no default", name, column)
			}
			report.AddedColumnDefaults = append(report.AddedColumnDefaults, name+"."+column)
		}
		for _, c := range t.columns {
			if _, exists := columns[c.name]; !exists {
				return report, fmt.Errorf("missing SQLite column %s.%s", name, c.name)
			}
			switch c.kind {
			case "bool", "bytea", "int2", "int4", "int8", "json", "jsonb", "numeric", "float4", "float8", "timestamp", "timestamptz", "text", "varchar":
			default:
				return report, fmt.Errorf("unsupported PostgreSQL type %s.%s=%s", name, c.name, c.kind)
			}
			if c.kind == "numeric" {
				var changed int64
				sql := "SELECT COUNT(*) FROM " + quote(name) + " WHERE " + quote(c.name) + " IS NOT NULL AND " + quote(c.name) + " <> ((" + quote(c.name) + "::float8)::text)::numeric"
				if e = snapshot.QueryRowContext(ctx, sql).Scan(&changed); e != nil {
					return report, e
				}
				if changed != 0 {
					return report, fmt.Errorf("numeric precision loss in %s.%s", name, c.name)
				}
			}
		}
		tables = append(tables, t)
	}
	transactions := map[*sql.DB]*sql.Tx{}
	for _, db := range []*sql.DB{configDB, logsDB} {
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			return report, e
		}
		defer tx.Rollback()
		transactions[db] = tx
		if _, e = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); e != nil {
			return report, e
		}
	}
	// Remove all canonical bootstrap rows before copying, so parent cascades
	// cannot erase already imported children.
	for _, t := range tables {
		if _, err = transactions[t.target].ExecContext(ctx, "DELETE FROM "+quote(t.name)); err != nil {
			return report, err
		}
	}
	type tableReceipt struct {
		digest [32]byte
		count  int64
	}
	receipts := map[string]tableReceipt{}
	for _, t := range tables {
		digest, count, e := copyTable(ctx, snapshot, transactions[t.target], t)
		if e != nil {
			return report, e
		}
		receipts[t.name] = tableReceipt{digest, count}
	}
	all := sha256.New()
	for _, t := range tables {
		targetDigest, targetCount, e := digestTable(ctx, transactions[t.target], t, false)
		if e != nil {
			return report, e
		}
		source := receipts[t.name]
		if source.digest != targetDigest || source.count != targetCount {
			return report, fmt.Errorf("record fingerprint mismatch for %s", t.name)
		}
		all.Write([]byte(t.name))
		all.Write(source.digest[:])
		report.Rows += source.count
	}
	// AUTOINCREMENT must retain deleted PostgreSQL identities as well as live IDs.
	for _, t := range tables {
		var autoincrement int
		if err = transactions[t.target].QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name=? AND upper(sql) LIKE '%AUTOINCREMENT%'", t.name).Scan(&autoincrement); err != nil {
			return report, err
		}
		if autoincrement == 0 {
			continue
		}
		var id string
		for _, c := range t.columns {
			if c.name == "id" {
				id = c.name
			}
		}
		if id == "" {
			return report, fmt.Errorf("unsupported identity in %s", t.name)
		}
		var sequence sql.NullInt64
		if err = snapshot.QueryRowContext(ctx, "SELECT pg_sequence_last_value(to_regclass(pg_get_serial_sequence($1,$2)))", t.name, id).Scan(&sequence); err != nil {
			return report, err
		}
		var maximum sql.NullInt64
		if err = snapshot.QueryRowContext(ctx, "SELECT MAX("+quote(id)+") FROM "+quote(t.name)).Scan(&maximum); err != nil {
			return report, err
		}
		high := max(sequence.Int64, maximum.Int64)
		tx := transactions[t.target]
		if _, err = tx.ExecContext(ctx, "DELETE FROM sqlite_sequence WHERE name=?", t.name); err != nil {
			return report, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO sqlite_sequence(name,seq) VALUES(?,?)", t.name, high); err != nil {
			return report, err
		}
	}
	for _, db := range []*sql.DB{configDB, logsDB} {
		tx := transactions[db]
		var integrity string
		if err = tx.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
			return report, errors.New("SQLite integrity failure")
		}
		rows, e := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
		if e != nil {
			return report, e
		}
		failed := rows.Next()
		e = rows.Err()
		rows.Close()
		if e != nil {
			return report, e
		}
		if failed {
			return report, errors.New("SQLite foreign key failure")
		}
	}
	for _, db := range []*sql.DB{configDB, logsDB} {
		if err = transactions[db].Commit(); err != nil {
			return report, err
		}
		if _, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return report, err
		}
		var journal string
		if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&journal); err != nil || journal != "delete" {
			return report, errors.New("SQLite export must use standalone rollback-journal format")
		}
	}
	report.Tables = len(tables)
	report.Fingerprint = hex.EncodeToString(all.Sum(nil))
	sort.Strings(report.AddedColumnDefaults)
	complete = true
	return report, nil
}
func sqliteColumns(ctx context.Context, db *sql.DB, name string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+quote(name)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var def any
		if err = rows.Scan(&cid, &name, &kind, &notNull, &def, &pk); err != nil {
			return nil, err
		}
		columns[name] = (notNull == 0 && pk == 0) || (def != nil && !strings.EqualFold(fmt.Sprint(def), "NULL"))
	}
	return columns, rows.Err()
}
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
func selectColumns(t table, postgres bool) string {
	out := make([]string, len(t.columns))
	for i, c := range t.columns {
		out[i] = quote(c.name)
		if postgres && c.kind == "numeric" {
			out[i] += "::float8"
		}
	}
	return strings.Join(out, ",")
}
func copyTable(ctx context.Context, source, destination *sql.Tx, t table) ([32]byte, int64, error) {
	rows, err := source.QueryContext(ctx, "SELECT "+selectColumns(t, true)+" FROM "+quote(t.name))
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer rows.Close()
	columns := make([]string, len(t.columns))
	placeholders := make([]string, len(columns))
	for i, c := range t.columns {
		columns[i] = quote(c.name)
		placeholders[i] = "?"
	}
	stmt, err := destination.PrepareContext(ctx, "INSERT INTO "+quote(t.name)+" ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(placeholders, ",")+")")
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer stmt.Close()
	return digestRows(rows, t, func(values []any) error { _, err := stmt.ExecContext(ctx, values...); return err })
}
func digestTable(ctx context.Context, tx *sql.Tx, t table, postgres bool) ([32]byte, int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+selectColumns(t, postgres)+" FROM "+quote(t.name))
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer rows.Close()
	return digestRows(rows, t, nil)
}
func digestRows(rows *sql.Rows, t table, insert func([]any) error) ([32]byte, int64, error) {
	values := make([]any, len(t.columns))
	dest := make([]any, len(values))
	for i := range values {
		dest[i] = &values[i]
	}
	var hashes []string
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return [32]byte{}, 0, err
		}
		h := sha256.New()
		for i, c := range t.columns {
			v, err := normalize(values[i], c.kind)
			if err != nil {
				return [32]byte{}, 0, fmt.Errorf("normalize %s.%s: %w", t.name, c.name, err)
			}
			values[i] = v
			encoded, err := canonical(v)
			if err != nil {
				return [32]byte{}, 0, err
			}
			binary.Write(h, binary.BigEndian, uint64(len(encoded)))
			h.Write(encoded)
		}
		if insert != nil {
			if err := insert(values); err != nil {
				return [32]byte{}, 0, fmt.Errorf("copy table %s: %w", t.name, err)
			}
		}
		hashes = append(hashes, string(h.Sum(nil)))
	}
	if err := rows.Err(); err != nil {
		return [32]byte{}, 0, err
	}
	sort.Strings(hashes)
	h := sha256.New()
	for _, hash := range hashes {
		h.Write([]byte(hash))
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, int64(len(hashes)), nil
}
func normalize(value any, kind string) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch kind {
	case "timestamp", "timestamptz":
		if date, ok := value.(time.Time); ok {
			return date.UTC().Format(timestampFormat), nil
		}
		if text, ok := value.(string); ok {
			date, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return nil, err
			}
			return date.UTC().Format(timestampFormat), nil
		}
	case "bool":
		switch v := value.(type) {
		case bool:
			if v {
				return int64(1), nil
			}
			return int64(0), nil
		case int64:
			return v, nil
		}
	case "numeric", "float4", "float8":
		switch v := value.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, errors.New("nonfinite number")
			}
			return v, nil
		case int64:
			return float64(v), nil
		case string:
			return strconv.ParseFloat(v, 64)
		}
	case "bytea":
		if bytes, ok := value.([]byte); ok {
			return append([]byte{}, bytes...), nil
		}
	case "text", "varchar", "json", "jsonb":
		if bytes, ok := value.([]byte); ok {
			return string(bytes), nil
		}
		if text, ok := value.(string); ok {
			return text, nil
		}
	case "int2", "int4", "int8":
		if n, ok := value.(int64); ok {
			return n, nil
		}
	}
	return nil, fmt.Errorf("unsupported SQL value %T for %s", value, kind)
}
func canonical(value any) ([]byte, error) {
	switch v := value.(type) {
	case nil:
		return []byte{'n'}, nil
	case int64:
		b := make([]byte, 9)
		b[0] = 'i'
		binary.BigEndian.PutUint64(b[1:], uint64(v))
		return b, nil
	case float64:
		b := make([]byte, 9)
		b[0] = 'f'
		binary.BigEndian.PutUint64(b[1:], math.Float64bits(v))
		return b, nil
	case string:
		return append([]byte{'s'}, []byte(v)...), nil
	case []byte:
		return append([]byte{'b'}, v...), nil
	}
	return nil, fmt.Errorf("unsupported canonical value %T", value)
}

func verifyMigrationHistory(ctx context.Context, source *sql.Tx, targets ...*sql.DB) error {
	applied := map[string]bool{}
	rows, err := source.QueryContext(ctx, "SELECT id FROM migrations")
	if err != nil {
		return errors.New("source migration history is required")
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		applied[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var missing []string
	checked := map[string]bool{}
	for _, target := range targets {
		rows, err := target.QueryContext(ctx, "SELECT id FROM migrations")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !applied[id] && !checked[id] {
				missing = append(missing, id)
			}
			checked[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("source must run current Bifrost migrations before conversion: %s", strings.Join(missing, ", "))
	}
	return nil
}
