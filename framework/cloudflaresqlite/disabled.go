//go:build !cloudflare

package cloudflaresqlite

import (
	"errors"
	"gorm.io/gorm"
)

// Dialector requires the explicitly enabled Cloudflare storage build. Ordinary
// builds retain their existing local SQLite driver and storage behavior.
func Dialector(authority string) (gorm.Dialector, error) {
	return nil, errors.New("Cloudflare SQLite requires a cloudflare build")
}
