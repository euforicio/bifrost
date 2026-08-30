package openaicodex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/providers/openai"
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
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("event: response.completed\n" +
		`data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_fixture","object":"response","status":"completed","model":"` + model + `","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
}

func responsesRequest() *schemas.BifrostResponsesRequest {
	return &schemas.BifrostResponsesRequest{
		Provider: schemas.OpenAICodex,
		Model:    "gpt-5.6-sol",
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
	seenBodies := make([]map[string]any, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("x-openai-internal-codex-responses-lite") != "true" {
			t.Errorf("missing Responses Lite header")
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		mu.Lock()
		seen = append(seen, [4]string{
			request.Header.Get("Authorization"),
			request.Header.Get("ChatGPT-Account-ID"),
			request.Header.Get("originator"),
			request.Header.Get("X-OpenAI-Fedramp"),
		})
		seenBodies = append(seenBodies, body)
		mu.Unlock()
		completedResponse(w, "gpt-5.6-sol")
	}))
	defer server.Close()

	resolver := &fixtureCredentialResolver{credentials: map[string]schemas.ResolvedProviderCredential{
		"key-a": {AccessToken: "token-a", AccountID: "account-a", ExtraHeaders: map[string]string{"X-OpenAI-Fedramp": "true"}},
		"key-b": {AccessToken: "token-b", AccountID: "account-b"},
	}}
	provider := newProviderForServer(t, server.URL+"/responses")
	request := responsesRequest()
	for _, keyID := range []string{"key-a", "key-b"} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
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
	for _, body := range seenBodies {
		if body["stream"] != true || body["store"] != false || body["parallel_tool_calls"] != false || body["tool_choice"] != "auto" {
			t.Fatalf("Codex request defaults = %#v", body)
		}
		input, ok := body["input"].([]any)
		if !ok || len(input) != 2 || input[0].(map[string]any)["type"] != "additional_tools" {
			t.Fatalf("Codex Responses Lite input = %#v", body["input"])
		}
		if _, ok := body["max_output_tokens"]; ok {
			t.Fatalf("Codex request includes unsupported max_output_tokens: %#v", body)
		}
		include, ok := body["include"].([]any)
		if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
			t.Fatalf("Codex request include = %#v", body["include"])
		}
	}
}

func TestResponsesNormalizesUnsupportedOpenAIParameters(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		completedResponse(w, "gpt-5.6-sol")
	}))
	defer server.Close()

	maxOutputTokens := 32
	temperature := 0.2
	request := responsesRequest()
	request.Params = &schemas.ResponsesParameters{
		MaxOutputTokens: &maxOutputTokens,
		Temperature:     &temperature,
		Metadata:        &map[string]any{"source": "fixture"},
		ExtraParams:     map[string]any{"unsupported_extension": true},
	}
	resolver := &fixtureCredentialResolver{credentials: map[string]schemas.ResolvedProviderCredential{
		"key-a": {AccessToken: "token-a", AccountID: "account-a"},
	}}
	provider := newProviderForServer(t, server.URL+"/responses")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.Responses(ctx, schemas.Key{ID: "key-a", CredentialResolver: resolver}, request)
	if bifrostErr != nil {
		t.Fatalf("Responses() error = %v", bifrostErr)
	}
	if response == nil || response.ID == nil || *response.ID != "resp_fixture" || response.Model != "gpt-5.6-sol" {
		t.Fatalf("Responses() = %#v", response)
	}
	for _, field := range []string{"max_output_tokens", "temperature", "metadata", "unsupported_extension"} {
		if _, ok := body[field]; ok {
			t.Fatalf("Codex request includes unsupported %s: %#v", field, body)
		}
	}
}

func TestNormalizeCodexResponsesRequestPreservesSupportedFieldsWithoutMutation(t *testing.T) {
	parallel := false
	store := true
	maxOutputTokens := 32
	toolChoice := &schemas.ResponsesToolChoice{ResponsesToolChoiceStr: schemas.Ptr("none")}
	original := &openai.OpenAIResponsesRequest{
		Model: "gpt-5.6-sol",
		ResponsesParameters: schemas.ResponsesParameters{
			Instructions:      schemas.Ptr("be concise"),
			Include:           []string{"custom.include"},
			ParallelToolCalls: &parallel,
			ToolChoice:        toolChoice,
			Store:             &store,
			MaxOutputTokens:   &maxOutputTokens,
		},
	}
	normalized := normalizeCodexResponsesRequest(original, false)
	if normalized == original {
		t.Fatal("normalizer returned the caller-owned request")
	}
	if normalized.Model != original.Model || normalized.Instructions != original.Instructions || normalized.ToolChoice == nil || normalized.ToolChoice.ResponsesToolChoiceStr == nil || *normalized.ToolChoice.ResponsesToolChoiceStr != "auto" {
		t.Fatalf("supported fields were not preserved: %#v", normalized)
	}
	if normalized.ParallelToolCalls == nil || *normalized.ParallelToolCalls || normalized.Stream == nil || !*normalized.Stream || normalized.Store == nil || *normalized.Store {
		t.Fatalf("normalized flags = %#v", normalized.ResponsesParameters)
	}
	if len(normalized.Include) != 1 || normalized.Include[0] != "custom.include" {
		t.Fatalf("include = %#v", normalized.Include)
	}
	if normalized.MaxOutputTokens != nil {
		t.Fatal("unsupported max_output_tokens survived normalization")
	}
	if original.Stream != nil || original.MaxOutputTokens == nil || original.Store == nil || !*original.Store {
		t.Fatalf("caller request was mutated: %#v", original)
	}
}

func TestResponsesLiteMovesInstructionsAndFunctionToolsIntoInput(t *testing.T) {
	strict := true
	request := &openai.OpenAIResponsesRequest{
		Model: "gpt-5.6-sol",
		Input: openai.OpenAIResponsesRequestInput{OpenAIResponsesRequestInputStr: schemas.Ptr("use the tool")},
		ResponsesParameters: schemas.ResponsesParameters{
			Instructions: schemas.Ptr("follow the contract"),
			Tools: []schemas.ResponsesTool{{
				Type:        schemas.ResponsesToolTypeFunction,
				Name:        schemas.Ptr("echo"),
				Description: schemas.Ptr("echo a value"),
				ResponsesToolFunction: &schemas.ResponsesToolFunction{
					Strict: &strict,
					Parameters: &schemas.ToolFunctionParameters{
						Type:       "object",
						Properties: schemas.NewOrderedMapFromPairs(schemas.KV("value", schemas.NewOrderedMapFromPairs(schemas.KV("type", "string")))),
						Required:   []string{"value"},
					},
				},
			}},
		},
	}

	normalized := normalizeCodexResponsesRequest(request, true)
	data, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("marshal normalized request: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode normalized request: %v", err)
	}
	if _, ok := body["instructions"]; ok {
		t.Fatalf("Responses Lite retained top-level instructions: %s", data)
	}
	if _, ok := body["tools"]; ok {
		t.Fatalf("Responses Lite retained top-level tools: %s", data)
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v, want false", body["parallel_tool_calls"])
	}
	reasoning := body["reasoning"].(map[string]any)
	if reasoning["context"] != "all_turns" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	input := body["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %#v", input)
	}
	additional := input[0].(map[string]any)
	if additional["type"] != "additional_tools" || additional["role"] != "developer" {
		t.Fatalf("additional_tools item = %#v", additional)
	}
	tools := additional["tools"].([]any)
	namespace := tools[0].(map[string]any)
	if namespace["type"] != "namespace" || namespace["name"] != "functions" {
		t.Fatalf("namespace = %#v", namespace)
	}
	child := namespace["tools"].([]any)[0].(map[string]any)
	if child["type"] != "function" || child["name"] != "echo" {
		t.Fatalf("function tool = %#v", child)
	}
	developer := input[1].(map[string]any)
	if developer["role"] != "developer" || developer["type"] != "message" {
		t.Fatalf("developer instructions = %#v", developer)
	}
	user := input[2].(map[string]any)
	if user["role"] != "user" || user["content"] != "use the tool" {
		t.Fatalf("user input = %#v", user)
	}
	if request.Instructions == nil || len(request.Tools) != 1 || request.Input.OpenAIResponsesRequestInputStr == nil {
		t.Fatalf("caller request was mutated: %#v", request)
	}
}

func TestResponsesReturnsIncompleteTerminalPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.incomplete\n" +
			`data: {"type":"response.incomplete","sequence_number":1,"response":{"id":"resp_incomplete","object":"response","status":"incomplete","model":"gpt-5.6-sol","output":[]}}` + "\n\n"))
	}))
	defer server.Close()
	resolver := &fixtureCredentialResolver{credentials: map[string]schemas.ResolvedProviderCredential{
		"key-a": {AccessToken: "token-a", AccountID: "account-a"},
	}}
	provider := newProviderForServer(t, server.URL+"/responses")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.Responses(ctx, schemas.Key{ID: "key-a", CredentialResolver: resolver}, responsesRequest())
	if bifrostErr != nil {
		t.Fatalf("Responses() error = %v", bifrostErr)
	}
	if response == nil || response.ID == nil || *response.ID != "resp_incomplete" || response.Status == nil || *response.Status != schemas.ResponsesResponseStatusIncomplete {
		t.Fatalf("Responses() = %#v", response)
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
		completedResponse(w, "gpt-5.6-sol")
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

func TestListModelsUsesAccountCatalogAndUnsupportedOperationsAreExplicit(t *testing.T) {
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestPath = request.URL.RequestURI()
		if request.Header.Get("Authorization") != "Bearer token-a" || request.Header.Get("ChatGPT-Account-ID") != "account-a" {
			t.Errorf("model headers = %#v", request.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol","description":"frontier","supported_in_api":true,"visibility":"list","use_responses_lite":true},{"slug":"codex-auto-review","display_name":"Internal Review","supported_in_api":true,"visibility":"hide","use_responses_lite":true},{"slug":"internal-only","display_name":"Internal","supported_in_api":false,"visibility":"hide"}]}`))
	}))
	defer server.Close()

	provider := newProviderForServer(t, server.URL+"/responses")
	resolver := &fixtureCredentialResolver{credentials: map[string]schemas.ResolvedProviderCredential{
		"key-a": {AccessToken: "token-a", AccountID: "account-a"},
	}}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.ListModels(ctx, []schemas.Key{{
		ID: "key-a", Models: schemas.WhiteList{"*"}, CredentialResolver: resolver,
	}}, &schemas.BifrostListModelsRequest{})
	if bifrostErr != nil {
		t.Fatalf("ListModels() error = %v", bifrostErr)
	}
	if !strings.HasPrefix(requestPath, "/models?client_version=") {
		t.Fatalf("models request path = %q", requestPath)
	}
	if len(response.Data) != 1 || response.Data[0].ID != "openai-codex/gpt-5.6-sol" || response.Data[0].Name == nil || *response.Data[0].Name != "GPT-5.6 Sol" {
		t.Fatalf("ListModels() data = %#v", response.Data)
	}
	if _, unsupportedErr := provider.ChatCompletion(nil, schemas.Key{}, nil); unsupportedErr == nil || unsupportedErr.Error == nil || unsupportedErr.Error.Code == nil || *unsupportedErr.Error.Code != "unsupported_operation" {
		t.Fatalf("ChatCompletion() error = %#v, want unsupported_operation", unsupportedErr)
	}
}

