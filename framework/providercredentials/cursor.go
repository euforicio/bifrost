package providercredentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	cursorPollPath    = "/auth/poll"
	cursorRefreshPath = "/auth/exchange_user_api_key"
)

type cursorGrant struct {
	verifier        string
	uuid            string
	loginURL        string
	expiresAt       time.Time
	expectedVersion uint64
}

func (m *Manager) startCursorLogin(ctx context.Context, credentialID string) (LoginStatus, error) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return LoginStatus{}, errors.New("credential_id is required")
	}

	version := uint64(0)
	if existing, err := m.load(ctx, ProviderCursor, credentialID); err == nil {
		version = existing.Version
	} else if !errors.Is(err, ErrCredentialNotFound) {
		return LoginStatus{}, err
	}

	grant, err := m.newCursorGrant()
	if err != nil {
		return LoginStatus{}, err
	}
	grant.expectedVersion = version
	attemptCtx, cancel := context.WithDeadline(context.Background(), grant.expiresAt)
	status := LoginStatus{
		LoginID:            grant.uuid,
		CredentialID:       credentialID,
		Provider:           string(ProviderCursor),
		Status:             StatusConnecting,
		VerificationURL:    grant.loginURL,
		ExpiresAt:          &grant.expiresAt,
		PollIntervalSecond: max(1, int(m.cursorPollInterval.Seconds())),
	}
	attempt := &loginAttempt{status: status, cancel: cancel}
	m.mu.Lock()
	now := m.now()
	m.cleanupTerminalAttemptsLocked(now)
	for _, current := range m.attempts {
		if current.status.Provider == string(ProviderCursor) && current.status.CredentialID == credentialID && current.status.Status == StatusConnecting {
			current.cancel()
			current.status.Status = StatusDisconnected
			current.status.ErrorCode = "superseded"
			current.terminalAt = now
		}
	}
	m.attempts[grant.uuid] = attempt
	m.cleanupTerminalAttemptsLocked(now)
	m.mu.Unlock()

	go m.completeCursorLogin(attemptCtx, attempt, grant)
	return status, nil
}

func (m *Manager) newCursorGrant() (cursorGrant, error) {
	verifierBytes := make([]byte, 96)
	if _, err := rand.Read(verifierBytes); err != nil {
		return cursorGrant{}, fmt.Errorf("failed to create Cursor PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	uuid := newLoginID()

	parsed, err := url.Parse(m.endpoints.cursorLoginURL)
	if err != nil {
		return cursorGrant{}, errors.New("cursor login endpoint is invalid")
	}
	query := parsed.Query()
	query.Set("challenge", challenge)
	query.Set("uuid", uuid)
	query.Set("mode", "login")
	query.Set("redirectTarget", "cli")
	parsed.RawQuery = query.Encode()
	expiresAt := m.now().Add(10 * time.Minute)
	return cursorGrant{verifier: verifier, uuid: uuid, loginURL: parsed.String(), expiresAt: expiresAt}, nil
}

func (m *Manager) completeCursorLogin(ctx context.Context, attempt *loginAttempt, grant cursorGrant) {
	tokens, err := m.pollCursorLogin(ctx, grant)
	if err == nil {
		err = m.saveLogin(ctx, ProviderCursor, attempt.status.CredentialID, grant.expectedVersion, tokens)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt.status.Status != StatusConnecting {
		return
	}
	if err == nil {
		attempt.status.Status = StatusConnected
		attempt.status.VerificationURL = ""
		attempt.terminalAt = m.now()
		m.cleanupTerminalAttemptsLocked(attempt.terminalAt)
		return
	}
	attempt.status.Status = StatusError
	attempt.status.ErrorCode = classifyLoginError(err)
	attempt.terminalAt = m.now()
	m.cleanupTerminalAttemptsLocked(attempt.terminalAt)
}

func (m *Manager) pollCursorLogin(ctx context.Context, grant cursorGrant) (tokenSet, error) {
	delay := m.cursorPollInterval
	consecutiveErrors := 0
	for {
		if err := wait(ctx, delay); err != nil {
			return tokenSet{}, err
		}
		endpoint, err := url.Parse(m.endpoints.cursorAPIBase + cursorPollPath)
		if err != nil {
			return tokenSet{}, errors.New("cursor authorization endpoint is invalid")
		}
		query := endpoint.Query()
		query.Set("uuid", grant.uuid)
		query.Set("verifier", grant.verifier)
		endpoint.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return tokenSet{}, err
		}
		resp, err := m.httpClient.Do(req)
		if err != nil {
			consecutiveErrors++
		} else {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes))
			resp.Body.Close() //nolint:errcheck // Best-effort protocol cleanup.
			if resp.StatusCode == http.StatusNotFound {
				consecutiveErrors = 0
				delay = min(time.Duration(float64(delay)*1.2), 10*time.Second)
				continue
			}
			if readErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var payload struct {
					AccessToken  string `json:"accessToken"`
					RefreshToken string `json:"refreshToken"`
				}
				if json.Unmarshal(body, &payload) != nil || payload.AccessToken == "" || payload.RefreshToken == "" {
					return tokenSet{}, errors.New("cursor authorization returned incomplete credentials")
				}
				return tokenSet{
					AccessToken:  payload.AccessToken,
					RefreshToken: payload.RefreshToken,
					ExpiresAt:    m.cursorExpiry(payload.AccessToken),
				}, nil
			}
			consecutiveErrors++
		}
		if consecutiveErrors >= 3 {
			return tokenSet{}, errors.New("cursor authorization failed after repeated network errors")
		}
	}
}

func (m *Manager) refreshCursorTokens(ctx context.Context, refreshToken string) (tokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoints.cursorAPIBase+cursorRefreshPath, strings.NewReader("{}"))
	if err != nil {
		return tokenSet{}, err
	}
	req.Header.Set("Authorization", "Bearer "+refreshToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return tokenSet{}, errors.New("cursor token service is unavailable")
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort protocol cleanup.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes))
	if err != nil {
		return tokenSet{}, errors.New("cursor token refresh response is invalid")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenSet{}, fmt.Errorf("cursor token refresh failed with status %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.AccessToken == "" {
		return tokenSet{}, errors.New("cursor token refresh returned no access token")
	}
	return tokenSet{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		ExpiresAt:    m.cursorExpiry(payload.AccessToken),
	}, nil
}

func (m *Manager) cursorExpiry(accessToken string) *time.Time {
	expiresAt := m.now().Add(time.Hour).UTC()
	if claims := jwtClaims(accessToken); claims != nil {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			expiresAt = time.Unix(int64(exp), 0).Add(-5 * time.Minute).UTC()
		}
	}
	return &expiresAt
}
