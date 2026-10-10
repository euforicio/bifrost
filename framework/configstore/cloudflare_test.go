//go:build cloudflare

package configstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/cloudflaresqlite"
	"github.com/maximhq/bifrost/framework/encrypt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestCloudflareNativeMigrations(t *testing.T) {
	if os.Getenv("BIFROST_CLOUDFLARE_CONFIG_SQL_URL") == "" && os.Getenv("BIFROST_LOCAL_CONFIG_SQLITE") == "" {
		t.Skip("requires real Cloudflare SQLite authority")
	}
	action := os.Getenv("CLOUDFLARE_ACCEPTANCE_ACTION")
	if action != "initialize" && action != "verify" && action != "production" && action != "production-local" && action != "ping-fence" {
		t.Fatal("explicit Cloudflare qualification or read-only production action required")
	}
	ctx := context.Background()
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	path := "cloudflare://config"
	if local := os.Getenv("BIFROST_LOCAL_CONFIG_SQLITE"); local != "" {
		path = local
	}
	production := os.Getenv("CLOUDFLARE_ACCEPTANCE_ACTION") == "production" || os.Getenv("CLOUDFLARE_ACCEPTANCE_ACTION") == "production-local"
	if production {
		if os.Getenv("BIFROST_ENCRYPTION_KEY") == "" {
			t.Fatal("existing Bifrost encryption key required")
		}
		encrypt.Init(os.Getenv("BIFROST_ENCRYPTION_KEY"), logger)
	}
	s, err := NewConfigStore(ctx, &Config{Enabled: true, Type: ConfigStoreTypeSQLite, Config: &SQLiteConfig{Path: path}}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if err = s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if action == "ping-fence" {
		body, _ := json.Marshal(map[string]string{"capability": strings.Repeat("a", 64)})
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, os.Getenv("BIFROST_CLOUDFLARE_CONFIG_SQL_URL")+"/__activate", bytes.NewReader(body))
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
		// Fencing schedules process termination on another goroutine.
		// Give that exit time to run before reporting a surviving process.
		time.Sleep(time.Second)
		t.Fatal("Native ping returned after fencing")
	}
	db := s.(*RDBConfigStore).ScopedDB(ctx)
	if production {
		providers, err := s.GetProvidersConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for name := range providers {
			names = append(names, string(name))
		}
		sort.Strings(names)
		hash := sha256.New()
		keys := 0
		for _, name := range names {
			provider := providers[schemas.ModelProvider(name)]
			payload, err := json.Marshal(provider)
			if err != nil {
				t.Fatal(err)
			}
			hash.Write(payload)
			for _, key := range provider.Keys {
				keys++
				hash.Write([]byte(key.Value.GetValue()))
			}
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if expected := os.Getenv("BIFROST_EXPECTED_CREDENTIAL_FINGERPRINT"); expected != "" && expected != digest {
			t.Fatal("Bifrost migrated credential fingerprint mismatch")
		}
		t.Logf("%d provider configurations and %d keys decrypted; credential fingerprint %s", len(names), keys, digest)
		var integrity string
		if err = db.Raw("PRAGMA quick_check").Scan(&integrity).Error; err != nil || integrity != "ok" {
			t.Fatal("production SQLite integrity failure")
		}
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
	if action == "initialize" {
		if err = db.Exec("CREATE TABLE cloudflare_acceptance_dates(id INTEGER PRIMARY KEY, created_at DATETIME NOT NULL)").Error; err != nil {
			t.Fatal(err)
		}
		// One converted timestamp and one new write from a non-UTC caller.
		if err = db.Exec("INSERT INTO cloudflare_acceptance_dates VALUES(1,?)", "2030-01-02 03:04:05.000000123").Error; err != nil {
			t.Fatal(err)
		}
		date := time.Date(2030, 1, 2, 5, 4, 5, 124, time.FixedZone("offset", 2*60*60))
		if err = db.Exec("INSERT INTO cloudflare_acceptance_dates VALUES(2,?)", date).Error; err != nil {
			t.Fatal(err)
		}
	}
	var timestamps []struct {
		ID        int
		CreatedAt time.Time
	}
	if err = db.Raw("SELECT id,created_at FROM cloudflare_acceptance_dates ORDER BY created_at,id").Scan(&timestamps).Error; err != nil || len(timestamps) != 2 {
		t.Fatalf("timestamp restore: %v", err)
	}
	if timestamps[0].CreatedAt.Nanosecond() != 123 || timestamps[1].CreatedAt.Nanosecond() != 124 || !timestamps[1].CreatedAt.After(timestamps[0].CreatedAt) {
		t.Fatal("timestamp precision or ordering changed")
	}
	var window int
	lower := time.Date(2030, 1, 2, 3, 4, 5, 123, time.UTC)
	if err = db.Raw("SELECT COUNT(*) FROM cloudflare_acceptance_dates WHERE created_at >= ? AND created_at < ?", lower, lower.Add(time.Nanosecond)).Scan(&window).Error; err != nil || window != 1 {
		t.Fatalf("mixed migrated/new cursor bounds: %d, %v", window, err)
	}
	var raw string
	if err = db.Raw("SELECT CAST(created_at AS TEXT) FROM cloudflare_acceptance_dates WHERE id=2").Scan(&raw).Error; err != nil || raw != timestamps[1].CreatedAt.UTC().Format(cloudflaresqlite.TimestampFormat) {
		t.Fatal("new timestamp must use fixed UTC storage format")
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
