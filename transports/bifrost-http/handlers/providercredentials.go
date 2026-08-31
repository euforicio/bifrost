package handlers

import (
	"errors"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/providercredentials"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

func (h *ProviderHandler) listProviderCredentials(ctx *fasthttp.RequestCtx) {
	manager, provider, ok := h.providerCredentialRequest(ctx)
	if !ok {
		return
	}
	credentials, err := manager.List(ctx, provider)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "Failed to list provider accounts")
		return
	}
	SendJSON(ctx, map[string]any{"credentials": credentials, "total": len(credentials)})
}

func (h *ProviderHandler) startProviderCredentialLogin(ctx *fasthttp.RequestCtx) {
	manager, provider, ok := h.providerCredentialRequest(ctx)
	if !ok {
		return
	}
	credentialID, ok := h.providerCredentialID(ctx, provider)
	if !ok {
		return
	}
	status, err := manager.StartDeviceLogin(ctx, provider, credentialID)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadGateway, "Provider device authorization could not be started")
		return
	}
	SendJSONWithStatus(ctx, status, fasthttp.StatusAccepted)
}

func (h *ProviderHandler) startProviderCredentialBrowserLogin(ctx *fasthttp.RequestCtx) {
	manager, provider, ok := h.providerCredentialRequest(ctx)
	if !ok {
		return
	}
	credentialID, ok := h.providerCredentialID(ctx, provider)
	if !ok {
		return
	}
	if provider != providercredentials.ProviderCursor {
		SendError(ctx, fasthttp.StatusBadRequest, "Provider does not support browser authorization")
		return
	}
	status, err := manager.StartLogin(ctx, provider, credentialID)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadGateway, "Provider browser authorization could not be started")
		return
	}
	SendJSONWithStatus(ctx, status, fasthttp.StatusAccepted)
}

func (h *ProviderHandler) getProviderCredentialLogin(ctx *fasthttp.RequestCtx) {
	manager, provider, ok := h.providerCredentialRequest(ctx)
	if !ok {
		return
	}
	credentialID, ok := h.providerCredentialID(ctx, provider)
	if !ok {
		return
	}
	loginID := fmt.Sprint(ctx.UserValue("login_id"))
	status, err := manager.LoginStatus(loginID)
	if err != nil {
		SendError(ctx, fasthttp.StatusNotFound, "Provider login not found")
		return
	}
	if status.Provider != string(provider) || status.CredentialID != credentialID {
		SendError(ctx, fasthttp.StatusNotFound, "Provider login not found")
		return
	}
	SendJSON(ctx, status)
}

func (h *ProviderHandler) cancelProviderCredentialLogin(ctx *fasthttp.RequestCtx) {
	manager, provider, ok := h.providerCredentialRequest(ctx)
	if !ok {
		return
	}
	credentialID, ok := h.providerCredentialID(ctx, provider)
	if !ok {
		return
	}
	loginID := fmt.Sprint(ctx.UserValue("login_id"))
	status, err := manager.LoginStatus(loginID)
	if err != nil || status.Provider != string(provider) || status.CredentialID != credentialID {
		SendError(ctx, fasthttp.StatusNotFound, "Provider login not found")
		return
	}
	if err := manager.CancelLogin(loginID); err != nil {
		SendError(ctx, fasthttp.StatusNotFound, "Provider login not found")
		return
	}
	ctx.SetStatusCode(fasthttp.StatusNoContent)
}

func (h *ProviderHandler) getProviderCredentialStatus(ctx *fasthttp.RequestCtx) {
	manager, provider, ok := h.providerCredentialRequest(ctx)
	if !ok {
		return
	}
	credentialID, ok := h.providerCredentialID(ctx, provider)
	if !ok {
		return
	}
	status, err := manager.Status(ctx, provider, credentialID)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "Failed to load provider account status")
		return
	}
	SendJSON(ctx, status)
}

func (h *ProviderHandler) getProviderCredentialUsage(ctx *fasthttp.RequestCtx) {
	manager, provider, ok := h.providerCredentialRequest(ctx)
	if !ok {
		return
	}
	credentialID, ok := h.providerCredentialID(ctx, provider)
	if !ok {
		return
	}
	SendJSON(ctx, manager.Usage(ctx, provider, credentialID))
}

func (h *ProviderHandler) refreshProviderCredential(ctx *fasthttp.RequestCtx) {
	manager, provider, ok := h.providerCredentialRequest(ctx)
	if !ok {
		return
	}
	credentialID, ok := h.providerCredentialID(ctx, provider)
	if !ok {
		return
	}
	status, err := manager.Refresh(ctx, provider, credentialID)
	if err != nil {
		SendError(ctx, fasthttp.StatusUnauthorized, "Provider account must be reconnected")
		return
	}
	SendJSON(ctx, status)
}

func (h *ProviderHandler) deleteProviderCredential(ctx *fasthttp.RequestCtx) {
	manager, provider, ok := h.providerCredentialRequest(ctx)
	if !ok {
		return
	}
	credentialID, ok := h.providerCredentialID(ctx, provider)
	if !ok {
		return
	}
	if err := manager.Disconnect(ctx, provider, credentialID); err != nil {
		if errors.Is(err, providercredentials.ErrCredentialNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, "Provider account not found")
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, "Failed to disconnect provider account")
		return
	}
	ctx.SetStatusCode(fasthttp.StatusNoContent)
}

func (h *ProviderHandler) providerCredentialRequest(ctx *fasthttp.RequestCtx) (*providercredentials.Manager, schemas.ModelProvider, bool) {
	// Use the same dashboard-auth policy as the rest of the management API.
	// When dashboard auth is enabled, the registered middleware requires a
	// valid session. Auth-disabled deployments intentionally expose management
	// routes and must still be able to bootstrap provider accounts from the UI.
	if h.inMemoryStore == nil || h.inMemoryStore.ProviderCredentialManager == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "Provider credential storage is not available")
		return nil, "", false
	}
	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Invalid provider")
		return nil, "", false
	}
	if provider != providercredentials.ProviderOpenAICodex && provider != providercredentials.ProviderXAI && provider != providercredentials.ProviderCursor {
		SendError(ctx, fasthttp.StatusBadRequest, "Provider does not support account login")
		return nil, "", false
	}
	return h.inMemoryStore.ProviderCredentialManager, provider, true
}

func (h *ProviderHandler) providerCredentialID(ctx *fasthttp.RequestCtx, provider schemas.ModelProvider) (string, bool) {
	credentialID := fmt.Sprint(ctx.UserValue("credential_id"))
	if credentialID == "" || credentialID == "<nil>" {
		SendError(ctx, fasthttp.StatusBadRequest, "credential_id is required")
		return "", false
	}
	if _, err := h.inMemoryStore.GetProviderKeyRaw(provider, credentialID); err != nil {
		if errors.Is(err, lib.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, "Provider key not found")
			return "", false
		}
		SendError(ctx, fasthttp.StatusInternalServerError, "Failed to load provider key")
		return "", false
	}
	return credentialID, true
}
