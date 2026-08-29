package handlers

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/providercredentials"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestProviderCredentialManagementUsesStandardDashboardAuthPolicy(t *testing.T) {
	handler := &ProviderHandler{inMemoryStore: &lib.Config{
		ProviderCredentialManager: providercredentials.NewManager(nil),
	}}
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("provider", string(schemas.OpenAICodex))
	ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)

	manager, provider, ok := handler.providerCredentialRequest(ctx)
	if !ok || manager == nil || provider != schemas.OpenAICodex {
		t.Fatalf("provider credential request rejected in auth-disabled management mode: ok=%v provider=%q", ok, provider)
	}
}

func TestProviderCredentialManagementRejectsUnsupportedProvider(t *testing.T) {
	handler := &ProviderHandler{inMemoryStore: &lib.Config{
		ProviderCredentialManager: providercredentials.NewManager(nil),
	}}
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("provider", string(schemas.Anthropic))

	handler.listProviderCredentials(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", got, fasthttp.StatusBadRequest, ctx.Response.Body())
	}
}

func TestProviderCredentialManagementAcceptsCursor(t *testing.T) {
	handler := &ProviderHandler{inMemoryStore: &lib.Config{
		ProviderCredentialManager: providercredentials.NewManager(nil),
	}}
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("provider", string(schemas.CursorProvider))

	manager, provider, ok := handler.providerCredentialRequest(ctx)
	if !ok || manager == nil || provider != schemas.CursorProvider {
		t.Fatalf("Cursor credential request rejected: ok=%v provider=%q", ok, provider)
	}
}

func TestProviderCredentialLoginRoutesBindLoginToProviderAndCredential(t *testing.T) {
	ctx := context.Background()
	store, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "provider-login.db")},
	}, &testLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	provider := providercredentials.ProviderCursor
	keys := []schemas.Key{
		{ID: "account-a", Name: "cursor-account-a", Value: *schemas.NewSecretVar("")},
		{ID: "account-b", Name: "cursor-account-b", Value: *schemas.NewSecretVar("")},
	}
	require.NoError(t, store.AddProvider(ctx, provider, configstore.ProviderConfig{Keys: keys}))
	manager := providercredentials.NewManager(store, providercredentials.WithCursorPollInterval(time.Hour))
	login, err := manager.StartLogin(ctx, provider, "account-a")
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.CancelLogin(login.LoginID) })

	handler := &ProviderHandler{inMemoryStore: &lib.Config{
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			provider:                        {Keys: keys},
			providercredentials.ProviderXAI: {Keys: []schemas.Key{{ID: "account-a", Name: "xai-account-a", Value: *schemas.NewSecretVar("")}}},
		},
		ProviderCredentialManager: manager,
	}}

	wrongProvider := &fasthttp.RequestCtx{}
	wrongProvider.SetUserValue("provider", string(providercredentials.ProviderXAI))
	wrongProvider.SetUserValue("credential_id", "account-a")
	wrongProvider.SetUserValue("login_id", login.LoginID)
	handler.getProviderCredentialLogin(wrongProvider)
	require.Equal(t, fasthttp.StatusNotFound, wrongProvider.Response.StatusCode())

	wrongCredential := &fasthttp.RequestCtx{}
	wrongCredential.SetUserValue("provider", string(provider))
	wrongCredential.SetUserValue("credential_id", "account-b")
	wrongCredential.SetUserValue("login_id", login.LoginID)
	handler.getProviderCredentialLogin(wrongCredential)
	require.Equal(t, fasthttp.StatusNotFound, wrongCredential.Response.StatusCode())

	missingKey := &fasthttp.RequestCtx{}
	missingKey.SetUserValue("provider", string(provider))
	missingKey.SetUserValue("credential_id", "deleted-account")
	missingKey.SetUserValue("login_id", login.LoginID)
	handler.getProviderCredentialLogin(missingKey)
	require.Equal(t, fasthttp.StatusNotFound, missingKey.Response.StatusCode())

	wrongCancel := &fasthttp.RequestCtx{}
	wrongCancel.SetUserValue("provider", string(provider))
	wrongCancel.SetUserValue("credential_id", "account-b")
	wrongCancel.SetUserValue("login_id", login.LoginID)
	handler.cancelProviderCredentialLogin(wrongCancel)
	require.Equal(t, fasthttp.StatusNotFound, wrongCancel.Response.StatusCode())
	stillConnecting, err := manager.LoginStatus(login.LoginID)
	require.NoError(t, err)
	require.Equal(t, providercredentials.StatusConnecting, stillConnecting.Status)

	matching := &fasthttp.RequestCtx{}
	matching.SetUserValue("provider", string(provider))
	matching.SetUserValue("credential_id", "account-a")
	matching.SetUserValue("login_id", login.LoginID)
	handler.getProviderCredentialLogin(matching)
	require.Equal(t, fasthttp.StatusOK, matching.Response.StatusCode())

	matchingCancel := &fasthttp.RequestCtx{}
	matchingCancel.SetUserValue("provider", string(provider))
	matchingCancel.SetUserValue("credential_id", "account-a")
	matchingCancel.SetUserValue("login_id", login.LoginID)
	handler.cancelProviderCredentialLogin(matchingCancel)
	require.Equal(t, fasthttp.StatusNoContent, matchingCancel.Response.StatusCode())
}
