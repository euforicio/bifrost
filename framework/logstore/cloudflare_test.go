//go:build cloudflare

package logstore

import (
	"bytes"
	"context"
	"encoding/json"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestCloudflareNativeMigrations(t *testing.T) {
	if os.Getenv("BIFROST_CLOUDFLARE_LOGS_SQL_URL") == "" {
		t.Skip("requires real Cloudflare SQLite authority")
	}
	action := os.Getenv("CLOUDFLARE_ACCEPTANCE_ACTION")
	if action != "initialize" && action != "verify" && action != "production" && action != "production-local" && action != "ping-fence" {
		t.Fatal("explicit Cloudflare qualification or read-only production action required")
	}
	ctx := context.Background()
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	s, err := NewLogStore(ctx, &Config{Enabled: true, Type: LogStoreTypeSQLite, Config: &SQLiteConfig{Path: "cloudflare://logs"}}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if err = s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if action == "ping-fence" {
		body, _ := json.Marshal(map[string]string{"capability": strings.Repeat("a", 64)})
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, os.Getenv("BIFROST_CLOUDFLARE_LOGS_SQL_URL")+"/__activate", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Storage-Capability", os.Getenv("SQLITE_STORAGE_TOKEN"))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatal("Native fencing rejected")
		}
		t.Log("Fenced native ping must terminate")
		_ = s.Ping(ctx)
		t.Fatal("Native ping returned after fencing")
	}
	db := s.(*RDBLogStore).ScopedDB(ctx)
	if os.Getenv("CLOUDFLARE_ACCEPTANCE_ACTION") == "production" {
		var count int64
		if err = db.Raw("SELECT COUNT(*) FROM logs").Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		var integrity string
		if err = db.Raw("PRAGMA quick_check").Scan(&integrity).Error; err != nil || integrity != "ok" {
			t.Fatal("production log integrity failure")
		}
		t.Logf("%d existing Bifrost log records retained", count)
		return
	}
	if err = db.Exec("CREATE TABLE IF NOT EXISTS cloudflare_acceptance(id INTEGER PRIMARY KEY, value BLOB NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CLOUDFLARE_ACCEPTANCE_ACTION") == "initialize" {
		tx := db.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		if err = tx.Exec("INSERT INTO cloudflare_acceptance VALUES(1,?)", []byte{0, 1, 255}).Error; err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit().Error; err != nil {
			t.Fatal(err)
		}
		tx = db.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		if err = tx.Exec("INSERT INTO cloudflare_acceptance VALUES(2,?)", []byte{}).Error; err != nil {
			t.Fatal(err)
		}
		if err = tx.Rollback().Error; err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err = db.Raw("SELECT COUNT(*) FROM cloudflare_acceptance WHERE id=1 AND hex(value)='0001FF'").Scan(&rows).Error; err != nil || rows != 1 {
		t.Fatalf("durable commit: %d, %v", rows, err)
	}
	if err = db.Raw("SELECT COUNT(*) FROM cloudflare_acceptance WHERE id=2").Scan(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("rollback: %d, %v", rows, err)
	}
	var integrity string
	if err = db.Raw("PRAGMA quick_check").Scan(&integrity).Error; err != nil || integrity != "ok" {
		t.Fatalf("integrity: %s, %v", integrity, err)
	}
	var failures []map[string]interface{}
	if err = db.Raw("PRAGMA foreign_key_check").Scan(&failures).Error; err != nil || len(failures) != 0 {
		t.Fatalf("foreign keys: %d, %v", len(failures), err)
	}
	var count int
	if err = db.Raw("SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&count).Error; err != nil || count < 2 {
		t.Fatalf("schema: %d, %v", count, err)
	}
	t.Logf("%d actual Bifrost tables and native commit/rollback/restart checks passed", count)
}
