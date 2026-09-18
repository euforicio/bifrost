package typesafe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
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

func newProviderForServer(t *testing.T, url string) *TypeSafeProvider {
	t.Helper()
	return NewTypeSafeProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:             url,
			AllowPrivateNetwork: true,
		},
	}, fixtureLogger{})
}

func testKey() schemas.Key {
	return schemas.Key{
		Value:  *schemas.NewSecretVar("test-typesafe-key"),
		Models: schemas.WhiteList{"*"},
	}
}

func TestChatCompletionUnsupported(t *testing.T) {
	provider := newProviderForServer(t, "http://127.0.0.1:1")
	resp, err := provider.ChatCompletion(schemas.NewBifrostContext(context.Background(), time.Time{}), testKey(), &schemas.BifrostChatRequest{
		Provider: schemas.TypeSafe,
		Model:    defaultModel,
	})
	assert.Nil(t, resp)
	require.NotNil(t, err)
	assert.Contains(t, err.Error.Message, "not supported")
}

func TestToTypeSafeSystemOneRequestDefaultsModel(t *testing.T) {
	req, err := ToTypeSafeSystemOneRequest(&schemas.BifrostSystemOneRequest{
		State: json.RawMessage(`"hello"`),
		Questions: map[string]json.RawMessage{
			"complexity_tier": json.RawMessage(`{"type":"choice","instructions":"pick a tier"}`),
		},
	})
	require.NoError(t, err)
	require.NotNil(t, req)
	assert.Equal(t, defaultModel, req.Model)
	assert.Equal(t, json.RawMessage(`"hello"`), req.State)
	require.Contains(t, req.Questions, "complexity_tier")
	assert.Equal(t, "choice", req.Questions["complexity_tier"].Type)
}

func TestToBifrostSystemOneResponsePreservesDocumentedFields(t *testing.T) {
	choice := "MEDIUM"
	confidence := 0.81
	noul := 0.22
	inTok, outTok := 12, 3
	converted, err := ToBifrostSystemOneResponse(&TypeSafeSystemOneResponse{
		Model: modelJev113,
		Answers: map[string]TypeSafeSystemOneAnswer{
			"complexity_tier": {Type: "choice", Choice: &choice, Confidence: &confidence, Probabilities: map[string]float64{"MEDIUM": 0.81, "SIMPLE": 0.12, "COMPLEX": 0.07}},
			"needs_frontier":  {Type: "noul", Noul: &noul},
		},
		Usage: &TypeSafeSystemOneUsage{InputTokens: &inTok, OutputTokens: &outTok},
	})
	require.NoError(t, err)
	require.NotNil(t, converted)
	assert.Equal(t, modelJev113, converted.Model)
	require.NotNil(t, converted.Usage)
	assert.Equal(t, 12, *converted.Usage.InputTokens)
	assert.Equal(t, 3, *converted.Usage.OutputTokens)

	var choiceAns jevWireChoice
	require.NoError(t, json.Unmarshal(converted.Answers["complexity_tier"], &choiceAns))
	assert.Equal(t, "choice", choiceAns.Type)
	assert.Equal(t, "MEDIUM", choiceAns.Choice)
	require.NotNil(t, choiceAns.Confidence)
	assert.Equal(t, 0.81, *choiceAns.Confidence)

	var noulAns jevWireNoul
	require.NoError(t, json.Unmarshal(converted.Answers["needs_frontier"], &noulAns))
	assert.Equal(t, "noul", noulAns.Type)
	require.NotNil(t, noulAns.Noul)
	assert.Equal(t, 0.22, *noulAns.Noul)
	assert.False(t, jsonHasField(converted.Answers["needs_frontier"], "confidence"), "noul answers have no confidence field")
}

type jevWireChoice struct {
	Type       string   `json:"type"`
	Choice     string   `json:"choice"`
	Confidence *float64 `json:"confidence"`
}

type jevWireNoul struct {
	Type string   `json:"type"`
	Noul *float64 `json:"noul"`
}

func jsonHasField(raw json.RawMessage, field string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	_, ok := obj[field]
	return ok
}

