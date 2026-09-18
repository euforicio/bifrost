package providercredentials

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
)

func TestCrossNodeLeaseSerializesInvalidGrantBeforeWinnerCommit(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" {
			http.NotFound(w, r)
			return
		}
		switch requests.Add(1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		default:
			close(secondStarted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "newer-access", "refresh_token": "rotated-refresh", "expires_in": 3600,
			})
		}
	}))
	defer server.Close()

	firstManager, db := newTestManager(t, server)
	secondManager := NewManager(
		databaseStore{db: db},
		WithHTTPClient(server.Client()),
		WithEndpoints(server.URL, server.URL+"/oauth2/device/code", server.URL+"/oauth2/token"),
	)
	expired := time.Now().Add(-time.Minute)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "shared-account", Provider: string(ProviderXAI), ProviderKeyID: "shared-account",
		AuthMode: "device_code", AccessToken: "expired-access", RefreshToken: "shared-refresh",
		ExpiresAt: &expired, Status: StatusConnected, Version: 1,
	}).Error)

	type refreshResult struct {
		accessToken string
		err         error
	}
	firstResult := make(chan refreshResult, 1)
	secondResult := make(chan refreshResult, 1)
	go func() {
		resolved, err := firstManager.ResolveProviderCredential(context.Background(), ProviderXAI, "shared-account", true)
		firstResult <- refreshResult{accessToken: resolved.AccessToken, err: err}
	}()
	<-firstStarted
	go func() {
		resolved, err := secondManager.ResolveProviderCredential(context.Background(), ProviderXAI, "shared-account", true)
		secondResult <- refreshResult{accessToken: resolved.AccessToken, err: err}
	}()
	select {
	case <-secondStarted:
		t.Fatal("second node reached upstream before the durable lease holder finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)

	first := <-firstResult
	require.Error(t, first.err)
	require.Empty(t, first.accessToken)
	second := <-secondResult
	require.NoError(t, second.err)
	require.Equal(t, "newer-access", second.accessToken)
	require.Equal(t, int64(2), requests.Load())

	var persisted tables.TableProviderCredential
	require.NoError(t, db.Where("credential_id = ?", "shared-account").First(&persisted).Error)
	require.EqualValues(t, 3, persisted.Version)
	require.Equal(t, StatusConnected, persisted.Status)
	require.Equal(t, "newer-access", persisted.AccessToken)

	firstManager.mu.Lock()
	require.Empty(t, firstManager.locks)
	firstManager.mu.Unlock()
	secondManager.mu.Lock()
	require.Empty(t, secondManager.locks)
	secondManager.mu.Unlock()
}

func TestTerminalLoginAttemptsArePollableAndBounded(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "device-code", "user_code": "USER-CODE",
				"verification_uri": "https://accounts.x.ai/device", "expires_in": 60, "interval": 0.01,
			})
		case "/oauth2/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-token", "refresh_token": "refresh-token", "expires_in": 3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager, _ := newTestManager(t, server)
	manager.attemptRetention = time.Hour
	manager.maxTerminalAttempts = 2

	logins := make([]LoginStatus, 0, 3)
	for _, credentialID := range []string{"account-a", "account-b", "account-c"} {
		login, err := manager.StartDeviceLogin(context.Background(), ProviderXAI, credentialID)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			status, statusErr := manager.LoginStatus(login.LoginID)
			return statusErr == nil && status.Status == StatusConnected
		}, 3*time.Second, 10*time.Millisecond)
		logins = append(logins, login)
	}

	_, err := manager.LoginStatus(logins[0].LoginID)
	require.EqualError(t, err, "provider login not found")
	for _, login := range logins[1:] {
		status, statusErr := manager.LoginStatus(login.LoginID)
		require.NoError(t, statusErr)
		require.Equal(t, StatusConnected, status.Status)
	}
	manager.mu.Lock()
	require.Len(t, manager.attempts, 2)
	manager.mu.Unlock()

	manager.attemptRetention = time.Nanosecond
	time.Sleep(time.Millisecond)
	_, err = manager.LoginStatus(logins[2].LoginID)
	require.EqualError(t, err, "provider login not found")
	manager.mu.Lock()
	require.Empty(t, manager.attempts)
	manager.mu.Unlock()
}

func TestCancellationOnlyAPIsDoNotDeleteCredentials(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	manager, db := newCursorTestManager(t, server)
	expiresAt := time.Now().Add(time.Hour)
	for _, credentialID := range []string{"cursor-a", "cursor-b"} {
		require.NoError(t, db.Create(&tables.TableProviderCredential{
			CredentialID: credentialID, Provider: string(ProviderCursor), ProviderKeyID: credentialID,
			AuthMode: "pkce_browser", AccessToken: "access-" + credentialID, RefreshToken: "refresh-" + credentialID,
			AccountID: credentialID, ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
		}).Error)
	}
	loginA, err := manager.StartLogin(context.Background(), ProviderCursor, "cursor-a")
	require.NoError(t, err)
	loginB, err := manager.StartLogin(context.Background(), ProviderCursor, "cursor-b")
	require.NoError(t, err)

	manager.CancelCredentialLogins(ProviderCursor, "cursor-a")
	statusA, err := manager.LoginStatus(loginA.LoginID)
	require.NoError(t, err)
	require.Equal(t, StatusDisconnected, statusA.Status)
	statusB, err := manager.LoginStatus(loginB.LoginID)
	require.NoError(t, err)
	require.Equal(t, StatusConnecting, statusB.Status)

	manager.CancelProviderLogins(ProviderCursor)
	statusB, err = manager.LoginStatus(loginB.LoginID)
	require.NoError(t, err)
	require.Equal(t, StatusDisconnected, statusB.Status)
	var count int64
	require.NoError(t, db.Model(&tables.TableProviderCredential{}).Where("provider = ?", string(ProviderCursor)).Count(&count).Error)
	require.EqualValues(t, 2, count)
}
