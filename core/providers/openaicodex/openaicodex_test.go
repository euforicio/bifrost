package openaicodex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

type fixtureLogger struct{}

func (fixtureLogger) Debug(string, ...any)                   {}
func (fixtureLogger) Info(string, ...any)                    {}
func (fixtureLogger) Warn(string, ...any)                    {}
func (fixtureLogger) Error(string, ...any)                   {}
func (fixtureLogger) Fatal(string, ...any)                   {}
func (fixtureLogger) SetLevel(schemas.LogLevel)              {}
func (fixtureLogger) SetOutputType(schemas.LoggerOutputType) {}
func (fixtureLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

type resolverCall struct {
	provider     schemas.ModelProvider
	credentialID string
	forceRefresh bool
}

type fixtureCredentialResolver struct {
	mu          sync.Mutex
	credentials map[string]schemas.ResolvedProviderCredential
	refreshed   map[string]schemas.ResolvedProviderCredential
	calls       []resolverCall
}

func (resolver *fixtureCredentialResolver) ResolveProviderCredential(_ context.Context, provider schemas.ModelProvider, credentialID string, forceRefresh bool) (schemas.ResolvedProviderCredential, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.calls = append(resolver.calls, resolverCall{provider: provider, credentialID: credentialID, forceRefresh: forceRefresh})
	if forceRefresh {
		return resolver.refreshed[credentialID], nil
	}
	return resolver.credentials[credentialID], nil
}

func newProviderForServer(t *testing.T, url string) *OpenAICodexProvider {
	t.Helper()
	provider, err := NewOpenAICodexProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL:             url,
		AllowPrivateNetwork: true,
	}}, fixtureLogger{})
	if err != nil {
		t.Fatalf("NewOpenAICodexProvider() error = %v", err)
	}
	return provider
}

func completedResponse(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "resp_fixture", "object": "response", "status": "completed",
		"model": model, "output": []any{},
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
	})
}

func responsesRequest() *schemas.BifrostResponsesRequest {
	return &schemas.BifrostResponsesRequest{
		Provider: schemas.OpenAICodex,
		Model:    "gpt-5-codex",
		Input: []schemas.ResponsesMessage{{
			Type:    schemas.Ptr(schemas.ResponsesMessageTypeMessage),
			Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
			Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hi")},
		}},
	}
}

func TestResponsesResolveDistinctKeyAccounts(t *testing.T) {
	var mu sync.Mutex
	seen := make([][4]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		seen = append(seen, [4]string{
			request.Header.Get("Authorization"),
			request.Header.Get("ChatGPT-Account-ID"),
			request.Header.Get("originator"),
			request.Header.Get("X-OpenAI-Fedramp"),
		})
		mu.Unlock()
		completedResponse(w, "gpt-5-codex")
	}))
	defer server.Close()

	resolver := &fixtureCredentialResolver{credentials: map[string]schemas.ResolvedProviderCredential{
		"key-a": {AccessToken: "token-a", AccountID: "account-a", ExtraHeaders: map[string]string{"X-OpenAI-Fedramp": "true"}},
		"key-b": {AccessToken: "token-b", AccountID: "account-b"},
	}}
	provider := newProviderForServer(t, server.URL+"/responses")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	request := responsesRequest()
	for _, keyID := range []string{"key-a", "key-b"} {
		if _, bifrostErr := provider.Responses(ctx, schemas.Key{ID: keyID, CredentialResolver: resolver}, request); bifrostErr != nil {
			t.Fatalf("Responses(%s) error = %v", keyID, bifrostErr)
		}
	}

	wantHeaders := [][4]string{
		{"Bearer token-a", "account-a", "bifrost", "true"},
		{"Bearer token-b", "account-b", "bifrost", ""},
	}
	if !reflect.DeepEqual(seen, wantHeaders) {
		t.Fatalf("request headers = %#v, want %#v", seen, wantHeaders)
	}
	wantCalls := []resolverCall{
		{provider: schemas.OpenAICodex, credentialID: "key-a", forceRefresh: false},
		{provider: schemas.OpenAICodex, credentialID: "key-b", forceRefresh: false},
	}
	if !reflect.DeepEqual(resolver.calls, wantCalls) {
		t.Fatalf("resolver calls = %#v, want %#v", resolver.calls, wantCalls)
	}
}

