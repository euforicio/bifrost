package schemas

import "context"

// ResolvedProviderCredential contains runtime authentication material for a
// provider account. Resolver implementations own caching and refresh policy;
// callers request a forced refresh only after an upstream authentication
// failure.
type ResolvedProviderCredential struct {
	AccessToken  string
	AccountID    string
	ExtraHeaders map[string]string
}

// ProviderCredentialResolver resolves provider-issued credentials bound to a
// configured key ID.
type ProviderCredentialResolver interface {
	ResolveProviderCredential(ctx context.Context, provider ModelProvider, credentialID string, forceRefresh bool) (ResolvedProviderCredential, error)
}
