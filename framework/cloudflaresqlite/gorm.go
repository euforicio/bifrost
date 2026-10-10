//go:build cloudflare

package cloudflaresqlite

import (
	"errors"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"os"
	"time"
)

// Fixed UTC text preserves nanoseconds and SQLite lexical range/cursor order.
const TimestampFormat = "2006-01-02 15:04:05.000000000"

type utcDialector struct {
	gorm.Dialector
	release func()
}

func (d utcDialector) Release() { d.release() }

func (d utcDialector) SavePoint(db *gorm.DB, name string) error {
	return d.Dialector.(gorm.SavePointerDialectorInterface).SavePoint(db, name)
}
func (d utcDialector) RollbackTo(db *gorm.DB, name string) error {
	return d.Dialector.(gorm.SavePointerDialectorInterface).RollbackTo(db, name)
}

func (d utcDialector) Initialize(db *gorm.DB) error {
	db.NowFunc = func() time.Time { return time.Now().UTC() }
	return d.Dialector.Initialize(db)
}

func (d utcDialector) BindVarTo(writer clause.Writer, statement *gorm.Statement, value interface{}) {
	if len(statement.Vars) > 0 {
		switch date := value.(type) {
		case time.Time:
			statement.Vars[len(statement.Vars)-1] = date.UTC().Format(TimestampFormat)
		case *time.Time:
			if date != nil {
				statement.Vars[len(statement.Vars)-1] = date.UTC().Format(TimestampFormat)
			}
		}
	}
	d.Dialector.BindVarTo(writer, statement, value)
}

// Dialector preserves GORM's SQLite transactions and migration semantics. The
// named authority is reachable only through the Container's outbound binding.
func Dialector(authority string) (gorm.Dialector, error) {
	var endpoint string
	switch authority {
	case "cloudflare://config":
		endpoint = os.Getenv("BIFROST_CLOUDFLARE_CONFIG_SQL_URL")
	case "cloudflare://logs":
		endpoint = os.Getenv("BIFROST_CLOUDFLARE_LOGS_SQL_URL")
	default:
		return nil, errors.New("invalid Cloudflare SQLite authority")
	}
	db, release, err := OpenWithOptions(endpoint, os.Getenv("SQLITE_STORAGE_TOKEN"), Options{
		GenerationCapability: os.Getenv("SQLITE_STORAGE_GENERATION_CAPABILITY"),
		OnFailure:            func() { os.Exit(1) },
		TimeFormat:           TimestampFormat,
	})
	if err != nil {
		return nil, err
	}
	return utcDialector{Dialector: sqlite.New(sqlite.Config{Conn: db}), release: release}, nil
}