func TestResponsesRefreshesExactlyOnceOnUnauthorized(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") == "Bearer stale" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"expired"}}`))
			return
		}
		completedResponse(w, "gpt-5-codex")
	}))
	defer server.Close()

	resolver := &fixtureCredentialResolver{
		credentials: map[string]schemas.ResolvedProviderCredential{"key-a": {AccessToken: "stale", AccountID: "account-a"}},
		refreshed:   map[string]schemas.ResolvedProviderCredential{"key-a": {AccessToken: "fresh", AccountID: "account-a"}},
	}
	provider := newProviderForServer(t, server.URL+"/responses")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Responses(ctx, schemas.Key{ID: "key-a", CredentialResolver: resolver}, responsesRequest())
	if bifrostErr != nil {
		t.Fatalf("Responses() error = %v", bifrostErr)
	}
	if requests != 2 {
		t.Fatalf("upstream request count = %d, want 2", requests)
	}
	wantCalls := []resolverCall{
		{provider: schemas.OpenAICodex, credentialID: "key-a", forceRefresh: false},
		{provider: schemas.OpenAICodex, credentialID: "key-a", forceRefresh: true},
	}
	if !reflect.DeepEqual(resolver.calls, wantCalls) {
		t.Fatalf("resolver calls = %#v, want %#v", resolver.calls, wantCalls)
	}
}

func TestResponsesStreamRefreshesOnceOnInitialUnauthorized(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") == "Bearer stale" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			`data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_fixture","object":"response","status":"completed","model":"gpt-5-codex","output":[]}}` + "\n\n"))
	}))
	defer server.Close()

	resolver := &fixtureCredentialResolver{
		credentials: map[string]schemas.ResolvedProviderCredential{"key-a": {AccessToken: "stale", AccountID: "account-a"}},
		refreshed:   map[string]schemas.ResolvedProviderCredential{"key-a": {AccessToken: "fresh", AccountID: "account-a"}},
	}
	provider := newProviderForServer(t, server.URL+"/responses")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	stream, bifrostErr := provider.ResponsesStream(ctx, func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, err
	}, nil, schemas.Key{ID: "key-a", CredentialResolver: resolver}, responsesRequest())
	if bifrostErr != nil {
		t.Fatalf("ResponsesStream() error = %v", bifrostErr)
	}
	var chunks int
	for range stream {
		chunks++
	}
	if chunks == 0 {
		t.Fatal("ResponsesStream() produced no chunks")
	}
	if requests != 2 {
		t.Fatalf("upstream request count = %d, want 2", requests)
	}
	if len(resolver.calls) != 2 || resolver.calls[0].forceRefresh || !resolver.calls[1].forceRefresh {
		t.Fatalf("resolver calls = %#v, want cached then forced refresh", resolver.calls)
	}
}

func TestListModelsIsLocalAndUnsupportedOperationsAreExplicit(t *testing.T) {
	provider := newProviderForServer(t, "http://127.0.0.1:1/responses")
	response, bifrostErr := provider.ListModels(nil, []schemas.Key{
		{Models: schemas.WhiteList{"gpt-5-codex", "gpt-5-codex-mini"}, BlacklistedModels: schemas.BlackList{"gpt-5-codex-mini"}},
		{Models: schemas.WhiteList{"*"}},
	}, &schemas.BifrostListModelsRequest{})
	if bifrostErr != nil {
		t.Fatalf("ListModels() error = %v", bifrostErr)
	}
	if len(response.Data) != 1 || response.Data[0].ID != "openai-codex/gpt-5-codex" {
		t.Fatalf("ListModels() data = %#v", response.Data)
	}
	if _, unsupportedErr := provider.ChatCompletion(nil, schemas.Key{}, nil); unsupportedErr == nil || unsupportedErr.Error == nil || unsupportedErr.Error.Code == nil || *unsupportedErr.Error.Code != "unsupported_operation" {
		t.Fatalf("ChatCompletion() error = %#v, want unsupported_operation", unsupportedErr)
	}
}
