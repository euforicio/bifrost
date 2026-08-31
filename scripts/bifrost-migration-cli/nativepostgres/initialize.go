package nativepostgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/postgresconn"
)

const (
	initializerMaxOpenConns = 2
	initializerMaxIdleConns = 1
)

var tlsURLToEnvironment = map[string]string{
	"sslrootcert": "PGSSLROOTCERT",
	"sslcert":     "PGSSLCERT",
	"sslkey":      "PGSSLKEY",
	"sslpassword": "PGSSLPASSWORD",
}

// Initialize creates the native configstore and logstore PostgreSQL schemas
// without starting the HTTP server or synchronizing runtime configuration.
// Repeated initialization is allowed only while every non-ledger table in the
// dedicated destination schema remains empty.
func Initialize(ctx context.Context, rawDSN, schema string, logger schemas.Logger) (err error) {
	if schema != "public" {
		return fmt.Errorf("native schema initialization supports only the public schema")
	}
	if logger == nil {
		return fmt.Errorf("native schema initialization requires a logger")
	}

	restoreTLS, err := applyTLSFileEnvironment(rawDSN)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, restoreTLS())
	}()

	connection, err := connectionConfig(rawDSN)
	if err != nil {
		return err
	}
	if err := requireEmptyBusinessTables(ctx, rawDSN, schema); err != nil {
		return err
	}

	configStore, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypePostgres,
		Config:  &connection,
	}, logger)
	if err != nil {
		return fmt.Errorf("initialize native config store schema: %w", err)
	}
	if err := configStore.Close(ctx); err != nil {
		return fmt.Errorf("close native config store initializer: %w", err)
	}
	if err := requireEmptyBusinessTables(ctx, rawDSN, schema); err != nil {
		return err
	}

	logsStore, err := logstore.NewLogStore(ctx, &logstore.Config{
		Enabled:       true,
		Type:          logstore.LogStoreTypePostgres,
		RetentionDays: 30,
		Config: &logstore.PostgresConfig{
			Config:                 connection,
			MatViewRefreshInterval: "off",
		},
	}, logger)
	if err != nil {
		return fmt.Errorf("initialize native log store schema: %w", err)
	}
	if err := logsStore.Close(ctx); err != nil {
		return fmt.Errorf("close native log store initializer: %w", err)
	}
	if err := requireEmptyBusinessTables(ctx, rawDSN, schema); err != nil {
		return err
	}
	return nil
}

func connectionConfig(rawDSN string) (postgresconn.Config, error) {
	parsedURL, err := url.Parse(rawDSN)
	if err != nil {
		return postgresconn.Config{}, fmt.Errorf("invalid PostgreSQL URL")
	}
	sslModes := parsedURL.Query()["sslmode"]
	if len(sslModes) != 1 || strings.TrimSpace(sslModes[0]) == "" {
		return postgresconn.Config{}, fmt.Errorf("PostgreSQL URL must set sslmode exactly once")
	}
	parsed, err := pgx.ParseConfig(rawDSN)
	if err != nil {
		return postgresconn.Config{}, fmt.Errorf("invalid PostgreSQL URL")
	}
	secret := func(value string) *schemas.SecretVar {
		return &schemas.SecretVar{Val: value, SecretType: schemas.SecretTypePlainText}
	}
	return postgresconn.Config{
		Host:         secret(parsed.Host),
		Port:         secret(strconv.FormatUint(uint64(parsed.Port), 10)),
		User:         secret(parsed.User),
		Password:     secret(parsed.Password),
		DBName:       secret(parsed.Database),
		SSLMode:      secret(strings.ToLower(strings.TrimSpace(sslModes[0]))),
		MaxOpenConns: initializerMaxOpenConns,
		MaxIdleConns: initializerMaxIdleConns,
	}, nil
}

func requireEmptyBusinessTables(ctx context.Context, rawDSN, schema string) error {
	conn, err := pgx.Connect(ctx, rawDSN)
	if err != nil {
		return fmt.Errorf("connect for native schema initialization preflight: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	rows, err := conn.Query(ctx, `
		SELECT c.relname
		FROM pg_catalog.pg_class AS c
		JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p')
		  AND c.relname <> 'migrations'
		ORDER BY c.relname`, schema)
	if err != nil {
		return fmt.Errorf("discover PostgreSQL destination tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return fmt.Errorf("scan PostgreSQL destination table: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate PostgreSQL destination tables: %w", err)
	}
	rows.Close()

	for _, table := range tables {
		var populated bool
		query := "SELECT EXISTS (SELECT 1 FROM " + qualifiedIdentifier(schema, table) + " LIMIT 1)"
		if err := conn.QueryRow(ctx, query).Scan(&populated); err != nil {
			return fmt.Errorf("check PostgreSQL destination table %s: %w", table, err)
		}
		if populated {
			return fmt.Errorf("PostgreSQL destination business table %s is not empty; refusing native schema initialization", table)
		}
	}
	return nil
}

func applyTLSFileEnvironment(rawDSN string) (func() error, error) {
	settings, err := tlsFileEnvironment(rawDSN)
	if err != nil {
		return nil, err
	}
	type priorValue struct {
		value  string
		exists bool
	}
	prior := make(map[string]priorValue, len(settings))
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	restore := func() error {
		var restoreErr error
		for _, key := range keys {
			value, exists := prior[key]
			if !exists {
				continue
			}
			if value.exists {
				restoreErr = errors.Join(restoreErr, os.Setenv(key, value.value))
			} else {
				restoreErr = errors.Join(restoreErr, os.Unsetenv(key))
			}
		}
		if restoreErr != nil {
			return fmt.Errorf("restore PostgreSQL TLS environment: %w", restoreErr)
		}
		return nil
	}
	for _, key := range keys {
		value, exists := os.LookupEnv(key)
		prior[key] = priorValue{value: value, exists: exists}
		if err := os.Setenv(key, settings[key]); err != nil {
			_ = restore()
			return nil, fmt.Errorf("set PostgreSQL TLS environment: %w", err)
		}
	}
	return restore, nil
}

func tlsFileEnvironment(rawDSN string) (map[string]string, error) {
	parsed, err := url.Parse(rawDSN)
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL URL")
	}
	settings := make(map[string]string)
	query := parsed.Query()
	for parameter, environment := range tlsURLToEnvironment {
		values, present := query[parameter]
		if !present {
			continue
		}
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return nil, fmt.Errorf("PostgreSQL URL must set %s exactly once when provided", parameter)
		}
		settings[environment] = values[0]
	}
	return settings, nil
}

func qualifiedIdentifier(schema, table string) string {
	return quoteIdentifier(schema) + "." + quoteIdentifier(table)
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}