func TestListModelsKeepsHealthyAccountWhenAnotherFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer broken" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"account unavailable"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol","supported_in_api":true,"visibility":"list","use_responses_lite":true}]}`))
	}))
	defer server.Close()
	resolver := &fixtureCredentialResolver{credentials: map[string]schemas.ResolvedProviderCredential{
		"broken":  {AccessToken: "broken", AccountID: "account-broken"},
		"healthy": {AccessToken: "healthy", AccountID: "account-healthy"},
	}}
	provider := newProviderForServer(t, server.URL+"/responses")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.ListModels(ctx, []schemas.Key{
		{ID: "broken", Models: schemas.WhiteList{"*"}, CredentialResolver: resolver},
		{ID: "healthy", Models: schemas.WhiteList{"*"}, CredentialResolver: resolver},
	}, &schemas.BifrostListModelsRequest{Provider: schemas.OpenAICodex})
	if bifrostErr != nil {
		t.Fatalf("ListModels() error = %v", bifrostErr)
	}
	if len(response.Data) != 1 || response.Data[0].ID != "openai-codex/gpt-5.6-sol" {
		t.Fatalf("models = %#v", response.Data)
	}
	if len(response.KeyStatuses) != 2 {
		t.Fatalf("key statuses = %#v", response.KeyStatuses)
	}
}

func TestListModelsMergesAndDeduplicatesHealthyAccounts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		unique := "alpha"
		if request.Header.Get("Authorization") == "Bearer account-b" {
			unique = "beta"
		}
		_, _ = fmt.Fprintf(w, `{"models":[{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol","supported_in_api":true,"visibility":"list","use_responses_lite":true},{"slug":%q,"display_name":%q,"supported_in_api":true,"visibility":"list"}]}`, unique, strings.ToUpper(unique))
	}))
	defer server.Close()
	resolver := &fixtureCredentialResolver{credentials: map[string]schemas.ResolvedProviderCredential{
		"account-a": {AccessToken: "account-a", AccountID: "account-a"},
		"account-b": {AccessToken: "account-b", AccountID: "account-b"},
	}}
	provider := newProviderForServer(t, server.URL+"/responses")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.ListModels(ctx, []schemas.Key{
		{ID: "account-a", Models: schemas.WhiteList{"*"}, CredentialResolver: resolver},
		{ID: "account-b", Models: schemas.WhiteList{"*"}, CredentialResolver: resolver},
	}, &schemas.BifrostListModelsRequest{Provider: schemas.OpenAICodex})
	if bifrostErr != nil {
		t.Fatalf("ListModels() error = %v", bifrostErr)
	}
	want := []string{"openai-codex/alpha", "openai-codex/beta", "openai-codex/gpt-5.6-sol"}
	got := make([]string, len(response.Data))
	for i := range response.Data {
		got[i] = response.Data[i].ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %#v, want %#v", got, want)
	}
	if len(response.KeyStatuses) != 2 {
		t.Fatalf("key statuses = %#v", response.KeyStatuses)
	}
	for _, status := range response.KeyStatuses {
		if status.Status != schemas.KeyStatusSuccess {
			t.Fatalf("key status = %#v", status)
		}
	}
}