func TestSystemOnePostsDocumentedWireShape(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NoError(t, json.Unmarshal(body, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {
				"complexity_tier": {
					"type": "choice",
					"choice": "SIMPLE",
					"probabilities": {"SIMPLE": 0.91, "MEDIUM": 0.06, "COMPLEX": 0.03},
					"confidence": 0.88
				},
				"needs_frontier": {"type": "noul", "noul": 0.11}
			},
			"usage": {"input_tokens": 40, "output_tokens": 8}
		}`))
	}))
	defer server.Close()

	provider := newProviderForServer(t, server.URL)
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	resp, bifrostErr := provider.SystemOne(ctx, testKey(), &schemas.BifrostSystemOneRequest{
		Provider: schemas.TypeSafe,
		Model:    defaultModel,
		State:    json.RawMessage(`"what is a mutex?"`),
		Questions: map[string]json.RawMessage{
			"complexity_tier": json.RawMessage(`{"type":"choice","instructions":{"question":"Which complexity tier should handle this user request?"},"criteria":{"SIMPLE":{"what":"trivial"}}}`),
			"needs_frontier":  json.RawMessage(`{"type":"noul","instructions":"frontier?"}`),
		},
	})
	require.Nil(t, bifrostErr)
	require.NotNil(t, resp)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, systemOnePath, gotPath)
	assert.Equal(t, "Bearer test-typesafe-key", gotAuth)
	assert.Equal(t, "jev-latest", gotBody["model"])
	assert.Equal(t, "what is a mutex?", gotBody["state"])
	questions, ok := gotBody["questions"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, questions, "complexity_tier")
	require.Contains(t, questions, "needs_frontier")
	assert.Equal(t, modelJev113, resp.Model)
	require.Contains(t, resp.Answers, "complexity_tier")
	require.Contains(t, resp.Answers, "needs_frontier")
}

func TestListModelsUsesDocumentedAliasShape(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": [
				{"name": "jev-latest", "description": "Current Jev alias", "release_date": "2026-09-01"},
				{"name": "jev-preview", "description": "Preview alias", "release_date": "2026-09-01"}
			]
		}`))
	}))
	defer server.Close()

	provider := newProviderForServer(t, server.URL)
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	resp, bifrostErr := provider.ListModels(ctx, []schemas.Key{testKey()}, &schemas.BifrostListModelsRequest{})
	require.Nil(t, bifrostErr)
	require.NotNil(t, resp)
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, listModelsPath, gotPath)
	assert.Equal(t, "Bearer test-typesafe-key", gotAuth)
	require.Len(t, resp.Data, 2)
	assert.Equal(t, "jev-latest", resp.Data[0].ID)
	assert.Equal(t, "jev-preview", resp.Data[1].ID)
}

func TestSystemOneErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantSubstr string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"message":"invalid api key"}`, wantSubstr: "invalid api key"},
		{name: "validation", status: 422, body: `{"detail":"questions required"}`, wantSubstr: "questions required"},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{"error":"slow down"}`, wantSubstr: "slow down"},
		{name: "overloaded", status: statusOverloaded, body: `{"message":"overloaded"}`, wantSubstr: "overloaded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			provider := newProviderForServer(t, server.URL)
			ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
			resp, bifrostErr := provider.SystemOne(ctx, testKey(), &schemas.BifrostSystemOneRequest{
				Model: defaultModel,
				State: json.RawMessage(`"hi"`),
				Questions: map[string]json.RawMessage{
					"q": json.RawMessage(`{"type":"choice","instructions":"x"}`),
				},
			})
			assert.Nil(t, resp)
			require.NotNil(t, bifrostErr)
			require.NotNil(t, bifrostErr.StatusCode)
			assert.Equal(t, tt.status, *bifrostErr.StatusCode)
			assert.Contains(t, strings.ToLower(bifrostErr.Error.Message), strings.ToLower(tt.wantSubstr))
		})
	}
}

func TestParseTypeSafeErrorDefaults(t *testing.T) {
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	resp.SetStatusCode(statusOverloaded)
	parsed := parseTypeSafeError(resp)
	require.NotNil(t, parsed)
	require.NotNil(t, parsed.StatusCode)
	assert.Equal(t, statusOverloaded, *parsed.StatusCode)
	assert.Contains(t, parsed.Error.Message, "529")
}

func TestTruncateState(t *testing.T) {
	assert.Equal(t, "hello", truncateState("  hello  "))
	long := strings.Repeat("a", maxStateChars+10)
	assert.Equal(t, maxStateChars, len(truncateState(long)))
}
