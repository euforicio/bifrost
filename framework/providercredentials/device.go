package providercredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const maxOAuthResponseBytes = 1 << 20

type deviceGrant struct {
	deviceCode      string
	deviceAuthID    string
	userCode        string
	verificationURL string
	expiresAt       time.Time
	interval        time.Duration
	expectedVersion uint64
}

// StartLogin starts the provider's supported interactive account flow. Cursor
// uses browser PKCE; existing providers retain their device-code behavior.
func (m *Manager) StartLogin(ctx context.Context, provider schemas.ModelProvider, credentialID string) (LoginStatus, error) {
	if provider == ProviderCursor {
		return m.startCursorLogin(ctx, credentialID)
	}
	return m.StartDeviceLogin(ctx, provider, credentialID)
}

func (m *Manager) StartDeviceLogin(ctx context.Context, provider schemas.ModelProvider, credentialID string) (LoginStatus, error) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return LoginStatus{}, errors.New("credential_id is required")
	}
	if provider != ProviderOpenAICodex && provider != ProviderXAI {
		return LoginStatus{}, errors.New("provider does not support account login")
	}
	version := uint64(0)
	if existing, err := m.load(ctx, provider, credentialID); err == nil {
		version = existing.Version
	} else if !errors.Is(err, ErrCredentialNotFound) {
		return LoginStatus{}, err
	}
	grant, err := m.requestDeviceGrant(ctx, provider)
	if err != nil {
		return LoginStatus{}, err
	}
	grant.expectedVersion = version
	loginID := newLoginID()
	attemptCtx, cancel := context.WithDeadline(context.Background(), grant.expiresAt)
	status := LoginStatus{
		LoginID:            loginID,
		CredentialID:       credentialID,
		Provider:           string(provider),
		Status:             StatusConnecting,
		VerificationURL:    grant.verificationURL,
		UserCode:           grant.userCode,
		ExpiresAt:          &grant.expiresAt,
		PollIntervalSecond: max(1, int(grant.interval.Seconds())),
	}
	attempt := &loginAttempt{status: status, cancel: cancel}
	m.mu.Lock()
	now := m.now()
	m.cleanupTerminalAttemptsLocked(now)
	for _, current := range m.attempts {
		if current.status.Provider == string(provider) && current.status.CredentialID == credentialID && current.status.Status == StatusConnecting {
			current.cancel()
			current.status.Status = StatusDisconnected
			current.status.ErrorCode = "superseded"
			current.terminalAt = now
		}
	}
	m.attempts[loginID] = attempt
	m.cleanupTerminalAttemptsLocked(now)
	m.mu.Unlock()
	go m.completeDeviceLogin(attemptCtx, attempt, provider, grant)
	return status, nil
}

func (m *Manager) completeDeviceLogin(ctx context.Context, attempt *loginAttempt, provider schemas.ModelProvider, grant deviceGrant) {
	tokens, err := m.pollDeviceGrant(ctx, provider, grant)
	if err == nil {
		err = m.saveLogin(ctx, provider, attempt.status.CredentialID, grant.expectedVersion, tokens)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt.status.Status != StatusConnecting {
		return
	}
	if err == nil {
		attempt.status.Status = StatusConnected
		attempt.status.UserCode = ""
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

func classifyLoginError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "expired"
	case strings.Contains(strings.ToLower(err.Error()), "denied"):
		return "access_denied"
	default:
		return "authorization_failed"
	}
}

func (m *Manager) requestDeviceGrant(ctx context.Context, provider schemas.ModelProvider) (deviceGrant, error) {
	if provider == ProviderOpenAICodex {
		return m.requestOpenAIDeviceGrant(ctx)
	}
	return m.requestXAIDeviceGrant(ctx)
}

func (m *Manager) requestOpenAIDeviceGrant(ctx context.Context) (deviceGrant, error) {
	body, err := json.Marshal(map[string]string{"client_id": openAIClientID})
	if err != nil {
		return deviceGrant{}, err
	}
	endpoint := m.endpoints.openAIIssuer + "/api/accounts/deviceauth/usercode"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return deviceGrant{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return deviceGrant{}, errors.New("OpenAI device authorization service is unavailable")
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort protocol cleanup.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return deviceGrant{}, fmt.Errorf("OpenAI device authorization failed with status %d", resp.StatusCode)
	}
	var payload struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		UserCodeAlt  string `json:"usercode"`
		Interval     string `json:"interval"`
	}
	if err := decodeOAuthJSON(resp.Body, &payload); err != nil {
		return deviceGrant{}, errors.New("OpenAI device authorization response is invalid")
	}
	if payload.UserCode == "" {
		payload.UserCode = payload.UserCodeAlt
	}
	if payload.DeviceAuthID == "" || payload.UserCode == "" {
		return deviceGrant{}, errors.New("OpenAI device authorization response is invalid")
	}
	interval := 5 * time.Second
	if seconds, err := strconv.Atoi(strings.TrimSpace(payload.Interval)); err == nil && seconds > 0 {
		interval = time.Duration(seconds) * time.Second
	}
	return deviceGrant{
		deviceAuthID:    payload.DeviceAuthID,
		userCode:        payload.UserCode,
		verificationURL: m.endpoints.openAIIssuer + "/codex/device",
		expiresAt:       m.now().Add(15 * time.Minute),
		interval:        interval,
	}, nil
}

