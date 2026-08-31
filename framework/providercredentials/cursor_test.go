package providercredentials

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newCursorTestManager(t *testing.T, server *httptest.Server) (*Manager, *gorm.DB) {
	t.Helper()
	encrypt.Init("cursor-provider-test-key-32bytes", bifrost.NewDefaultLogger(schemas.LogLevelError))
	db, err := gorm.Open(sqlite.Open(providerCredentialTestDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tables.TableProviderCredential{}))
	manager := NewManager(
		databaseStore{db: db},
		WithHTTPClient(server.Client()),
		WithCursorEndpoints(server.URL+"/loginDeepControl", server.URL),
		WithCursorPollInterval(5*time.Millisecond),
	)
	return manager, db
}

func TestCursorPKCELoginIsolatesDynamicAccounts(t *testing.T) {
	var mu sync.Mutex
	polls := make(map[string]int)
	verifiers := make(map[string]string)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cursorPollPath {
			http.NotFound(w, r)
			return
		}
		uuid := r.URL.Query().Get("uuid")
		verifier := r.URL.Query().Get("verifier")
		mu.Lock()
		polls[uuid]++
		verifiers[uuid] = verifier
		count := polls[uuid]
		mu.Unlock()
		if count == 1 {
			http.NotFound(w, r)
			return
		}
		access := unsignedJWT(map[string]any{"exp": time.Now().Add(2 * time.Hour).Unix(), "session": uuid})
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessToken": access, "refreshToken": "refresh-" + uuid,
		})
	}))
	defer server.Close()
	manager, db := newCursorTestManager(t, server)

	loginA, err := manager.StartLogin(context.Background(), ProviderCursor, "cursor-account-a")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		status, statusErr := manager.LoginStatus(loginA.LoginID)
		return statusErr == nil && status.Status == StatusConnected
	}, 3*time.Second, 10*time.Millisecond)
	loginB, err := manager.StartLogin(context.Background(), ProviderCursor, "cursor-account-b")
	require.NoError(t, err)
	require.Empty(t, loginA.UserCode)
	require.Empty(t, loginB.UserCode)
	require.NotEqual(t, loginA.LoginID, loginB.LoginID)

	for _, login := range []LoginStatus{loginA, loginB} {
		parsed, parseErr := url.Parse(login.VerificationURL)
		require.NoError(t, parseErr)
		require.Equal(t, "/loginDeepControl", parsed.Path)
		require.Equal(t, login.LoginID, parsed.Query().Get("uuid"))
		require.Equal(t, "login", parsed.Query().Get("mode"))
		require.Equal(t, "cli", parsed.Query().Get("redirectTarget"))
		require.NotEmpty(t, parsed.Query().Get("challenge"))
	}

	var finalA, finalB LoginStatus
	connected := assert.Eventually(t, func() bool {
		statusA, errA := manager.LoginStatus(loginA.LoginID)
		statusB, errB := manager.LoginStatus(loginB.LoginID)
		finalA, finalB = statusA, statusB
		return errA == nil && errB == nil && statusA.Status == StatusConnected && statusB.Status == StatusConnected
	}, 3*time.Second, 10*time.Millisecond)
	if !connected {
		t.Fatalf("final statuses: A=%+v B=%+v", finalA, finalB)
	}

	for _, tc := range []struct {
		credentialID string
		login        LoginStatus
	}{
		{credentialID: "cursor-account-a", login: loginA},
		{credentialID: "cursor-account-b", login: loginB},
	} {
		parsed, parseErr := url.Parse(tc.login.VerificationURL)
		require.NoError(t, parseErr)
		mu.Lock()
		verifier := verifiers[tc.login.LoginID]
		mu.Unlock()
		digest := sha256.Sum256([]byte(verifier))
		require.Equal(t, parsed.Query().Get("challenge"), base64.RawURLEncoding.EncodeToString(digest[:]))

		resolved, resolveErr := manager.ResolveProviderCredential(context.Background(), ProviderCursor, tc.credentialID, false)
		require.NoError(t, resolveErr)
		require.Equal(t, tc.credentialID, resolved.AccountID)
		claims := jwtClaims(resolved.AccessToken)
		require.Equal(t, tc.login.LoginID, claims["session"])

		status, statusErr := manager.Status(context.Background(), ProviderCursor, tc.credentialID)
		require.NoError(t, statusErr)
		require.Equal(t, tc.credentialID, status.AccountID)
		encoded, marshalErr := json.Marshal(status)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(encoded), "refresh-")
		require.NotContains(t, string(encoded), verifier)
	}

	var rows []tables.TableProviderCredential
	require.NoError(t, db.Where("provider = ?", string(ProviderCursor)).Order("credential_id").Find(&rows).Error)
	require.Len(t, rows, 2)
	for i := range rows {
		require.Equal(t, "pkce_browser", rows[i].AuthMode)
		require.Equal(t, rows[i].CredentialID, rows[i].AccountID)
	}
	var raw map[string]any
	require.NoError(t, db.Table("provider_credentials").Where("credential_id = ?", "cursor-account-a").Take(&raw).Error)
	require.NotContains(t, raw["access_token"], loginA.LoginID)
	require.NotEqual(t, "refresh-"+loginA.LoginID, raw["refresh_token"])
}

