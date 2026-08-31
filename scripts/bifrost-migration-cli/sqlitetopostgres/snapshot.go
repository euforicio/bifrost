package sqlitetopostgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
)

const (
	configSnapshotName = "config.sqlite"
	logsSnapshotName   = "logs.sqlite"
)

// Snapshots are immutable, retained SQLite rollback points used by migration
// and post-migration verification.
type Snapshots struct {
	ConfigPath   string
	LogsPath     string
	ConfigSHA256 string
	LogsSHA256   string
}

// CreateSnapshots makes transactionally consistent online backups of both
// SQLite stores. The destination directory must not exist; snapshots are never
// removed automatically, including when the PostgreSQL migration later fails.
func CreateSnapshots(ctx context.Context, configPath, logsPath, snapshotDir string) (Snapshots, error) {
	configPath, configInfo, err := validateSourcePath(configPath)
	if err != nil {
		return Snapshots{}, fmt.Errorf("config sqlite: %w", err)
	}
	logsPath, logsInfo, err := validateSourcePath(logsPath)
	if err != nil {
		return Snapshots{}, fmt.Errorf("logs sqlite: %w", err)
	}
	if os.SameFile(configInfo, logsInfo) {
		return Snapshots{}, fmt.Errorf("config and logs sqlite paths must be different files")
	}

	snapshotDir, err = filepath.Abs(snapshotDir)
	if err != nil {
		return Snapshots{}, fmt.Errorf("resolve snapshot directory: %w", err)
	}
	if err := os.Mkdir(snapshotDir, 0o700); err != nil {
		if os.IsExist(err) {
			return Snapshots{}, fmt.Errorf("snapshot directory already exists: %s", snapshotDir)
		}
		return Snapshots{}, fmt.Errorf("create snapshot directory: %w", err)
	}

	result := Snapshots{
		ConfigPath: filepath.Join(snapshotDir, configSnapshotName),
		LogsPath:   filepath.Join(snapshotDir, logsSnapshotName),
	}
	if err := backupSQLite(ctx, configPath, result.ConfigPath); err != nil {
		return result, fmt.Errorf("snapshot config sqlite: %w (retained directory: %s)", err, snapshotDir)
	}
	if err := backupSQLite(ctx, logsPath, result.LogsPath); err != nil {
		return result, fmt.Errorf("snapshot logs sqlite: %w (retained directory: %s)", err, snapshotDir)
	}
	if result.ConfigSHA256, err = fileSHA256(result.ConfigPath); err != nil {
		return result, fmt.Errorf("hash config snapshot: %w", err)
	}
	if result.LogsSHA256, err = fileSHA256(result.LogsPath); err != nil {
		return result, fmt.Errorf("hash logs snapshot: %w", err)
	}
	return result, nil
}

// OpenSnapshots resolves the retained snapshots in snapshotDir and verifies
// they are regular, distinct files before post-migration verification.
func OpenSnapshots(snapshotDir string) (Snapshots, error) {
	snapshotDir, err := filepath.Abs(snapshotDir)
	if err != nil {
		return Snapshots{}, fmt.Errorf("resolve snapshot directory: %w", err)
	}
	configPath, configInfo, err := validateSourcePath(filepath.Join(snapshotDir, configSnapshotName))
	if err != nil {
		return Snapshots{}, fmt.Errorf("config snapshot: %w", err)
	}
	logsPath, logsInfo, err := validateSourcePath(filepath.Join(snapshotDir, logsSnapshotName))
	if err != nil {
		return Snapshots{}, fmt.Errorf("logs snapshot: %w", err)
	}
	if os.SameFile(configInfo, logsInfo) {
		return Snapshots{}, fmt.Errorf("config and logs snapshots must be different files")
	}
	configHash, err := fileSHA256(configPath)
	if err != nil {
		return Snapshots{}, fmt.Errorf("hash config snapshot: %w", err)
	}
	logsHash, err := fileSHA256(logsPath)
	if err != nil {
		return Snapshots{}, fmt.Errorf("hash logs snapshot: %w", err)
	}
	return Snapshots{
		ConfigPath:   configPath,
		LogsPath:     logsPath,
		ConfigSHA256: configHash,
		LogsSHA256:   logsHash,
	}, nil
}

func validateSourcePath(path string) (string, os.FileInfo, error) {
	if path == "" {
		return "", nil, fmt.Errorf("path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve path: %w", err)
	}
	linkInfo, err := os.Lstat(absPath)
	if err != nil {
		return "", nil, err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("symlinks are not accepted: %s", absPath)
	}
	if !linkInfo.Mode().IsRegular() {
		return "", nil, fmt.Errorf("not a regular file: %s", absPath)
	}
	return absPath, linkInfo, nil
}

type sqliteBackuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

func backupSQLite(ctx context.Context, sourcePath, destinationPath string) error {
	db, err := openSQLite(sourcePath, false)
	if err != nil {
		return err
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open source connection: %w", err)
	}
	defer conn.Close()

	err = conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(sqliteBackuper)
		if !ok {
			return fmt.Errorf("sqlite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(destinationPath)
		if err != nil {
			return err
		}
		more, stepErr := backup.Step(-1)
		finishErr := backup.Finish()
		if stepErr != nil {
			return stepErr
		}
		if more {
			return fmt.Errorf("sqlite backup stopped before all pages were copied")
		}
		return finishErr
	})
	if err != nil {
		return err
	}
	if err := os.Chmod(destinationPath, 0o600); err != nil {
		return fmt.Errorf("set snapshot permissions: %w", err)
	}
	f, err := os.OpenFile(destinationPath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open snapshot for sync: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync snapshot: %w", err)
	}

	snapshot, err := openSQLite(destinationPath, true)
	if err != nil {
		return err
	}
	defer snapshot.Close()
	if err := checkSQLite(ctx, snapshot); err != nil {
		return err
	}
	return nil
}

func openSQLite(path string, immutable bool) (*sql.DB, error) {
	u := &url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(60000)")
	if immutable {
		query.Set("immutable", "1")
	}
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return db, nil
}

func checkSQLite(ctx context.Context, q queryer) error {
	var result string
	if err := q.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("sqlite quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("sqlite quick_check failed: %s", result)
	}
	rows, err := q.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("sqlite foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID any
		var parent string
		var fkID int
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return fmt.Errorf("read sqlite foreign key violation: %w", err)
		}
		return fmt.Errorf("sqlite foreign key violation: table=%s rowid=%v parent=%s foreign_key=%d", table, rowID, parent, fkID)
	}
	return rows.Err()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
