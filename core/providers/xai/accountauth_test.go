package xai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

type xaiFixtureLogger struct{}

func (xaiFixtureLogger) Debug(string, ...any)                   {}
func (xaiFixtureLogger) Info(string, ...any)                    {}
func (xaiFixtureLogger) Warn(string, ...any)                    {}
func (xaiFixtureLogger) Error(string, ...any)                   {}
func (xaiFixtureLogger) Fatal(string, ...any)                   {}
func (xaiFixtureLogger) SetLevel(schemas.LogLevel)              {}
func (xaiFixtureLogger) SetOutputType(schemas.LoggerOutputType) {}
func (xaiFixtureLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

type xaiResolverCall struct {
	credentialID string
	forceRefresh bool
}

type xaiFixtureResolver struct {
	mu          sync.Mutex
	credentials map[string]string
	refreshed   map[string]string
	calls       []xaiResolverCall
}

func (resolver *xaiFixtureResolver) ResolveProviderCredential(_ context.Context, provider schemas.ModelProvider, credentialID string, forceRefresh bool) (schemas.ResolvedProviderCredential, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if provider != schemas.XAI {
		panic("unexpected provider: " + provider)
	}
	resolver.calls = append(resolver.calls, xaiResolverCall{credentialID: credentialID, forceRefresh: forceRefresh})
	token := resolver.credentials[credentialID]
	if forceRefresh {
		token = resolver.refreshed[credentialID]
	}
	return schemas.ResolvedProviderCredential{AccessToken: token, ExtraHeaders: map[string]string{"X-Account-Fixture": credentialID}}, nil
}

func newXAIProviderForServer(t *testing.T, url string) *XAIProvider {
	t.Helper()
	provider, err := NewXAIProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL:             url,
		AllowPrivateNetwork: true,
	}}, xaiFixtureLogger{})
	if err != nil {
		t.Fatalf("NewXAIProvider() error = %v", err)
	}
	return provider
}

func TestListModelsUsesDistinctAccountCredentials(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		seen[request.Header.Get("X-Account-Fixture")] = request.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"grok-4","object":"model","owned_by":"xai"}]}`))
	}))
	defer server.Close()

	resolver := &xaiFixtureResolver{credentials: map[string]string{"key-a": "token-a", "key-b": "token-b"}}
	provider := newXAIProviderForServer(t, server.URL)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.ListModels(ctx, []schemas.Key{
		{ID: "key-a", Models: schemas.WhiteList{"grok-4"}, CredentialResolver: resolver},
		{ID: "key-b", Models: schemas.WhiteList{"grok-4"}, CredentialResolver: resolver},
	}, &schemas.BifrostListModelsRequest{Provider: schemas.XAI})
	if bifrostErr != nil {
		t.Fatalf("ListModels() error = %v", bifrostErr)
	}
	if len(response.Data) != 1 || response.Data[0].ID != "xai/grok-4" {
		t.Fatalf("ListModels() data = %#v", response.Data)
	}
	wantSeen := map[string]string{"key-a": "Bearer token-a", "key-b": "Bearer token-b"}
	if len(seen) != len(wantSeen) || seen["key-a"] != wantSeen["key-a"] || seen["key-b"] != wantSeen["key-b"] {
		t.Fatalf("account headers = %#v, want %#v", seen, wantSeen)
	}
	ids := []string{resolver.calls[0].credentialID, resolver.calls[1].credentialID}
	sort.Strings(ids)
	if ids[0] != "key-a" || ids[1] != "key-b" {
		t.Fatalf("resolved credential IDs = %#v", ids)
	}
}

func TestListModelsRefreshesOnceOnlyForResolverCredentials(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") == "Bearer stale" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","error":"expired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"grok-4","object":"model","owned_by":"xai"}]}`))
	}))
	defer server.Close()

	resolver := &xaiFixtureResolver{credentials: map[string]string{"key-a": "stale"}, refreshed: map[string]string{"key-a": "fresh"}}
	provider := newXAIProviderForServer(t, server.URL)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.ListModels(ctx, []schemas.Key{{
		ID: "key-a", Models: schemas.WhiteList{"grok-4"}, CredentialResolver: resolver,
	}}, &schemas.BifrostListModelsRequest{Provider: schemas.XAI})
	if bifrostErr != nil {
		t.Fatalf("ListModels() error = %v", bifrostErr)
	}
	if requests != 2 {
		t.Fatalf("upstream request count = %d, want 2", requests)
	}
	if len(resolver.calls) != 2 || resolver.calls[0].forceRefresh || !resolver.calls[1].forceRefresh {
		t.Fatalf("resolver calls = %#v, want cached then forced refresh", resolver.calls)
	}
}

func TestAPIKeyUnauthorizedDoesNotUseResolverRetry(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","error":"bad api key"}`))
	}))
	defer server.Close()

	provider := newXAIProviderForServer(t, server.URL)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.ListModels(ctx, []schemas.Key{{
		ID: "api-key", Value: schemas.SecretVar{Val: "static"}, Models: schemas.WhiteList{"grok-4"},
	}}, &schemas.BifrostListModelsRequest{Provider: schemas.XAI})
	if bifrostErr == nil {
		t.Fatal("ListModels() error = nil, want upstream unauthorized")
	}
	if requests != 1 {
		t.Fatalf("upstream request count = %d, want 1", requests)
	}
}
