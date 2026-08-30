package datasheet

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseModelsDevOfficialPricing(t *testing.T) {
	rows, err := parseModelsDevOfficialPricing([]byte(`{
		"openai":{"models":{"gpt-5.6-sol":{"id":"gpt-5.6-sol","family":"gpt","modalities":{"output":["text"]},"limit":{"context":1050000,"input":922000,"output":128000},"cost":{"input":4,"output":20,"cache_read":0.4,"cache_write":5,"tiers":[{"tier":{"type":"context","size":272000},"input":8,"output":30,"cache_read":0.8,"cache_write":10}]}},"gpt-image":{"family":"gpt-image","modalities":{"output":["image"]},"cost":{"input":8,"output":32}},"text-embedding":{"family":"text-embedding","modalities":{"output":["text"]},"cost":{"input":0.02,"output":0}}}},
		"anthropic":{"models":{"claude-opus":{"cost":{"input":15,"output":75}}}},
		"google":{"models":{"gemini-pro":{"cost":{"input":1.25,"output":10}}}},
		"xai":{"models":{"grok-4":{"cost":{"input":3,"output":15}}}},
		"reseller":{"models":{"gpt-5.6-sol":{"cost":{"input":999,"output":999}}}}
	}`))
	require.NoError(t, err)

	indexed := make(map[string]configstoreTables.TableModelPricing, len(rows))
	for _, row := range rows {
		indexed[makeKey(row.Model, row.Provider, row.Mode)] = row
	}

	openAI := indexed[makeKey("gpt-5.6-sol", "openai", "responses")]
	require.NotNil(t, openAI.InputCostPerToken)
	assert.InDelta(t, 4.0/1_000_000, *openAI.InputCostPerToken, 1e-15)
	assert.InDelta(t, 20.0/1_000_000, *openAI.OutputCostPerToken, 1e-15)
	assert.InDelta(t, 0.4/1_000_000, *openAI.CacheReadInputTokenCost, 1e-15)
	assert.InDelta(t, 5.0/1_000_000, *openAI.CacheCreationInputTokenCost, 1e-15)
	assert.InDelta(t, 8.0/1_000_000, *openAI.InputCostPerTokenAbove272kTokens, 1e-15)
	assert.InDelta(t, 10.0/1_000_000, *openAI.CacheCreationInputTokenCostAbove272kTokens, 1e-15)
	assert.Equal(t, 1050000, *openAI.ContextLength)
	assert.Equal(t, 922000, *openAI.MaxInputTokens)
	assert.Equal(t, 128000, *openAI.MaxOutputTokens)

	assert.Contains(t, indexed, makeKey("claude-opus", "anthropic", "chat"))
	assert.Contains(t, indexed, makeKey("gemini-pro", "gemini", "responses"))
	assert.Contains(t, indexed, makeKey("grok-4", "xai", "chat"))
	assert.NotContains(t, indexed, makeKey("gpt-5.6-sol", "reseller", "responses"))
	assert.NotContains(t, indexed, makeKey("gpt-image", "openai", "responses"))
	assert.NotContains(t, indexed, makeKey("text-embedding", "openai", "responses"))
}

func TestSubscriptionPricingUsesOfficialModelVendor(t *testing.T) {
	pricing := func(model, provider string, input float64) configstoreTables.TableModelPricing {
		return configstoreTables.TableModelPricing{
			Model: model, Provider: provider, Mode: "chat", InputCostPerToken: &input,
		}
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-5.6-sol", "openai", "chat"):          pricing("gpt-5.6-sol", "openai", 1),
		makeKey("grok-4", "xai", "chat"):                  pricing("grok-4", "xai", 2),
		makeKey("claude-opus", "anthropic", "chat"):       pricing("claude-opus", "anthropic", 3),
		makeKey("gemini-pro", "gemini", "chat"):           pricing("gemini-pro", "gemini", 4),
		makeKey("grok-4.6", "xai", "chat"):                pricing("grok-4.6", "xai", 5),
		makeKey("claude-sonnet-4-6", "anthropic", "chat"): pricing("claude-sonnet-4-6", "anthropic", 6),
	})

	tests := []struct {
		name     string
		provider schemas.ModelProvider
		model    string
		want     float64
	}{
		{name: "Codex OpenAI", provider: schemas.OpenAICodex, model: "gpt-5.6-sol", want: 1},
		{name: "Cursor OpenAI", provider: schemas.CursorProvider, model: "gpt-5.6-sol", want: 1},
		{name: "Cursor xAI", provider: schemas.CursorProvider, model: "grok-4", want: 2},
		{name: "Cursor Anthropic", provider: schemas.CursorProvider, model: "claude-opus", want: 3},
		{name: "Cursor Google", provider: schemas.CursorProvider, model: "gemini-pro", want: 4},
		{name: "xAI direct", provider: schemas.XAI, model: "grok-4", want: 2},
		{name: "Cursor prefixed OpenAI effort", provider: schemas.CursorProvider, model: "cursor/gpt-5.6-sol-high-fast", want: 1},
		{name: "Cursor xAI routing prefix and effort", provider: schemas.CursorProvider, model: "cursor/cursor-grok-4.6-xhigh-fast", want: 5},
		{name: "Cursor reordered Anthropic effort", provider: schemas.CursorProvider, model: "cursor/claude-4.6-sonnet-medium-thinking", want: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.resolvePricing(routingInfoFor(tt.provider, tt.model), schemas.ChatCompletionRequest, LookupScopes{})
			require.NotNil(t, got)
			require.NotNil(t, got.InputCostPerToken)
			assert.Equal(t, tt.want, *got.InputCostPerToken)
		})
	}
}

