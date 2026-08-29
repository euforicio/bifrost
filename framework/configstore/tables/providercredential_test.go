package tables

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupProviderCredentialTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&TableProviderCredential{}))
	return db
}

func TestTableProviderCredentialEncryptsTokensAtRest(t *testing.T) {
	db := setupProviderCredentialTestDB(t)
	expiresAt := time.Now().UTC().Add(time.Hour)
	lastRefresh := time.Now().UTC()
	credential := &TableProviderCredential{
		CredentialID:  "credential-1",
		Provider:      "openai",
		ProviderKeyID: "provider-key-1",
		AuthMode:      "oauth",
		AccessToken:   "access-secret",
		RefreshToken:  "refresh-secret",
		IDToken:       "id-secret",
		AccountID:     "account-1",
		ExpiresAt:     &expiresAt,
		LastRefresh:   &lastRefresh,
	}

	require.NoError(t, db.Create(credential).Error)

	var raw map[string]any
	require.NoError(t, db.Table("provider_credentials").Where("credential_id = ?", credential.CredentialID).Take(&raw).Error)
	assert.Equal(t, EncryptionStatusEncrypted, raw["encryption_status"])
	assert.NotEqual(t, "access-secret", raw["access_token"])
	assert.NotEqual(t, "refresh-secret", raw["refresh_token"])
	assert.NotEqual(t, "id-secret", raw["id_token"])

	var found TableProviderCredential
	require.NoError(t, db.First(&found, "credential_id = ?", credential.CredentialID).Error)
	assert.Equal(t, "access-secret", found.AccessToken)
	assert.Equal(t, "refresh-secret", found.RefreshToken)
	assert.Equal(t, "id-secret", found.IDToken)
	assert.Equal(t, "active", found.Status)
	assert.EqualValues(t, 1, found.Version)
}

func TestTableProviderCredentialJSONRedactsTokens(t *testing.T) {
	leaseExpiresAt := time.Now().UTC().Add(time.Minute)
	credential := TableProviderCredential{
		CredentialID:          "credential-1",
		Provider:              "openai",
		ProviderKeyID:         "provider-key-1",
		AuthMode:              "oauth",
		AccessToken:           "access-secret",
		RefreshToken:          "refresh-secret",
		IDToken:               "id-secret",
		Status:                "active",
		Version:               3,
		RefreshLeaseOwner:     "node-secret",
		RefreshLeaseExpiresAt: &leaseExpiresAt,
	}

	data, err := json.Marshal(credential)
	require.NoError(t, err)
	jsonValue := string(data)
	assert.NotContains(t, jsonValue, "access_token")
	assert.NotContains(t, jsonValue, "refresh_token")
	assert.NotContains(t, jsonValue, "id_token")
	assert.NotContains(t, jsonValue, "access-secret")
	assert.NotContains(t, jsonValue, "refresh-secret")
	assert.NotContains(t, jsonValue, "id-secret")
	assert.NotContains(t, jsonValue, "refresh_lease_owner")
	assert.NotContains(t, jsonValue, "refresh_lease_expires_at")
	assert.NotContains(t, jsonValue, "node-secret")
	assert.Contains(t, jsonValue, `"credential_id":"credential-1"`)
	assert.Contains(t, jsonValue, `"version":3`)
}
