package configstore

import (
	"context"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func openProviderCredentialTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	require.NoError(t, err)
	return db
}

func TestMigrationAddProviderCredentialsTable(t *testing.T) {
	db := openProviderCredentialTestDB(t)
	ctx := context.Background()
	logger := bifrost.NewDefaultLogger(schemas.LogLevelInfo)

	require.NoError(t, migrationAddProviderCredentialsTable(ctx, db, logger))
	assert.True(t, db.Migrator().HasTable(&tables.TableProviderCredential{}))
	assert.True(t, db.Migrator().HasIndex(&tables.TableProviderCredential{}, "idx_provider_credentials_provider_key_id"))
	assert.True(t, db.Migrator().HasIndex(&tables.TableProviderCredential{}, "idx_provider_credentials_provider_auth_mode"))
	assert.True(t, db.Migrator().HasIndex(&tables.TableProviderCredential{}, "idx_provider_credentials_account_id"))
	assert.True(t, db.Migrator().HasIndex(&tables.TableProviderCredential{}, "idx_provider_credentials_status_expires_at"))
	assert.True(t, db.Migrator().HasIndex(&tables.TableProviderCredential{}, "idx_provider_credentials_refresh_lease_expires_at"))
	assert.True(t, db.Migrator().HasColumn(&tables.TableProviderCredential{}, "refresh_lease_owner"))
	assert.True(t, db.Migrator().HasColumn(&tables.TableProviderCredential{}, "refresh_lease_expires_at"))

	credential := tables.TableProviderCredential{
		CredentialID:  "credential-1",
		Provider:      "openai",
		ProviderKeyID: "provider-key-1",
		AuthMode:      "oauth",
	}
	require.NoError(t, db.Create(&credential).Error)
	assert.EqualValues(t, 1, credential.Version)
	assert.Equal(t, "active", credential.Status)
}

func TestMigrationAddProviderCredentialRefreshLeaseColumnsUpgradesExistingTable(t *testing.T) {
	db := openProviderCredentialTestDB(t)
	require.NoError(t, db.Exec(`CREATE TABLE provider_credentials (credential_id TEXT PRIMARY KEY)`).Error)

	require.NoError(t, migrationAddProviderCredentialRefreshLeaseColumns(
		context.Background(), db, bifrost.NewDefaultLogger(schemas.LogLevelInfo),
	))
	assert.True(t, db.Migrator().HasColumn(&tables.TableProviderCredential{}, "refresh_lease_owner"))
	assert.True(t, db.Migrator().HasColumn(&tables.TableProviderCredential{}, "refresh_lease_expires_at"))
	assert.True(t, db.Migrator().HasIndex(&tables.TableProviderCredential{}, "idx_provider_credentials_refresh_lease_expires_at"))
}

func TestEncryptPlaintextProviderCredentials(t *testing.T) {
	db := openProviderCredentialTestDB(t)
	require.NoError(t, db.AutoMigrate(&tables.TableProviderCredential{}))
	now := time.Now().UTC()
	require.NoError(t, db.Exec(
		`INSERT INTO provider_credentials
		 (credential_id, provider, provider_key_id, auth_mode, access_token, refresh_token, id_token, status, version, encryption_status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"credential-1", "openai", "provider-key-1", "oauth", "access-secret", "refresh-secret", "id-secret", "active", 1, "plain_text", now, now,
	).Error)

	store := &RDBConfigStore{logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
	store.db.Store(db)
	count, err := store.encryptPlaintextProviderCredentials(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	var raw map[string]any
	require.NoError(t, db.Table("provider_credentials").Where("credential_id = ?", "credential-1").Take(&raw).Error)
	assert.Equal(t, "encrypted", raw["encryption_status"])
	assert.NotEqual(t, "access-secret", raw["access_token"])
	assert.NotEqual(t, "refresh-secret", raw["refresh_token"])
	assert.NotEqual(t, "id-secret", raw["id_token"])

	var found tables.TableProviderCredential
	require.NoError(t, db.First(&found, "credential_id = ?", "credential-1").Error)
	assert.Equal(t, "access-secret", found.AccessToken)
	assert.Equal(t, "refresh-secret", found.RefreshToken)
	assert.Equal(t, "id-secret", found.IDToken)
}
