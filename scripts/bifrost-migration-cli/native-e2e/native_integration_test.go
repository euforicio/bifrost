package nativee2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/postgresconn"
	migration "github.com/maximhq/bifrost/scripts/bifrost-migration-cli/sqlitetopostgres"
)

const postgresTestDSNEnv = "BIFROST_MIGRATION_TEST_POSTGRES_DSN"

// TestNativeBifrostSchemasRoundTrip couples the migration release gate to the
// schemas produced by the same Bifrost checkout. It uses the public store
// constructors for both dialects rather than another hand-written schema.
func TestNativeBifrostSchemasRoundTrip(t *testing.T) {
	baseDSN := strings.TrimSpace(os.Getenv(postgresTestDSNEnv))
	if baseDSN == "" {
		t.Skipf("%s is not set", postgresTestDSNEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	parsed, err := pgx.ParseConfig(baseDSN)
	mustNoError(t, err)
	admin, err := pgx.ConnectConfig(ctx, parsed)
	mustNoError(t, err)
	defer admin.Close(context.Background())

	databaseName := fmt.Sprintf("bf_migrate_native_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE DATABASE "+quoteIdentifier(databaseName)+" ENCODING 'UTF8' TEMPLATE template0")
	mustNoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		cleanup, connectErr := pgx.ConnectConfig(cleanupCtx, parsed)
		if connectErr != nil {
			t.Errorf("connect for native database cleanup: %v", connectErr)
			return
		}
		defer cleanup.Close(cleanupCtx)
		if _, dropErr := cleanup.Exec(cleanupCtx, "DROP DATABASE "+quoteIdentifier(databaseName)+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop native test database: %v", dropErr)
		}
	})

	nativePGConfig := parsed.Copy()
	nativePGConfig.Database = databaseName
	connection := nativeStoreConnection(nativePGConfig)
	nativeDSN := postgresconn.BuildDSN(&connection)
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)

	configPath := t.TempDir() + "/config.db"
	logsPath := t.TempDir() + "/logs.db"
	configSQLite, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: configPath},
	}, logger)
	mustNoError(t, err)
	mustNoError(t, configSQLite.Close(ctx))
	logsSQLite, err := logstore.NewLogStore(ctx, &logstore.Config{
		Enabled:       true,
		Type:          logstore.LogStoreTypeSQLite,
		RetentionDays: 30,
		Config:        &logstore.SQLiteConfig{Path: logsPath},
	}, logger)
	mustNoError(t, err)
	mustNoError(t, logsSQLite.Close(ctx))
	setSQLiteSequenceHighWater(t, configPath, "config_client", 500)

	configPostgres, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypePostgres,
		Config:  &connection,
	}, logger)
	mustNoError(t, err)
	mustNoError(t, configPostgres.Close(ctx))
	logsPostgres, err := logstore.NewLogStore(ctx, &logstore.Config{
		Enabled:       true,
		Type:          logstore.LogStoreTypePostgres,
		RetentionDays: 30,
		Config: &logstore.PostgresConfig{
			Config:                 connection,
			MatViewRefreshInterval: "off",
		},
	}, logger)
	mustNoError(t, err)
	mustNoError(t, logsPostgres.Close(ctx))

	snapshots, err := migration.CreateSnapshots(ctx, configPath, logsPath, t.TempDir()+"/rollback")
	mustNoError(t, err)
	report, err := migration.Migrate(ctx, snapshots, nativeDSN, "public")
	mustNoError(t, err)
	if len(report.Tables) == 0 {
		t.Fatal("native migration report contains no tables")
	}
	verified, err := migration.Verify(ctx, snapshots, nativeDSN, "public")
	mustNoError(t, err)
	if !reflect.DeepEqual(report, verified) {
		t.Fatalf("native verification report differs:\nmigrate=%#v\nverify=%#v", report, verified)
	}

	verification, err := pgx.Connect(ctx, nativeDSN)
	mustNoError(t, err)
	var lastValue int64
	var called bool
	err = verification.QueryRow(ctx, `SELECT last_value, is_called FROM public.config_client_id_seq`).Scan(&lastValue, &called)
	mustNoError(t, err)
	if lastValue != 501 || called {
		t.Fatalf("config_client sequence state = (%d, called=%t), want (501, false)", lastValue, called)
	}
	mustNoError(t, verification.Close(ctx))

	configRuntime, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypePostgres,
		Config:  &connection,
	}, logger)
	mustNoError(t, err)
	mustNoError(t, configRuntime.Close(ctx))
	logsRuntime, err := logstore.NewLogStore(ctx, &logstore.Config{
		Enabled:       true,
		Type:          logstore.LogStoreTypePostgres,
		RetentionDays: 30,
		Config: &logstore.PostgresConfig{
			Config:                 connection,
			MatViewRefreshInterval: "off",
		},
	}, logger)
	mustNoError(t, err)
	mustNoError(t, logsRuntime.Close(ctx))
}

func nativeStoreConnection(config *pgx.ConnConfig) postgresconn.Config {
	sslMode := "require"
	if config.TLSConfig == nil {
		sslMode = "disable"
	}
	secret := func(value string) *schemas.SecretVar {
		return &schemas.SecretVar{Val: value, SecretType: schemas.SecretTypePlainText}
	}
	return postgresconn.Config{
		Host:         secret(config.Host),
		Port:         secret(strconv.FormatUint(uint64(config.Port), 10)),
		User:         secret(config.User),
		Password:     secret(config.Password),
		DBName:       secret(config.Database),
		SSLMode:      secret(sslMode),
		MaxOpenConns: 5,
		MaxIdleConns: 2,
	}
}

func setSQLiteSequenceHighWater(t *testing.T, path, table string, sequence int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	mustNoError(t, err)
	defer db.Close()
	result, err := db.Exec(`UPDATE sqlite_sequence SET seq = ? WHERE name = ?`, sequence, table)
	mustNoError(t, err)
	updated, err := result.RowsAffected()
	mustNoError(t, err)
	if updated == 0 {
		_, err = db.Exec(`INSERT INTO sqlite_sequence(name, seq) VALUES (?, ?)`, table, sequence)
		mustNoError(t, err)
	}
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func mustNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