func (m *Manager) requestXAIDeviceGrant(ctx context.Context) (deviceGrant, error) {
	form := url.Values{"client_id": {xAIClientID}, "scope": {xAIScopes}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoints.xAIDeviceURL, strings.NewReader(form.Encode()))
	if err != nil {
		return deviceGrant{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return deviceGrant{}, errors.New("xAI device authorization service is unavailable")
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort protocol cleanup.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return deviceGrant{}, fmt.Errorf("xAI device authorization failed with status %d", resp.StatusCode)
	}
	var payload struct {
		DeviceCode              string  `json:"device_code"`
		UserCode                string  `json:"user_code"`
		VerificationURI         string  `json:"verification_uri"`
		VerificationURIComplete string  `json:"verification_uri_complete"`
		ExpiresIn               float64 `json:"expires_in"`
		Interval                float64 `json:"interval"`
	}
	if err := decodeOAuthJSON(resp.Body, &payload); err != nil || payload.DeviceCode == "" || payload.UserCode == "" || payload.VerificationURI == "" {
		return deviceGrant{}, errors.New("xAI device authorization response is invalid")
	}
	expires := durationSeconds(payload.ExpiresIn, 5*time.Minute)
	if expires > time.Hour {
		return deviceGrant{}, errors.New("xAI device authorization response is invalid")
	}
	interval := durationSeconds(payload.Interval, 5*time.Second)
	interval = min(max(interval, time.Second), time.Minute)
	verificationURL := payload.VerificationURI
	if payload.VerificationURIComplete != "" {
		verificationURL = payload.VerificationURIComplete
	}
	return deviceGrant{
		deviceCode:      payload.DeviceCode,
		userCode:        payload.UserCode,
		verificationURL: verificationURL,
		expiresAt:       m.now().Add(expires),
		interval:        interval,
	}, nil
}

func (m *Manager) pollDeviceGrant(ctx context.Context, provider schemas.ModelProvider, grant deviceGrant) (tokenSet, error) {
	interval := grant.interval
	for {
		if err := wait(ctx, interval); err != nil {
			return tokenSet{}, err
		}
		if provider == ProviderOpenAICodex {
			tokens, pending, err := m.pollOpenAI(ctx, grant)
			if pending {
				continue
			}
			return tokens, err
		}
		tokens, state, err := m.pollXAI(ctx, grant)
		switch state {
		case "authorization_pending":
			continue
		case "slow_down":
			interval = min(interval+5*time.Second, time.Minute)
			continue
		case "access_denied", "authorization_denied":
			return tokenSet{}, errors.New("xAI device authorization denied")
		case "expired_token":
			return tokenSet{}, context.DeadlineExceeded
		}
		return tokens, err
	}
}

func (m *Manager) pollOpenAI(ctx context.Context, grant deviceGrant) (tokenSet, bool, error) {
	body, _ := json.Marshal(map[string]string{"device_auth_id": grant.deviceAuthID, "user_code": grant.userCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoints.openAIIssuer+"/api/accounts/deviceauth/token", bytes.NewReader(body))
	if err != nil {
		return tokenSet{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return tokenSet{}, false, errors.New("OpenAI device authorization service is unavailable")
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort protocol cleanup.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxOAuthResponseBytes))
		return tokenSet{}, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenSet{}, false, fmt.Errorf("OpenAI device authorization failed with status %d", resp.StatusCode)
	}
	var code struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if err := decodeOAuthJSON(resp.Body, &code); err != nil || code.AuthorizationCode == "" || code.CodeVerifier == "" {
		return tokenSet{}, false, errors.New("OpenAI device authorization response is invalid")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code.AuthorizationCode},
		"redirect_uri":  {m.endpoints.openAIIssuer + "/deviceauth/callback"},
		"client_id":     {openAIClientID},
		"code_verifier": {code.CodeVerifier},
	}
	tokens, _, err := m.postTokenForm(ctx, m.endpoints.openAIIssuer+"/oauth/token", form)
	return tokens, false, err
}