func TestSubscriptionPricingIsExposedByCatalogReads(t *testing.T) {
	input := 2.0
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("grok-4", "xai", "chat"): {
			Model: "grok-4", Provider: "xai", Mode: "chat", InputCostPerToken: &input,
		},
	})

	row := s.Get("grok-4", schemas.CursorProvider, schemas.ChatCompletionRequest)
	require.NotNil(t, row)
	assert.Equal(t, input, *row.InputCostPerToken)

	entry := s.GetPricingEntryForModel("grok-4", schemas.CursorProvider)
	require.NotNil(t, entry)
	assert.Equal(t, input, *entry.InputCostPerToken)
}

func TestSubscriptionPricingDoesNotGuessUnknownModels(t *testing.T) {
	s := newTestStore()
	assert.Nil(t, s.resolvePricing(routingInfoFor(schemas.CursorProvider, "auto"), schemas.ChatCompletionRequest, LookupScopes{}))
	assert.Nil(t, s.resolvePricing(routingInfoFor(schemas.OpenAICodex, "unknown-model"), schemas.ChatCompletionRequest, LookupScopes{}))
}

func TestCursorPricingModelNormalization(t *testing.T) {
	tests := map[string]string{
		"cursor/default":                            "default",
		"cursor/gpt-5.6-sol-low":                    "gpt-5.6-sol",
		"cursor/gpt-5.6-sol-xhigh-fast":             "gpt-5.6-sol",
		"cursor/cursor-grok-4.6-medium-fast":        "grok-4.6",
		"cursor/claude-4.6-sonnet-medium":           "claude-sonnet-4-6",
		"cursor/claude-4-sonnet-thinking":           "claude-sonnet-4",
		"cursor/claude-opus-4-8-thinking-xhigh":     "claude-opus-4-8",
		"cursor/claude-fable-5-thinking-extra-high": "claude-fable-5",
		"cursor/gemini-3.6-flash-minimal":           "gemini-3.6-flash",
		"cursor/gpt-5-mini":                         "gpt-5-mini",
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, want, subscriptionPricingModel(string(schemas.CursorProvider), input))
		})
	}
}

func TestHasTokenPricingUsesOfficialVendorMapping(t *testing.T) {
	input := 1.0
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("grok-4", "xai", "chat"): {Model: "grok-4", Provider: "xai", Mode: "chat", InputCostPerToken: &input},
	})
	assert.True(t, s.HasTokenPricing(schemas.CursorProvider, "grok-4", schemas.ChatCompletionRequest, nil))
	assert.True(t, s.HasTokenPricing(schemas.CursorProvider, "cursor/grok-4", schemas.ChatCompletionRequest, nil))
	assert.True(t, s.HasTokenPricing(schemas.CursorProvider, "cursor/cursor-grok-4-high-fast", schemas.ChatCompletionRequest, nil))
	assert.False(t, s.HasTokenPricing(schemas.CursorProvider, "auto", schemas.ChatCompletionRequest, nil))
}

func TestMergeModelsDevRowPreservesUnpublishedFields(t *testing.T) {
	oldInput := 1.0
	oldQuery := 2.0
	newOutput := 3.0
	merged := mergeModelsDevRow(
		configstoreTables.TableModelPricing{Model: "m-version", BaseModel: "m", Provider: "openai", Mode: "chat", InputCostPerToken: &oldInput, InputCostPerQuery: &oldQuery},
		configstoreTables.TableModelPricing{Model: "m-version", Provider: "openai", Mode: "chat", OutputCostPerToken: &newOutput},
	)
	assert.Equal(t, "m", merged.BaseModel)
	assert.Equal(t, oldInput, *merged.InputCostPerToken)
	assert.Equal(t, oldQuery, *merged.InputCostPerQuery)
	assert.Equal(t, newOutput, *merged.OutputCostPerToken)
}

func TestModelsDevRefreshFailureReappliesLastKnownRows(t *testing.T) {
	input := 2.0
	s := newTestStore()
	s.modelsDevRows = []configstoreTables.TableModelPricing{{
		Model: "grok-4", Provider: "xai", Mode: "chat", InputCostPerToken: &input,
	}}

	err := s.SyncOfficialPricingFromModelsDev(context.Background(), "://invalid")
	require.Error(t, err)
	row := s.Get("grok-4", schemas.XAI, schemas.ChatCompletionRequest)
	require.NotNil(t, row)
	assert.Equal(t, input, *row.InputCostPerToken)
}
