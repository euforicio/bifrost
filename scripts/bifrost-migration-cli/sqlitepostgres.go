package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/scripts/bifrost-migration-cli/nativepostgres"
	"github.com/maximhq/bifrost/scripts/bifrost-migration-cli/sqlitetopostgres"
)

const postgresDSNEnv = "BIFROST_MIGRATION_POSTGRES_DSN"

func runSQLiteToPostgres(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected initialize, migrate, or verify subcommand")
	}
	switch args[0] {
	case "initialize":
		return runSQLiteToPostgresInitialize(args[1:])
	case "migrate":
		return runSQLiteToPostgresMigrate(args[1:])
	case "verify":
		return runSQLiteToPostgresVerify(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; expected initialize, migrate, or verify", args[0])
	}
}

func runSQLiteToPostgresInitialize(args []string) error {
	flags := flag.NewFlagSet("sqlite-to-postgres initialize", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	allowInsecurePostgres := flags.Bool("allow-insecure-postgres", false, "allow sslmode=disable only for an explicit loopback PostgreSQL URL")
	schema := flags.String("schema", "public", "empty PostgreSQL schema (native initialization supports public)")
	timeout := flags.Duration("timeout", 15*time.Minute, "native schema initialization timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	dsn, err := resolvePostgresEnvironment(*allowInsecurePostgres)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := nativepostgres.Initialize(ctx, dsn, *schema, nativeSchemaLogger{}); err != nil {
		return err
	}
	fmt.Println("sqlite-to-postgres: native PostgreSQL schema initialized; destination business tables are empty")
	return nil
}

func runSQLiteToPostgresMigrate(args []string) error {
	flags := flag.NewFlagSet("sqlite-to-postgres migrate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config-sqlite", "", "path to the Bifrost config SQLite database")
	logsPath := flags.String("logs-sqlite", "", "path to the Bifrost logs SQLite database")
	snapshotDir := flags.String("snapshot-dir", "", "new directory that will retain config.sqlite and logs.sqlite rollback snapshots")
	postgresDSN := flags.String("postgres-dsn", "", "PostgreSQL DSN (prefer "+postgresDSNEnv+")")
	allowInsecurePostgres := flags.Bool("allow-insecure-postgres", false, "allow sslmode=disable only for an explicit loopback PostgreSQL URL")
	schema := flags.String("schema", "public", "initialized PostgreSQL schema")
	timeout := flags.Duration("timeout", 2*time.Hour, "migration timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *configPath == "" || *logsPath == "" || *snapshotDir == "" {
		return fmt.Errorf("--config-sqlite, --logs-sqlite, and --snapshot-dir are required")
	}
	dsn, err := resolvePostgresDSN(*postgresDSN, *allowInsecurePostgres)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	snapshots, err := sqlitetopostgres.CreateSnapshots(ctx, *configPath, *logsPath, *snapshotDir)
	if err != nil {
		return err
	}
	report, err := sqlitetopostgres.Migrate(ctx, snapshots, dsn, *schema)
	if err != nil {
		return fmt.Errorf("migration failed; rollback snapshots retained at %s: %w", *snapshotDir, err)
	}
	printSQLitePostgresReport("migration committed", *snapshotDir, report)
	return nil
}

func runSQLiteToPostgresVerify(args []string) error {
	flags := flag.NewFlagSet("sqlite-to-postgres verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	snapshotDir := flags.String("snapshot-dir", "", "directory containing retained config.sqlite and logs.sqlite snapshots")
	postgresDSN := flags.String("postgres-dsn", "", "PostgreSQL DSN (prefer "+postgresDSNEnv+")")
	allowInsecurePostgres := flags.Bool("allow-insecure-postgres", false, "allow sslmode=disable only for an explicit loopback PostgreSQL URL")
	schema := flags.String("schema", "public", "initialized PostgreSQL schema")
	timeout := flags.Duration("timeout", 2*time.Hour, "verification timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *snapshotDir == "" {
		return fmt.Errorf("--snapshot-dir is required")
	}
	dsn, err := resolvePostgresDSN(*postgresDSN, *allowInsecurePostgres)
	if err != nil {
		return err
	}
	snapshots, err := sqlitetopostgres.OpenSnapshots(*snapshotDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := sqlitetopostgres.Verify(ctx, snapshots, dsn, *schema)
	if err != nil {
		return err
	}
	printSQLitePostgresReport("verification passed", *snapshotDir, report)
	return nil
}

func resolvePostgresDSN(flagValue string, allowInsecure bool) (string, error) {
	dsn := strings.TrimSpace(flagValue)
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv(postgresDSNEnv))
	}
	if dsn == "" {
		return "", fmt.Errorf("--postgres-dsn or %s is required", postgresDSNEnv)
	}
	if err := validatePostgresTransport(dsn, allowInsecure); err != nil {
		return "", err
	}
	return dsn, nil
}

func resolvePostgresEnvironment(allowInsecure bool) (string, error) {
	dsn := strings.TrimSpace(os.Getenv(postgresDSNEnv))
	if dsn == "" {
		return "", fmt.Errorf("%s is required", postgresDSNEnv)
	}
	if err := validatePostgresTransport(dsn, allowInsecure); err != nil {
		return "", err
	}
	return dsn, nil
}

func validatePostgresTransport(rawDSN string, allowInsecure bool) error {
	parsed, err := url.Parse(rawDSN)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() == "" {
		return fmt.Errorf("PostgreSQL target must be a postgres:// or postgresql:// URL")
	}
	query := parsed.Query()
	modes := query["sslmode"]
	if len(modes) != 1 {
		return fmt.Errorf("PostgreSQL URL must set sslmode exactly once")
	}
	for _, key := range []string{"host", "port", "service", "servicefile"} {
		if _, present := query[key]; present {
			return fmt.Errorf("PostgreSQL URL must declare its endpoint in the URL authority")
		}
	}
	if strings.Contains(parsed.Host, ",") {
		return fmt.Errorf("PostgreSQL URL must declare exactly one endpoint")
	}

	config, err := pgx.ParseConfig(rawDSN)
	if err != nil {
		return fmt.Errorf("invalid PostgreSQL URL")
	}
	expectedPort := uint16(5432)
	if port := parsed.Port(); port != "" {
		value, parseErr := strconv.ParseUint(port, 10, 16)
		if parseErr != nil || value == 0 {
			return fmt.Errorf("PostgreSQL URL contains an invalid port")
		}
		expectedPort = uint16(value)
	}
	if config.Host != parsed.Hostname() || config.Port != expectedPort || len(config.Fallbacks) != 0 {
		return fmt.Errorf("PostgreSQL effective endpoint must exactly match the URL authority")
	}

	mode := strings.ToLower(strings.TrimSpace(modes[0]))
	if mode == "verify-full" {
		if config.TLSConfig == nil || config.TLSConfig.InsecureSkipVerify || config.TLSConfig.ServerName != config.Host {
			return fmt.Errorf("PostgreSQL verify-full did not produce hostname-verifying TLS")
		}
		return nil
	}
	if allowInsecure && mode == "disable" && isLoopbackPostgresHost(config.Host) && config.TLSConfig == nil {
		return nil
	}
	return fmt.Errorf("PostgreSQL requires sslmode=verify-full; insecure mode is limited to explicitly enabled loopback development")
}

func isLoopbackPostgresHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func printSQLitePostgresReport(status, snapshotDir string, report sqlitetopostgres.Report) {
	fmt.Printf("sqlite-to-postgres: %s\n", status)
	fmt.Printf("rollback snapshots: %s\n", snapshotDir)
	fmt.Printf("config snapshot sha256: %s\n", report.ConfigSHA256)
	fmt.Printf("logs snapshot sha256: %s\n", report.LogsSHA256)
	for _, table := range report.Tables {
		fmt.Printf("%s.%s: sqlite=%d postgres=%d digest=%s\n", table.Store, table.Table, table.SourceRows, table.TargetRows, table.Digest)
	}
}

type nativeSchemaLogger struct{}

func (nativeSchemaLogger) Debug(string, ...any)                   {}
func (nativeSchemaLogger) Info(string, ...any)                    {}
func (nativeSchemaLogger) Warn(string, ...any)                    {}
func (nativeSchemaLogger) Error(string, ...any)                   {}
func (nativeSchemaLogger) Fatal(string, ...any)                   {}
func (nativeSchemaLogger) SetLevel(schemas.LogLevel)              {}
func (nativeSchemaLogger) SetOutputType(schemas.LoggerOutputType) {}
func (nativeSchemaLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}