func (m *Manager) pollXAI(ctx context.Context, grant deviceGrant) (tokenSet, string, error) {
	form := url.Values{"grant_type": {deviceGrantType}, "client_id": {xAIClientID}, "device_code": {grant.deviceCode}}
	return m.postTokenForm(ctx, m.endpoints.xAITokenURL, form)
}

func (m *Manager) refreshTokens(ctx context.Context, provider schemas.ModelProvider, refreshToken string) (tokenSet, error) {
	if provider == ProviderCursor {
		return m.refreshCursorTokens(ctx, refreshToken)
	}
	if provider == ProviderOpenAICodex {
		body, _ := json.Marshal(map[string]string{"client_id": openAIClientID, "grant_type": "refresh_token", "refresh_token": refreshToken})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoints.openAIIssuer+"/oauth/token", bytes.NewReader(body))
		if err != nil {
			return tokenSet{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		return m.doTokenRequest(req)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {xAIClientID}}
	tokens, state, err := m.postTokenForm(ctx, m.endpoints.xAITokenURL, form)
	if state == "invalid_grant" || state == "access_denied" || state == "expired_token" {
		return tokenSet{}, errors.New("xAI authentication expired")
	}
	return tokens, err
}

func (m *Manager) postTokenForm(ctx context.Context, endpoint string, form url.Values) (tokenSet, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenSet{}, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return tokenSet{}, "", errors.New("provider token service is unavailable")
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort protocol cleanup.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var payload struct {
			AccessToken  string  `json:"access_token"`
			RefreshToken string  `json:"refresh_token"`
			IDToken      string  `json:"id_token"`
			ExpiresIn    float64 `json:"expires_in"`
		}
		if err := decodeOAuthJSON(resp.Body, &payload); err != nil || payload.AccessToken == "" {
			return tokenSet{}, "", errors.New("provider token response is invalid")
		}
		expiresAt := expiryFromToken(m.now(), payload.AccessToken, payload.ExpiresIn)
		return tokenSet{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, IDToken: payload.IDToken, ExpiresAt: expiresAt}, "", nil
	}
	var failure struct {
		Error string `json:"error"`
	}
	_ = decodeOAuthJSON(resp.Body, &failure)
	return tokenSet{}, failure.Error, fmt.Errorf("provider token exchange failed with status %d", resp.StatusCode)
}

func (m *Manager) doTokenRequest(req *http.Request) (tokenSet, error) {
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return tokenSet{}, errors.New("provider token service is unavailable")
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort protocol cleanup.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenSet{}, fmt.Errorf("provider token refresh failed with status %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken  *string `json:"access_token"`
		RefreshToken *string `json:"refresh_token"`
		IDToken      *string `json:"id_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := decodeOAuthJSON(resp.Body, &payload); err != nil || payload.AccessToken == nil || *payload.AccessToken == "" {
		return tokenSet{}, errors.New("provider token refresh response is invalid")
	}
	tokens := tokenSet{AccessToken: *payload.AccessToken, ExpiresAt: expiryFromToken(m.now(), *payload.AccessToken, payload.ExpiresIn)}
	if payload.RefreshToken != nil {
		tokens.RefreshToken = *payload.RefreshToken
	}
	if payload.IDToken != nil {
		tokens.IDToken = *payload.IDToken
	}
	return tokens, nil
}

func decodeOAuthJSON(reader io.Reader, dst any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxOAuthResponseBytes))
	return decoder.Decode(dst)
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func durationSeconds(value float64, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return time.Duration(value * float64(time.Second))
}

func expiryFromToken(now time.Time, accessToken string, expiresIn float64) *time.Time {
	if claims := jwtClaims(accessToken); claims != nil {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			expiresAt := time.Unix(int64(exp), 0).UTC()
			return &expiresAt
		}
	}
	expiresAt := now.Add(durationSeconds(expiresIn, time.Hour)).UTC()
	return &expiresAt
}
