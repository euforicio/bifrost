package tables

import (
	"fmt"
	"time"

	"github.com/maximhq/bifrost/framework/encrypt"
	"gorm.io/gorm"
)

// TableProviderCredential stores a provider-issued credential independently
// from the provider key configuration that uses it. Token fields are encrypted
// at rest and are intentionally excluded from JSON serialization.
type TableProviderCredential struct {
	CredentialID          string     `gorm:"column:credential_id;type:varchar(255);primaryKey" json:"credential_id"`
	Provider              string     `gorm:"type:varchar(50);not null;index:idx_provider_credentials_provider_auth_mode" json:"provider"`
	ProviderKeyID         string     `gorm:"type:varchar(255);not null;index:idx_provider_credentials_provider_key_id" json:"provider_key_id"`
	AuthMode              string     `gorm:"type:varchar(50);not null;index:idx_provider_credentials_provider_auth_mode" json:"auth_mode"`
	AccessToken           string     `gorm:"type:text" json:"-"`
	RefreshToken          string     `gorm:"type:text" json:"-"`
	IDToken               string     `gorm:"type:text" json:"-"`
	AccountID             string     `gorm:"type:varchar(255);index:idx_provider_credentials_account_id" json:"account_id,omitempty"`
	ExpiresAt             *time.Time `gorm:"index:idx_provider_credentials_status_expires_at" json:"expires_at,omitempty"`
	LastRefresh           *time.Time `json:"last_refresh,omitempty"`
	Status                string     `gorm:"type:varchar(50);not null;default:'active';index:idx_provider_credentials_status_expires_at" json:"status"`
	Version               uint64     `gorm:"not null;default:1" json:"version"`
	RefreshLeaseOwner     string     `gorm:"type:varchar(255)" json:"-"`
	RefreshLeaseExpiresAt *time.Time `gorm:"index:idx_provider_credentials_refresh_lease_expires_at" json:"-"`
	CreatedAt             time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt             time.Time  `gorm:"not null" json:"updated_at"`
	EncryptionStatus      string     `gorm:"type:varchar(20);not null;default:'plain_text'" json:"-"`
}

// TableName returns the provider credential table name.
func (TableProviderCredential) TableName() string { return "provider_credentials" }

// BeforeSave encrypts provider-issued tokens before they are persisted.
func (c *TableProviderCredential) BeforeSave(tx *gorm.DB) error {
	if c.Status == "" {
		c.Status = "active"
	}
	if c.Version == 0 {
		c.Version = 1
	}
	if !encrypt.IsEnabled() {
		return nil
	}
	if err := encryptString(&c.AccessToken); err != nil {
		return fmt.Errorf("failed to encrypt provider access token: %w", err)
	}
	if err := encryptString(&c.RefreshToken); err != nil {
		return fmt.Errorf("failed to encrypt provider refresh token: %w", err)
	}
	if err := encryptString(&c.IDToken); err != nil {
		return fmt.Errorf("failed to encrypt provider id token: %w", err)
	}
	c.EncryptionStatus = EncryptionStatusEncrypted
	return nil
}

// AfterFind decrypts provider-issued tokens after a row is loaded.
func (c *TableProviderCredential) AfterFind(tx *gorm.DB) error {
	if c.EncryptionStatus != EncryptionStatusEncrypted {
		return nil
	}
	if err := decryptString(&c.AccessToken); err != nil {
		return fmt.Errorf("failed to decrypt provider access token: %w", err)
	}
	if err := decryptString(&c.RefreshToken); err != nil {
		return fmt.Errorf("failed to decrypt provider refresh token: %w", err)
	}
	if err := decryptString(&c.IDToken); err != nil {
		return fmt.Errorf("failed to decrypt provider id token: %w", err)
	}
	return nil
}
