package providercredentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func unsignedJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

type databaseStore struct{ db *gorm.DB }

func (s databaseStore) DB() *gorm.DB { return s.db }

var providerCredentialTestDBSequence atomic.Uint64

func providerCredentialTestDSN(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", t.Name(), providerCredentialTestDBSequence.Add(1))
	return "file:" + url.QueryEscape(name) + "?mode=memory&cache=shared"
}

func newTestManager(t *testing.T, server *httptest.Server) (*Manager, *gorm.DB) {
	t.Helper()
	encrypt.Init("provider-credentials-test-key-32bytes", bifrost.NewDefaultLogger(schemas.LogLevelError))
	db, err := gorm.Open(sqlite.Open(providerCredentialTestDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tables.TableProviderCredential{}))
	m := NewManager(databaseStore{db: db}, WithHTTPClient(server.Client()), WithEndpoints(server.URL, server.URL+"/oauth2/device/code", server.URL+"/oauth2/token"))
	return m, db
}

func TestMultipleXAIAccountsRemainIsolated(t *testing.T) {
	var deviceSequence atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/device/code":
			id := deviceSequence.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": fmt.Sprintf("device-%d", id), "user_code": fmt.Sprintf("CODE-%d", id),
				"verification_uri": "https://accounts.x.ai/device", "expires_in": 60, "interval": 0.01,
			})
		case "/oauth2/token":
			require.NoError(t, r.ParseForm())
			device := r.Form.Get("device_code")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-" + device, "refresh_token": "refresh-" + device, "expires_in": 3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	m, _ := newTestManager(t, server)
	m.now = time.Now

	for _, credentialID := range []string{"account-key-a", "account-key-b"} {
		login, err := m.StartDeviceLogin(context.Background(), ProviderXAI, credentialID)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			status, statusErr := m.LoginStatus(login.LoginID)
			return statusErr == nil && status.Status == StatusConnected
		}, 3*time.Second, 10*time.Millisecond)
	}

	first, err := m.ResolveProviderCredential(context.Background(), ProviderXAI, "account-key-a", false)
	require.NoError(t, err)
	second, err := m.ResolveProviderCredential(context.Background(), ProviderXAI, "account-key-b", false)
	require.NoError(t, err)
	require.Equal(t, "access-device-1", first.AccessToken)
	require.Equal(t, "access-device-2", second.AccessToken)
	statuses, err := m.List(context.Background(), ProviderXAI)
	require.NoError(t, err)
	require.Len(t, statuses, 2)
}

func TestOpenAIDeviceLoginPersistsAccountBinding(t *testing.T) {
	var polls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			require.Equal(t, "application/json", r.Header.Get("Content-Type"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "device-auth-1", "user_code": "OPEN-AI", "interval": "1",
			})
		case "/api/accounts/deviceauth/token":
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_code": "authorization-code", "code_verifier": "verifier",
			})
		case "/oauth/token":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "authorization_code", r.Form.Get("grant_type"))
			idToken := unsignedJWT(map[string]any{
				"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "account-123"},
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "openai-access", "refresh_token": "openai-refresh",
				"id_token": idToken, "expires_in": 3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	m, _ := newTestManager(t, server)

	login, err := m.StartDeviceLogin(context.Background(), ProviderOpenAICodex, "openai-key-a")
	require.NoError(t, err)
	require.Equal(t, server.URL+"/codex/device", login.VerificationURL)
	require.Eventually(t, func() bool {
		status, statusErr := m.LoginStatus(login.LoginID)
		return statusErr == nil && status.Status == StatusConnected
	}, 4*time.Second, 20*time.Millisecond)

	credential, err := m.ResolveProviderCredential(context.Background(), ProviderOpenAICodex, "openai-key-a", false)
	require.NoError(t, err)
	require.Equal(t, "openai-access", credential.AccessToken)
	require.Equal(t, "account-123", credential.AccountID)
	require.Equal(t, int64(2), polls.Load())
}

func TestConcurrentForcedRefreshRotatesOnce(t *testing.T) {
	var refreshes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(100 * time.Millisecond)
		count := refreshes.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("rotated-%d", count), "refresh_token": "rotated-refresh", "expires_in": 3600,
		})
	}))
	defer server.Close()
	m, db := newTestManager(t, server)
	expiresAt := time.Now().Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "shared-key", Provider: string(ProviderXAI), ProviderKeyID: "shared-key", AuthMode: "device_code",
		AccessToken: "stale", RefreshToken: "refresh", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
	}).Error)

	start := make(chan struct{})
	results := make([]schemas.ResolvedProviderCredential, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results[index], errs[index] = m.ResolveProviderCredential(context.Background(), ProviderXAI, "shared-key", true)
		}(i)
	}
	close(start)
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, int64(1), refreshes.Load())
	require.Equal(t, "rotated-1", results[0].AccessToken)
	require.Equal(t, results[0].AccessToken, results[1].AccessToken)
	m.mu.Lock()
	require.Empty(t, m.locks)
	m.mu.Unlock()
}

func TestDisconnectProviderRemovesOnlyItsAccounts(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	m, db := newTestManager(t, server)
	expiresAt := time.Now().Add(time.Hour)
	for _, row := range []tables.TableProviderCredential{
		{CredentialID: "xai-a", Provider: string(ProviderXAI), ProviderKeyID: "xai-a", AuthMode: "device_code", AccessToken: "a", Status: StatusConnected, ExpiresAt: &expiresAt},
		{CredentialID: "xai-b", Provider: string(ProviderXAI), ProviderKeyID: "xai-b", AuthMode: "device_code", AccessToken: "b", Status: StatusConnected, ExpiresAt: &expiresAt},
		{CredentialID: "codex-a", Provider: string(ProviderOpenAICodex), ProviderKeyID: "codex-a", AuthMode: "device_code", AccessToken: "c", Status: StatusConnected, ExpiresAt: &expiresAt},
	} {
		require.NoError(t, db.Create(&row).Error)
	}

	require.NoError(t, m.DisconnectProvider(context.Background(), ProviderXAI))
	xai, err := m.List(context.Background(), ProviderXAI)
	require.NoError(t, err)
	require.Empty(t, xai)
	codex, err := m.List(context.Background(), ProviderOpenAICodex)
	require.NoError(t, err)
	require.Len(t, codex, 1)
}
