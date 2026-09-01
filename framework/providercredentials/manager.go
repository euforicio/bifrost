// Package providercredentials manages subscription-backed provider credentials.
package providercredentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

const (
	ProviderOpenAICodex schemas.ModelProvider = "openai-codex"
	ProviderXAI         schemas.ModelProvider = "xai"
	ProviderCursor      schemas.ModelProvider = "cursor"

	StatusConnected    = "connected"
	StatusConnecting   = "connecting"
	StatusExpired      = "expired"
	StatusDisconnected = "disconnected"
	StatusError        = "error"
)

const (
	openAIClientID  = "app_EMoamEEZ73f0CkXaXp7hrann"
	xAIClientID     = "b1a00492-073a-47ea-816f-4c329264a828"
	xAIScopes       = "openid profile email offline_access grok-cli:access api:access"
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	defaultAttemptRetention    = 10 * time.Minute
	defaultMaxTerminalAttempts = 256
	refreshLeaseDuration       = 45 * time.Second
	refreshLeasePollInterval   = 100 * time.Millisecond
)

var (
	ErrCredentialNotFound = errors.New("provider credential not found")
	errCredentialChanged  = errors.New("credential changed while operation was in progress")
)

type endpoints struct {
	openAIIssuer   string
	openAIUsageAPI string
	xAIDeviceURL   string
	xAITokenURL    string
	cursorLoginURL string
	cursorAPIBase  string
}

type Option func(*Manager)

// WithHTTPClient replaces the OAuth protocol client. It is intended for local
// protocol fixtures and does not change production endpoint defaults.
func WithHTTPClient(client *http.Client) Option {
	return func(m *Manager) {
		if client != nil {
			m.httpClient = client
		}
	}
}

// WithEndpoints replaces OAuth endpoints for local protocol fixtures.
func WithEndpoints(openAIIssuer, xAIDeviceURL, xAITokenURL string) Option {
	return func(m *Manager) {
		if strings.TrimSpace(openAIIssuer) != "" {
			m.endpoints.openAIIssuer = strings.TrimRight(openAIIssuer, "/")
		}
		if strings.TrimSpace(xAIDeviceURL) != "" {
			m.endpoints.xAIDeviceURL = xAIDeviceURL
		}
		if strings.TrimSpace(xAITokenURL) != "" {
			m.endpoints.xAITokenURL = xAITokenURL
		}
	}
}

// WithCursorEndpoints replaces Cursor's browser-login and API endpoints for
// local protocol fixtures.
func WithCursorEndpoints(loginURL, apiBaseURL string) Option {
	return func(m *Manager) {
		if strings.TrimSpace(loginURL) != "" {
			m.endpoints.cursorLoginURL = strings.TrimRight(loginURL, "/")
		}
		if strings.TrimSpace(apiBaseURL) != "" {
			m.endpoints.cursorAPIBase = strings.TrimRight(apiBaseURL, "/")
		}
	}
}

// WithCursorPollInterval reduces the first Cursor poll delay for local
// protocol fixtures. Production uses the provider-compatible one-second
// default.
func WithCursorPollInterval(interval time.Duration) Option {
	return func(m *Manager) {
		if interval > 0 {
			m.cursorPollInterval = interval
		}
	}
}

// WithUsageEndpoints replaces provider usage endpoints for local protocol
// fixtures. Empty values preserve production defaults.
func WithUsageEndpoints(openAIBaseURL, cursorAPIBaseURL string) Option {
	return func(m *Manager) {
		if strings.TrimSpace(openAIBaseURL) != "" {
			m.endpoints.openAIUsageAPI = strings.TrimRight(openAIBaseURL, "/")
		}
		if strings.TrimSpace(cursorAPIBaseURL) != "" {
			m.endpoints.cursorAPIBase = strings.TrimRight(cursorAPIBaseURL, "/")
		}
	}
}

