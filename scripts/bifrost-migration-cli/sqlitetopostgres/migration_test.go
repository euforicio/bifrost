package sqlitetopostgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const postgresTestDSNEnv = "BIFROST_MIGRATION_TEST_POSTGRES_DSN"

func TestMigrateAndVerifyRealPostgres(t *testing.T) {
	postgresDSN := requirePostgresTestDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn := openPostgresTestConnection(t, ctx, postgresDSN)
	defer conn.Close(ctx)
	schema := createPostgresTestSchema(t, ctx, conn, false)
	defer dropPostgresTestSchema(t, context.Background(), conn, schema)

	configPath, logsPath := createSQLiteFixtures(t, false)
	configHashBefore, err := fileSHA256(configPath)
	require.NoError(t, err)
	logsHashBefore, err := fileSHA256(logsPath)
	require.NoError(t, err)
	snapshotDir := filepath.Join(t.TempDir(), "rollback")
	snapshots, err := CreateSnapshots(ctx, configPath, logsPath, snapshotDir)
	require.NoError(t, err)

	report, err := Migrate(ctx, snapshots, postgresDSN, schema)
	require.NoError(t, err)
	require.Len(t, report.Tables, 7)
	require.NotEmpty(t, report.ConfigSHA256)
	require.NotEmpty(t, report.LogsSHA256)

	verified, err := Verify(ctx, snapshots, postgresDSN, schema)
	require.NoError(t, err)
	require.Equal(t, report, verified)

	configHashAfter, err := fileSHA256(configPath)
	require.NoError(t, err)
	logsHashAfter, err := fileSHA256(logsPath)
	require.NoError(t, err)
	require.Equal(t, configHashBefore, configHashAfter, "config sqlite source must remain unchanged")
	require.Equal(t, logsHashBefore, logsHashAfter, "logs sqlite source must remain unchanged")
	require.FileExists(t, snapshots.ConfigPath)
	require.FileExists(t, snapshots.LogsPath)

	var providerID int64
	var enabled bool
	var metadata string
	var createdAt time.Time
	err = conn.QueryRow(ctx, "SELECT id, enabled, metadata_json::text, created_at FROM "+qualifiedTable(schema, "config_providers")+" WHERE id = 42").Scan(&providerID, &enabled, &metadata, &createdAt)
	require.NoError(t, err)
	require.Equal(t, int64(42), providerID)
	require.True(t, enabled)
	require.JSONEq(t, `{"region":"us-west-2","nested":{"b":2,"a":1},"ratio":1.0}`, metadata)
	require.Equal(t, time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), createdAt.UTC())

	var nextSerial int64
	err = conn.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence($1, $2))", qualifiedTable(schema, "config_providers"), "id").Scan(&nextSerial)
	require.NoError(t, err)
	require.Equal(t, int64(101), nextSerial)

	_, err = Migrate(ctx, snapshots, postgresDSN, schema)
	require.ErrorContains(t, err, "refusing to overwrite or merge business data")
}

func TestMigrateRollsBackWholeDestinationTransaction(t *testing.T) {
	postgresDSN := requirePostgresTestDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn := openPostgresTestConnection(t, ctx, postgresDSN)
	defer conn.Close(ctx)
	schema := createPostgresTestSchema(t, ctx, conn, true)
	defer dropPostgresTestSchema(t, context.Background(), conn, schema)

	configPath, logsPath := createSQLiteFixtures(t, true)
	snapshotDir := filepath.Join(t.TempDir(), "rollback")
	snapshots, err := CreateSnapshots(ctx, configPath, logsPath, snapshotDir)
	require.NoError(t, err)

	_, err = Migrate(ctx, snapshots, postgresDSN, schema)
	require.ErrorContains(t, err, "config_providers")
	require.FileExists(t, snapshots.ConfigPath)
	require.FileExists(t, snapshots.LogsPath)

	for _, table := range []string{"config_providers", "config_keys", "logs", "mcp_tool_logs", "cycle_owners", "cycle_budgets"} {
		var count int64
		err := conn.QueryRow(ctx, "SELECT COUNT(*) FROM "+qualifiedTable(schema, table)).Scan(&count)
		require.NoError(t, err)
		require.Zero(t, count, "table %s must remain empty after rollback", table)
	}
	var appliedAt time.Time
	err = conn.QueryRow(ctx, "SELECT applied_at FROM "+qualifiedTable(schema, migrationTableName)+" WHERE id = 'config_init'").Scan(&appliedAt)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), appliedAt.UTC(), "migration metadata update must roll back too")
	var lastValue int64
	var called bool
	err = conn.QueryRow(ctx, "SELECT last_value, is_called FROM "+qualifiedTable(schema, "config_providers_id_seq")).Scan(&lastValue, &called)
	require.NoError(t, err)
	require.Equal(t, int64(1), lastValue, "sequence restart must roll back")
	require.Equal(t, false, called, "sequence restart must roll back")
}

func requirePostgresTestDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(postgresTestDSNEnv))
	if dsn == "" {
		t.Skipf("%s is not set", postgresTestDSNEnv)
	}
	return dsn
}

func openPostgresTestConnection(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	return conn
}

func createPostgresTestSchema(t *testing.T, ctx context.Context, conn *pgx.Conn, mutateCopiedRow bool) string {
	t.Helper()
	schema := fmt.Sprintf("bf_migrate_%d", time.Now().UnixNano())
	_, err := conn.Exec(ctx, "CREATE SCHEMA "+quotePostgresIdentifier(schema))
	require.NoError(t, err)
	statements := []string{
		`CREATE TABLE ` + qualifiedTable(schema, migrationTableName) + ` (
			id varchar(255) PRIMARY KEY,
			sequence bigint NOT NULL,
			applied_at timestamptz NOT NULL,
			status varchar(20) NOT NULL
		)`,
		`CREATE TABLE ` + qualifiedTable(schema, "config_providers") + ` (
			id bigserial PRIMARY KEY,
			name text NOT NULL UNIQUE,
			enabled boolean NOT NULL,
			metadata_json jsonb,
			created_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL,
			revision integer NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE ` + qualifiedTable(schema, "config_keys") + ` (
			id varchar(36) PRIMARY KEY,
			provider_id bigint NOT NULL REFERENCES ` + qualifiedTable(schema, "config_providers") + `(id),
			secret_blob bytea,
			created_at timestamptz NOT NULL
		)`,
		`CREATE TABLE ` + qualifiedTable(schema, "logs") + ` (
			id varchar(255) PRIMARY KEY,
			timestamp timestamptz NOT NULL,
			provider varchar(255) NOT NULL,
			cost numeric NOT NULL,
			latency real NOT NULL,
			metadata jsonb,
			success boolean NOT NULL
		)`,
		`CREATE TABLE ` + qualifiedTable(schema, "mcp_tool_logs") + ` (
			id varchar(255) PRIMARY KEY,
			request_id varchar(255) NOT NULL REFERENCES ` + qualifiedTable(schema, "logs") + `(id),
			timestamp timestamptz NOT NULL
		)`,
		`CREATE TABLE ` + qualifiedTable(schema, "cycle_owners") + ` (
			id varchar(36) PRIMARY KEY,
			legacy_budget_id varchar(36)
		)`,
		`CREATE TABLE ` + qualifiedTable(schema, "cycle_budgets") + ` (
			id varchar(36) PRIMARY KEY,
			owner_id varchar(36) REFERENCES ` + qualifiedTable(schema, "cycle_owners") + `(id)
		)`,
		`ALTER TABLE ` + qualifiedTable(schema, "cycle_owners") + ` ADD FOREIGN KEY (legacy_budget_id) REFERENCES ` + qualifiedTable(schema, "cycle_budgets") + `(id)`,
		`INSERT INTO ` + qualifiedTable(schema, migrationTableName) + ` (id, sequence, applied_at, status) VALUES
			('config_init', 1, '2026-01-01T00:00:00Z', 'success'),
			('logs_init', 2, '2026-01-01T00:00:00Z', 'success'),
			('target_only', 3, '2026-01-01T00:00:00Z', 'success')`,
	}
	for _, statement := range statements {
		_, err := conn.Exec(ctx, statement)
		require.NoError(t, err)
	}
	if mutateCopiedRow {
		_, err := conn.Exec(ctx, `CREATE FUNCTION `+qualifiedTable(schema, "mutate_migrated_provider")+`() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN NEW.name := NEW.name || '-changed'; RETURN NEW; END $$`)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `CREATE TRIGGER mutate_migrated_provider BEFORE INSERT ON `+qualifiedTable(schema, "config_providers")+`
			FOR EACH ROW EXECUTE FUNCTION `+qualifiedTable(schema, "mutate_migrated_provider")+`()`)
		require.NoError(t, err)
	}
	return schema
}

func dropPostgresTestSchema(t *testing.T, ctx context.Context, conn *pgx.Conn, schema string) {
	t.Helper()
	_, err := conn.Exec(ctx, "DROP SCHEMA "+quotePostgresIdentifier(schema)+" CASCADE")
	require.NoError(t, err)
}