func TestListModelsRefreshesOnceThenReturnsUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"expired"}}`))
	}))
	defer server.Close()
	resolver := &fixtureCredentialResolver{
		credentials: map[string]schemas.ResolvedProviderCredential{"key-a": {AccessToken: "stale", AccountID: "account-a"}},
		refreshed:   map[string]schemas.ResolvedProviderCredential{"key-a": {AccessToken: "fresh", AccountID: "account-a"}},
	}
	provider := newProviderForServer(t, server.URL+"/responses")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.ListModels(ctx, []schemas.Key{{ID: "key-a", Models: schemas.WhiteList{"*"}, CredentialResolver: resolver}}, &schemas.BifrostListModelsRequest{Provider: schemas.OpenAICodex})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ListModels() error = %#v", bifrostErr)
	}
	if len(resolver.calls) != 2 || resolver.calls[0].forceRefresh || !resolver.calls[1].forceRefresh {
		t.Fatalf("resolver calls = %#v", resolver.calls)
	}
}

func TestListModelsRejectsMissingAndOversizedCatalogs(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "missing models",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			},
		},
		{
			name: "oversized fixed length",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				payload := strings.Repeat("x", maxModelsBodyBytes+1)
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				_, _ = w.Write([]byte(payload))
			},
		},
		{
			name: "oversized chunked",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				flusher, _ := w.(http.Flusher)
				chunk := strings.Repeat("x", 64<<10)
				for written := 0; written <= maxModelsBodyBytes; written += len(chunk) {
					_, _ = w.Write([]byte(chunk))
					if flusher != nil {
						flusher.Flush()
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			provider := newProviderForServer(t, server.URL+"/responses")
			resolver := &fixtureCredentialResolver{credentials: map[string]schemas.ResolvedProviderCredential{
				"key-a": {AccessToken: "token-a", AccountID: "account-a"},
			}}
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := provider.ListModels(ctx, []schemas.Key{{ID: "key-a", Models: schemas.WhiteList{"*"}, CredentialResolver: resolver}}, &schemas.BifrostListModelsRequest{Provider: schemas.OpenAICodex})
			if bifrostErr == nil {
				t.Fatal("ListModels() succeeded for invalid catalog")
			}
		})
	}
}