func TestCursorRefreshPreservesAndRotatesRefreshToken(t *testing.T) {
	var refreshes atomic.Int64
	var protocolOK atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cursorRefreshPath {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("Authorization") != "" && r.Header.Get("Content-Type") == "application/json" {
			protocolOK.Store(true)
		}
		count := refreshes.Add(1)
		payload := map[string]string{
			"accessToken": unsignedJWT(map[string]any{"exp": time.Now().Add(2 * time.Hour).Unix(), "rotation": count}),
		}
		if count == 2 {
			payload["refreshToken"] = "refresh-rotated"
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()
	manager, db := newCursorTestManager(t, server)
	expired := time.Now().Add(-time.Minute)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "cursor-account", Provider: string(ProviderCursor), ProviderKeyID: "cursor-account",
		AuthMode: "pkce_browser", AccessToken: "expired-access", RefreshToken: "refresh-original",
		AccountID: "cursor-account", ExpiresAt: &expired, Status: StatusConnected, Version: 1,
	}).Error)

	first, err := manager.Refresh(context.Background(), ProviderCursor, "cursor-account")
	require.NoError(t, err)
	require.EqualValues(t, 2, first.Version)
	var row tables.TableProviderCredential
	require.NoError(t, db.Where("credential_id = ?", "cursor-account").First(&row).Error)
	require.Equal(t, "refresh-original", row.RefreshToken)
	require.Equal(t, float64(1), jwtClaims(row.AccessToken)["rotation"])

	second, err := manager.Refresh(context.Background(), ProviderCursor, "cursor-account")
	require.NoError(t, err)
	require.EqualValues(t, 3, second.Version)
	require.NoError(t, db.Where("credential_id = ?", "cursor-account").First(&row).Error)
	require.Equal(t, "refresh-rotated", row.RefreshToken)
	require.Equal(t, float64(2), jwtClaims(row.AccessToken)["rotation"])
	var raw map[string]any
	require.NoError(t, db.Table("provider_credentials").Where("credential_id = ?", "cursor-account").Take(&raw).Error)
	require.NotEqual(t, row.AccessToken, raw["access_token"])
	require.NotEqual(t, row.RefreshToken, raw["refresh_token"])
	require.Equal(t, tables.EncryptionStatusEncrypted, raw["encryption_status"])
	require.True(t, protocolOK.Load())
	require.Equal(t, int64(2), refreshes.Load())
}

func TestCursorDisconnectCancelsOnlyMatchingLoginAndCredential(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	manager, db := newCursorTestManager(t, server)
	expiresAt := time.Now().Add(time.Hour)
	for _, id := range []string{"cursor-account-a", "cursor-account-b"} {
		require.NoError(t, db.Create(&tables.TableProviderCredential{
			CredentialID: id, Provider: string(ProviderCursor), ProviderKeyID: id, AuthMode: "pkce_browser",
			AccessToken: "access-" + id, RefreshToken: "refresh-" + id, AccountID: id,
			ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
		}).Error)
	}
	loginA, err := manager.StartLogin(context.Background(), ProviderCursor, "cursor-account-a")
	require.NoError(t, err)
	loginB, err := manager.StartLogin(context.Background(), ProviderCursor, "cursor-account-b")
	require.NoError(t, err)

	require.NoError(t, manager.Disconnect(context.Background(), ProviderCursor, "cursor-account-a"))
	statusA, err := manager.LoginStatus(loginA.LoginID)
	require.NoError(t, err)
	require.Equal(t, StatusDisconnected, statusA.Status)
	require.Equal(t, "cancelled", statusA.ErrorCode)
	statusB, err := manager.LoginStatus(loginB.LoginID)
	require.NoError(t, err)
	require.Equal(t, StatusConnecting, statusB.Status)

	disconnected, err := manager.Status(context.Background(), ProviderCursor, "cursor-account-a")
	require.NoError(t, err)
	require.Equal(t, StatusDisconnected, disconnected.Status)
	resolvedB, err := manager.ResolveProviderCredential(context.Background(), ProviderCursor, "cursor-account-b", false)
	require.NoError(t, err)
	require.Equal(t, "cursor-account-b", resolvedB.AccountID)
	require.NoError(t, manager.CancelLogin(loginB.LoginID))

	rows, err := manager.List(context.Background(), ProviderCursor)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "cursor-account-b", rows[0].CredentialID)
}

func TestStartDeviceLoginKeepsCursorOnBrowserFlow(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	manager, _ := newCursorTestManager(t, server)
	_, err := manager.StartDeviceLogin(context.Background(), ProviderCursor, "cursor-account")
	require.EqualError(t, err, "provider does not support account login")
}