func createSQLiteFixtures(t *testing.T, _ bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.db")
	logsPath := filepath.Join(dir, "logs.db")
	config := openWritableSQLite(t, configPath)
	execSQLiteStatements(t, config,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE migrations (id TEXT PRIMARY KEY, sequence INTEGER NOT NULL, applied_at DATETIME NOT NULL, status TEXT NOT NULL)`,
		`CREATE TABLE config_providers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			enabled BOOLEAN NOT NULL,
			metadata_json TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE config_keys (
			id TEXT PRIMARY KEY,
			provider_id INTEGER NOT NULL REFERENCES config_providers(id),
			secret_blob BLOB,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE cycle_owners (id TEXT PRIMARY KEY, legacy_budget_id TEXT REFERENCES cycle_budgets(id))`,
		`CREATE TABLE cycle_budgets (id TEXT PRIMARY KEY, owner_id TEXT REFERENCES cycle_owners(id))`,
		`INSERT INTO migrations VALUES ('config_init', 1, '2025-01-01T00:00:00Z', 'success')`,
		`INSERT INTO config_providers VALUES (42, 'synthetic', 1, '{"nested":{"a":1,"b":2},"ratio":1.0,"region":"us-west-2"}', '2025-01-02T03:04:05Z', '2025-01-03T04:05:06Z')`,
		`INSERT INTO config_keys VALUES ('key-1', 42, x'000102FEFF', '2025-01-04T05:06:07Z')`,
		`INSERT INTO cycle_owners VALUES ('owner-1', NULL)`,
		`INSERT INTO cycle_budgets VALUES ('budget-1', 'owner-1')`,
		`UPDATE cycle_owners SET legacy_budget_id = 'budget-1' WHERE id = 'owner-1'`,
		`INSERT INTO config_providers VALUES (100, 'deleted-high-water', 1, NULL, '2025-01-02T03:04:05Z', '2025-01-03T04:05:06Z')`,
		`DELETE FROM config_providers WHERE id = 100`,
	)
	require.NoError(t, config.Close())

	logs := openWritableSQLite(t, logsPath)
	execSQLiteStatements(t, logs,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE migrations (id TEXT PRIMARY KEY, sequence INTEGER NOT NULL, applied_at DATETIME NOT NULL, status TEXT NOT NULL)`,
		`CREATE TABLE logs (
			id TEXT PRIMARY KEY,
			timestamp DATETIME NOT NULL,
			provider TEXT NOT NULL,
			cost REAL NOT NULL,
			latency REAL NOT NULL,
			metadata TEXT,
			success BOOLEAN NOT NULL
		)`,
		`CREATE TABLE mcp_tool_logs (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL REFERENCES logs(id),
			timestamp DATETIME NOT NULL
		)`,
		`INSERT INTO migrations VALUES ('logs_init', 1, '2025-01-01T00:00:01Z', 'success')`,
		`INSERT INTO logs VALUES ('log-1', '2025-02-03T04:05:06.123456Z', 'synthetic', 1.25, 0.1, '{"trace":"synthetic","tags":["a","b"]}', 1)`,
		`INSERT INTO mcp_tool_logs VALUES ('mcp-1', 'log-1', '2025-02-03T04:05:07Z')`,
	)
	require.NoError(t, logs.Close())
	return configPath, logsPath
}

func openWritableSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	return db
}

func execSQLiteStatements(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		_, err := db.Exec(statement)
		require.NoError(t, err)
	}
}

type testRequire struct{}

var require testRequire

func (testRequire) NoError(t *testing.T, err error, message ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v %v", err, message)
	}
}

func (testRequire) ErrorContains(t *testing.T, err error, contains string, message ...any) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("error %v does not contain %q %v", err, contains, message)
	}
}

func (testRequire) Equal(t *testing.T, expected, actual any, message ...any) {
	t.Helper()
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("not equal:\nexpected: %#v\nactual:   %#v\n%v", expected, actual, message)
	}
}

func (testRequire) Len(t *testing.T, value any, expected int, message ...any) {
	t.Helper()
	got := reflect.ValueOf(value).Len()
	if got != expected {
		t.Fatalf("length is %d, expected %d %v", got, expected, message)
	}
}

func (testRequire) NotEmpty(t *testing.T, value any, message ...any) {
	t.Helper()
	if reflect.ValueOf(value).Len() == 0 {
		t.Fatalf("value is empty %v", message)
	}
}

func (testRequire) True(t *testing.T, value bool, message ...any) {
	t.Helper()
	if !value {
		t.Fatalf("value is false %v", message)
	}
}

func (testRequire) Zero(t *testing.T, value any, message ...any) {
	t.Helper()
	if !reflect.ValueOf(value).IsZero() {
		t.Fatalf("value is not zero: %#v %v", value, message)
	}
}

func (testRequire) Contains(t *testing.T, value, contains string, message ...any) {
	t.Helper()
	if !strings.Contains(value, contains) {
		t.Fatalf("%q does not contain %q %v", value, contains, message)
	}
}

func (testRequire) JSONEq(t *testing.T, expected, actual string, message ...any) {
	t.Helper()
	var expectedValue, actualValue any
	if err := json.Unmarshal([]byte(expected), &expectedValue); err != nil {
		t.Fatalf("invalid expected json: %v", err)
	}
	if err := json.Unmarshal([]byte(actual), &actualValue); err != nil {
		t.Fatalf("invalid actual json: %v", err)
	}
	if !reflect.DeepEqual(expectedValue, actualValue) {
		t.Fatalf("json differs:\nexpected: %s\nactual:   %s\n%v", expected, actual, message)
	}
}

func (testRequire) FileExists(t *testing.T, path string, message ...any) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("file does not exist: %s: %v %v", path, err, message)
	}
}