// WithNow replaces the manager clock for deterministic local protocol tests.
func WithNow(now func() time.Time) Option {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

type Manager struct {
	store               Store
	httpClient          *http.Client
	endpoints           endpoints
	now                 func() time.Time
	cursorPollInterval  time.Duration
	attemptRetention    time.Duration
	maxTerminalAttempts int

	mu       sync.Mutex
	attempts map[string]*loginAttempt
	locks    map[string]*credentialLockEntry
}

type tokenRefreshHTTPError struct {
	status int
	label  string
}

func (e *tokenRefreshHTTPError) Error() string {
	return fmt.Sprintf("%s failed with status %d", e.label, e.status)
}

func isTerminalRefreshError(err error) bool {
	var statusErr *tokenRefreshHTTPError
	return errors.As(err, &statusErr) && statusErr.status >= 400 && statusErr.status < 500 && statusErr.status != http.StatusTooManyRequests
}

type credentialLockEntry struct {
	mu   sync.Mutex
	refs int
}

// Store is the narrow persistence surface required by the credential manager.
type Store interface {
	DB() *gorm.DB
}

func NewManager(store Store, opts ...Option) *Manager {
	m := &Manager{
		store:      store,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		endpoints: endpoints{
			openAIIssuer:   "https://auth.openai.com",
			openAIUsageAPI: "https://chatgpt.com/backend-api",
			xAIDeviceURL:   "https://auth.x.ai/oauth2/device/code",
			xAITokenURL:    "https://auth.x.ai/oauth2/token",
			cursorLoginURL: "https://cursor.com/loginDeepControl",
			cursorAPIBase:  "https://api2.cursor.sh",
		},
		now:                 time.Now,
		cursorPollInterval:  time.Second,
		attemptRetention:    defaultAttemptRetention,
		maxTerminalAttempts: defaultMaxTerminalAttempts,
		attempts:            make(map[string]*loginAttempt),
		locks:               make(map[string]*credentialLockEntry),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

type CredentialStatus struct {
	CredentialID string     `json:"credential_id"`
	Provider     string     `json:"provider"`
	AccountID    string     `json:"account_id,omitempty"`
	Status       string     `json:"status"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	LastRefresh  *time.Time `json:"last_refresh,omitempty"`
	Version      uint64     `json:"version"`
}

func (m *Manager) List(ctx context.Context, provider schemas.ModelProvider) ([]CredentialStatus, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("provider credential storage is unavailable")
	}
	var rows []tables.TableProviderCredential
	if err := m.store.DB().WithContext(ctx).Where("provider = ?", string(provider)).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	statuses := make([]CredentialStatus, 0, len(rows))
	for i := range rows {
		statuses = append(statuses, publicStatus(&rows[i], m.now()))
	}
	return statuses, nil
}

type LoginStatus struct {
	LoginID            string     `json:"login_id"`
	CredentialID       string     `json:"credential_id"`
	Provider           string     `json:"provider"`
	Status             string     `json:"status"`
	VerificationURL    string     `json:"verification_url,omitempty"`
	UserCode           string     `json:"user_code,omitempty"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	PollIntervalSecond int        `json:"poll_interval_seconds,omitempty"`
	ErrorCode          string     `json:"error_code,omitempty"`
}

type loginAttempt struct {
	status     LoginStatus
	cancel     context.CancelFunc
	terminalAt time.Time
}

func (m *Manager) Status(ctx context.Context, provider schemas.ModelProvider, credentialID string) (CredentialStatus, error) {
	row, err := m.load(ctx, provider, credentialID)
	if err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			return CredentialStatus{CredentialID: credentialID, Provider: string(provider), Status: StatusDisconnected}, nil
		}
		return CredentialStatus{}, err
	}
	return publicStatus(row, m.now()), nil
}

func publicStatus(row *tables.TableProviderCredential, now time.Time) CredentialStatus {
	status := row.Status
	if row.ExpiresAt != nil && !row.ExpiresAt.After(now) && status == StatusConnected {
		status = StatusExpired
	}
	return CredentialStatus{
		CredentialID: row.CredentialID,
		Provider:     row.Provider,
		AccountID:    row.AccountID,
		Status:       status,
		ExpiresAt:    row.ExpiresAt,
		LastRefresh:  row.LastRefresh,
		Version:      row.Version,
	}
}

func (m *Manager) LoginStatus(loginID string) (LoginStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupTerminalAttemptsLocked(m.now())
	attempt := m.attempts[loginID]
	if attempt == nil {
		return LoginStatus{}, errors.New("provider login not found")
	}
	return attempt.status, nil
}

func (m *Manager) CancelLogin(loginID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupTerminalAttemptsLocked(m.now())
	attempt := m.attempts[loginID]
	if attempt == nil {
		return errors.New("provider login not found")
	}
	attempt.cancel()
	attempt.status.Status = StatusDisconnected
	attempt.status.ErrorCode = "cancelled"
	attempt.terminalAt = m.now()
	m.cleanupTerminalAttemptsLocked(attempt.terminalAt)
	return nil
}

func (m *Manager) Disconnect(ctx context.Context, provider schemas.ModelProvider, credentialID string) error {
	m.CancelCredentialLogins(provider, credentialID)
	result := m.store.DB().WithContext(ctx).
		Where("provider = ? AND credential_id = ?", string(provider), credentialID).
		Delete(&tables.TableProviderCredential{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrCredentialNotFound
	}
	return nil
}

// DisconnectProvider removes every account credential owned by a provider.
// Provider deletion uses this to avoid retaining orphaned refresh tokens.
func (m *Manager) DisconnectProvider(ctx context.Context, provider schemas.ModelProvider) error {
	m.CancelProviderLogins(provider)
	return m.store.DB().WithContext(ctx).Where("provider = ?", string(provider)).Delete(&tables.TableProviderCredential{}).Error
}

// CancelProviderLogins cancels active login attempts for a provider without
// changing its persisted credentials. Use this only after the caller's own
// durable transaction has committed.
func (m *Manager) CancelProviderLogins(provider schemas.ModelProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.cleanupTerminalAttemptsLocked(now)
	for _, attempt := range m.attempts {
		if attempt.status.Provider == string(provider) && attempt.status.Status == StatusConnecting {
			attempt.cancel()
			attempt.status.Status = StatusDisconnected
			attempt.status.ErrorCode = "cancelled"
			attempt.terminalAt = now
		}
	}
	m.cleanupTerminalAttemptsLocked(now)
}

// CancelCredentialLogins cancels active login attempts for one credential
// without changing its persisted credential row.
func (m *Manager) CancelCredentialLogins(provider schemas.ModelProvider, credentialID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.cleanupTerminalAttemptsLocked(now)
	for _, attempt := range m.attempts {
		if attempt.status.Provider == string(provider) && attempt.status.CredentialID == credentialID && attempt.status.Status == StatusConnecting {
			attempt.cancel()
			attempt.status.Status = StatusDisconnected
			attempt.status.ErrorCode = "cancelled"
			attempt.terminalAt = now
		}
	}
	m.cleanupTerminalAttemptsLocked(now)
}

func (m *Manager) lockCredential(provider schemas.ModelProvider, credentialID string) func() {
	key := string(provider) + ":" + credentialID
	m.mu.Lock()
	entry := m.locks[key]
	if entry == nil {
		entry = &credentialLockEntry{}
		m.locks[key] = entry
	}
	entry.refs++
	m.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		m.mu.Lock()
		entry.refs--
		if entry.refs == 0 && m.locks[key] == entry {
			delete(m.locks, key)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) ResolveProviderCredential(ctx context.Context, provider schemas.ModelProvider, credentialID string, forceRefresh bool) (schemas.ResolvedProviderCredential, error) {
	return m.resolveProviderCredential(ctx, provider, credentialID, forceRefresh, true)
}

// resolveProviderCredentialForUsage resolves and, when necessary, refreshes a
// credential without allowing a telemetry failure to change inference status.
// Successful token rotation is still persisted and shared across processes.
func (m *Manager) resolveProviderCredentialForUsage(ctx context.Context, provider schemas.ModelProvider, credentialID string, forceRefresh bool) (schemas.ResolvedProviderCredential, error) {
	return m.resolveProviderCredential(ctx, provider, credentialID, forceRefresh, false)
}

func (m *Manager) resolveProviderCredential(ctx context.Context, provider schemas.ModelProvider, credentialID string, forceRefresh, expireOnRefreshFailure bool) (schemas.ResolvedProviderCredential, error) {
	observedVersion := uint64(0)
	if forceRefresh {
		observed, err := m.load(ctx, provider, credentialID)
		if err != nil {
			return schemas.ResolvedProviderCredential{}, err
		}
		observedVersion = observed.Version
	}
	unlock := m.lockCredential(provider, credentialID)
	defer unlock()

	row, err := m.load(ctx, provider, credentialID)
	if err != nil {
		return schemas.ResolvedProviderCredential{}, err
	}
	if row.Status != StatusConnected && row.Status != StatusExpired {
		return schemas.ResolvedProviderCredential{}, fmt.Errorf("provider credential is %s", row.Status)
	}
	// A concurrent 401 recovery may have rotated the credential while this
	// caller waited for the per-account lock. Reuse that newer token instead of
	// consuming the refresh token a second time.
	if forceRefresh && row.Version != observedVersion {
		forceRefresh = false
	}
	refreshWindow := 5 * time.Minute
	if provider == ProviderXAI {
		refreshWindow = 2 * time.Minute
	}
	needsRefresh := forceRefresh || row.ExpiresAt == nil || !row.ExpiresAt.After(m.now().Add(refreshWindow))
	if needsRefresh {
		refreshed, refreshErr := m.refreshLockedWithPolicy(ctx, row, expireOnRefreshFailure)
		if refreshed != nil {
			row = refreshed
		}
		err = refreshErr
		if err != nil {
			if row.ExpiresAt != nil && row.ExpiresAt.After(m.now()) && !forceRefresh {
				return resolved(row), nil
			}
			return schemas.ResolvedProviderCredential{}, err
		}
	}
	return resolved(row), nil
}

func resolved(row *tables.TableProviderCredential) schemas.ResolvedProviderCredential {
	extra := map[string]string{}
	if row.IDToken != "" {
		claims := jwtClaims(row.IDToken)
		if authClaims, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			if fedramp, _ := authClaims["chatgpt_account_is_fedramp"].(bool); fedramp {
				extra["X-OpenAI-Fedramp"] = "true"
			}
		}
	}
	return schemas.ResolvedProviderCredential{AccessToken: row.AccessToken, AccountID: row.AccountID, ExtraHeaders: extra}
}

func (m *Manager) Refresh(ctx context.Context, provider schemas.ModelProvider, credentialID string) (CredentialStatus, error) {
	unlock := m.lockCredential(provider, credentialID)
	defer unlock()
	row, err := m.load(ctx, provider, credentialID)
	if err != nil {
		return CredentialStatus{}, err
	}
	// A user-initiated refresh is a best-effort token rotation. A transient
	// provider failure must not invalidate an otherwise connected credential;
	// callers can retry or explicitly disconnect it.
	row, err = m.refreshLockedWithPolicy(ctx, row, false)
	if err != nil {
		return CredentialStatus{}, err
	}
	return publicStatus(row, m.now()), nil
}

func (m *Manager) refreshLocked(ctx context.Context, row *tables.TableProviderCredential) (*tables.TableProviderCredential, error) {
	return m.refreshLockedWithPolicy(ctx, row, true)
}

func (m *Manager) refreshLockedWithPolicy(ctx context.Context, row *tables.TableProviderCredential, expireOnFailure bool) (*tables.TableProviderCredential, error) {
	if row.RefreshToken == "" {
		return row, errors.New("provider credential has no refresh token")
	}
	leaseOwner := uuid.NewString()
	for {
		leaseExpiresAt, acquired, err := m.acquireRefreshLease(ctx, row, leaseOwner)
		if err != nil {
			return row, err
		}
		if acquired {
			return m.refreshWithLease(ctx, row, leaseOwner, leaseExpiresAt, expireOnFailure)
		}

		latest, loadErr := m.load(ctx, schemas.ModelProvider(row.Provider), row.CredentialID)
		if loadErr != nil {
			return row, loadErr
		}
		if latest.Version > row.Version && isUsableCredential(latest, m.now()) {
			return latest, nil
		}
		row = latest
		waitFor := refreshLeasePollInterval
		if row.RefreshLeaseExpiresAt != nil {
			untilExpiry := row.RefreshLeaseExpiresAt.Sub(m.now())
			if untilExpiry > 0 && untilExpiry < waitFor {
				waitFor = untilExpiry
			}
		}
		if err := wait(ctx, waitFor); err != nil {
			return row, err
		}
	}
}

func (m *Manager) refreshWithLease(ctx context.Context, row *tables.TableProviderCredential, leaseOwner string, leaseExpiresAt time.Time, expireOnFailure bool) (*tables.TableProviderCredential, error) {
	refreshCtx, cancel := context.WithDeadline(ctx, leaseExpiresAt)
	defer cancel()
	tokens, err := m.refreshTokens(refreshCtx, schemas.ModelProvider(row.Provider), row.RefreshToken)
	if err != nil {
		var markErr error
		if expireOnFailure || isTerminalRefreshError(err) {
			markErr = m.failRefreshLease(ctx, row, leaseOwner)
		} else {
			markErr = m.releaseRefreshLease(ctx, row, leaseOwner)
		}
		if markErr != nil && !errors.Is(markErr, errCredentialChanged) {
			return row, markErr
		}
		return row, err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = row.RefreshToken
	}
	if tokens.IDToken == "" {
		tokens.IDToken = row.IDToken
	}
	accountID := row.AccountID
	if accountID == "" {
		accountID = accountIDFromToken(tokens.IDToken)
	}
	now := m.now().UTC()
	err = m.updateAtVersionWithLease(ctx, schemas.ModelProvider(row.Provider), row.CredentialID, row.Version, leaseOwner, func(candidate *tables.TableProviderCredential) {
		candidate.AccessToken = tokens.AccessToken
		candidate.RefreshToken = tokens.RefreshToken
		candidate.IDToken = tokens.IDToken
		candidate.AccountID = accountID
		candidate.ExpiresAt = tokens.ExpiresAt
		candidate.LastRefresh = &now
		candidate.Status = StatusConnected
		candidate.Version = row.Version + 1
		candidate.RefreshLeaseOwner = ""
		candidate.RefreshLeaseExpiresAt = nil
	})
	if err != nil {
		if !errors.Is(err, errCredentialChanged) {
			return row, err
		}
		latest, loadErr := m.load(ctx, schemas.ModelProvider(row.Provider), row.CredentialID)
		if loadErr != nil {
			return row, err
		}
		if isUsableCredential(latest, m.now()) {
			return latest, nil
		}
		return row, err
	}
	return m.load(ctx, schemas.ModelProvider(row.Provider), row.CredentialID)
}

func (m *Manager) releaseRefreshLease(ctx context.Context, row *tables.TableProviderCredential, leaseOwner string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	result := m.store.DB().WithContext(cleanupCtx).Table(row.TableName()).
		Where("credential_id = ? AND provider = ? AND version = ? AND refresh_lease_owner = ?", row.CredentialID, row.Provider, row.Version, leaseOwner).
		Updates(map[string]any{
			"refresh_lease_owner":      "",
			"refresh_lease_expires_at": nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errCredentialChanged
	}
	return nil
}

func isUsableCredential(row *tables.TableProviderCredential, now time.Time) bool {
	return row.Status == StatusConnected && row.AccessToken != "" && row.ExpiresAt != nil && row.ExpiresAt.After(now)
}

func (m *Manager) acquireRefreshLease(ctx context.Context, row *tables.TableProviderCredential, leaseOwner string) (time.Time, bool, error) {
	now := m.now().UTC()
	expiresAt := now.Add(refreshLeaseDuration)
	result := m.store.DB().WithContext(ctx).Table(row.TableName()).
		Where("credential_id = ? AND provider = ? AND version = ?", row.CredentialID, row.Provider, row.Version).
		Where("refresh_lease_owner = '' OR refresh_lease_owner IS NULL OR refresh_lease_expires_at IS NULL OR refresh_lease_expires_at <= ?", now).
		Updates(map[string]any{"refresh_lease_owner": leaseOwner, "refresh_lease_expires_at": expiresAt})
	if result.Error != nil {
		return time.Time{}, false, result.Error
	}
	return expiresAt, result.RowsAffected == 1, nil
}

func (m *Manager) failRefreshLease(ctx context.Context, row *tables.TableProviderCredential, leaseOwner string) error {
	result := m.store.DB().WithContext(ctx).Table(row.TableName()).
		Where("credential_id = ? AND provider = ? AND version = ? AND refresh_lease_owner = ?", row.CredentialID, row.Provider, row.Version, leaseOwner).
		Updates(map[string]any{
			"status":                   StatusExpired,
			"version":                  row.Version + 1,
			"refresh_lease_owner":      "",
			"refresh_lease_expires_at": nil,
			"updated_at":               m.now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errCredentialChanged
	}
	return nil
}

type terminalAttemptRef struct {
	id string
	at time.Time
}

// cleanupTerminalAttemptsLocked retains terminal status long enough for API
// polling, then expires it. The hard cap prevents bursts of completed logins
// from growing the manager permanently even before the retention window ends.
// Connecting attempts are never removed here.
func (m *Manager) cleanupTerminalAttemptsLocked(now time.Time) {
	terminal := make([]terminalAttemptRef, 0)
	for id, attempt := range m.attempts {
		if attempt.status.Status == StatusConnecting {
			continue
		}
		if attempt.terminalAt.IsZero() {
			attempt.terminalAt = now
		}
		if !now.Before(attempt.terminalAt.Add(m.attemptRetention)) {
			delete(m.attempts, id)
			continue
		}
		terminal = append(terminal, terminalAttemptRef{id: id, at: attempt.terminalAt})
	}
	if len(terminal) <= m.maxTerminalAttempts {
		return
	}
	sort.Slice(terminal, func(i, j int) bool { return terminal[i].at.Before(terminal[j].at) })
	for _, ref := range terminal[:len(terminal)-m.maxTerminalAttempts] {
		delete(m.attempts, ref.id)
	}
}

func (m *Manager) load(ctx context.Context, provider schemas.ModelProvider, credentialID string) (*tables.TableProviderCredential, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("provider credential storage is unavailable")
	}
	var row tables.TableProviderCredential
	err := m.store.DB().WithContext(ctx).
		Where("provider = ? AND credential_id = ?", string(provider), credentialID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrCredentialNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (m *Manager) saveLogin(ctx context.Context, provider schemas.ModelProvider, credentialID string, expectedVersion uint64, tokens tokenSet) error {
	accountID := accountIDFromToken(tokens.IDToken)
	authMode := "device_code"
	if provider == ProviderCursor {
		accountID = credentialID
		authMode = "pkce_browser"
	}
	now := m.now().UTC()
	row := tables.TableProviderCredential{
		CredentialID:  credentialID,
		Provider:      string(provider),
		ProviderKeyID: credentialID,
		AuthMode:      authMode,
		AccessToken:   tokens.AccessToken,
		RefreshToken:  tokens.RefreshToken,
		IDToken:       tokens.IDToken,
		AccountID:     accountID,
		ExpiresAt:     tokens.ExpiresAt,
		LastRefresh:   &now,
		Status:        StatusConnected,
		Version:       expectedVersion + 1,
	}
	if expectedVersion == 0 {
		result := m.store.DB().WithContext(ctx).Create(&row)
		if result.Error == nil {
			return nil
		}
		if !errors.Is(result.Error, gorm.ErrDuplicatedKey) {
			return result.Error
		}
	}
	return m.updateAtVersion(ctx, provider, credentialID, expectedVersion, func(candidate *tables.TableProviderCredential) {
		candidate.ProviderKeyID = credentialID
		candidate.AuthMode = authMode
		candidate.AccessToken = tokens.AccessToken
		candidate.RefreshToken = tokens.RefreshToken
		candidate.IDToken = tokens.IDToken
		candidate.AccountID = accountID
		candidate.ExpiresAt = tokens.ExpiresAt
		candidate.LastRefresh = &now
		candidate.Status = StatusConnected
		candidate.Version = expectedVersion + 1
		candidate.RefreshLeaseOwner = ""
		candidate.RefreshLeaseExpiresAt = nil
	})
}

func (m *Manager) updateAtVersion(ctx context.Context, provider schemas.ModelProvider, credentialID string, expectedVersion uint64, mutate func(*tables.TableProviderCredential)) error {
	return m.updateAtVersionWithLease(ctx, provider, credentialID, expectedVersion, "", mutate)
}

// updateAtVersionWithLease performs a true single-statement CAS. Preparing the
// candidate through BeforeSave preserves token encryption while the raw table
// update prevents GORM from issuing an unfenced primary-key Save.
func (m *Manager) updateAtVersionWithLease(ctx context.Context, provider schemas.ModelProvider, credentialID string, expectedVersion uint64, leaseOwner string, mutate func(*tables.TableProviderCredential)) error {
	db := m.store.DB().WithContext(ctx)
	var current tables.TableProviderCredential
	if err := db.Where("credential_id = ? AND provider = ?", credentialID, string(provider)).First(&current).Error; err != nil {
		return err
	}
	if current.Version != expectedVersion || (leaseOwner != "" && current.RefreshLeaseOwner != leaseOwner) {
		return errCredentialChanged
	}
	mutate(&current)
	current.UpdatedAt = m.now().UTC()
	if err := current.BeforeSave(db); err != nil {
		return err
	}

	query := db.Table(current.TableName()).
		Where("credential_id = ? AND provider = ? AND version = ?", credentialID, string(provider), expectedVersion)
	if leaseOwner != "" {
		query = query.Where("refresh_lease_owner = ? AND refresh_lease_expires_at > ?", leaseOwner, m.now().UTC())
	}
	result := query.Updates(map[string]any{
		"provider_key_id":          current.ProviderKeyID,
		"auth_mode":                current.AuthMode,
		"access_token":             current.AccessToken,
		"refresh_token":            current.RefreshToken,
		"id_token":                 current.IDToken,
		"account_id":               current.AccountID,
		"expires_at":               current.ExpiresAt,
		"last_refresh":             current.LastRefresh,
		"status":                   current.Status,
		"version":                  current.Version,
		"refresh_lease_owner":      current.RefreshLeaseOwner,
		"refresh_lease_expires_at": current.RefreshLeaseExpiresAt,
		"updated_at":               current.UpdatedAt,
		"encryption_status":        current.EncryptionStatus,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errCredentialChanged
	}
	return nil
}

type tokenSet struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    *time.Time
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

func accountIDFromToken(idToken string) string {
	claims := jwtClaims(idToken)
	authClaims, _ := claims["https://api.openai.com/auth"].(map[string]any)
	accountID, _ := authClaims["chatgpt_account_id"].(string)
	return accountID
}

func newLoginID() string { return uuid.NewString() }
