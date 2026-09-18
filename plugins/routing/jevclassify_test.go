package routing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/routing/complexity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testJevClassifierConfig() *complexity.JevConfig {
	return &complexity.JevConfig{Provider: "typesafe", Model: "jev-latest"}
}

func TestClassifyComplexityTextViaJev(t *testing.T) {
	t.Run("returns system one response", func(t *testing.T) {
		inTok, outTok := 11, 2
		plugin := &RoutingPlugin{}
		plugin.SetSystemOneRequestExecutor(func(_ *schemas.BifrostContext, req *schemas.BifrostSystemOneRequest) (*schemas.BifrostSystemOneResponse, *schemas.BifrostError) {
			assert.Equal(t, schemas.ModelProvider("typesafe"), req.Provider)
			assert.Equal(t, "jev-latest", req.Model)
			assert.Equal(t, json.RawMessage(`"classify me"`), req.State)
			return &schemas.BifrostSystemOneResponse{
				Model: "jev-1.13.0",
				Usage: &schemas.SystemOneUsage{InputTokens: &inTok, OutputTokens: &outTok},
			}, nil
		})
		got, err := plugin.classifyComplexityTextViaJev(t.Context(), testJevClassifierConfig(), "classify me", map[string]json.RawMessage{
			"complexity_tier": json.RawMessage(`{"type":"choice"}`),
		})
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "jev-1.13.0", got.Model)
	})

	t.Run("401 is fail-closed key missing", func(t *testing.T) {
		status := 401
		plugin := &RoutingPlugin{}
		plugin.SetSystemOneRequestExecutor(func(*schemas.BifrostContext, *schemas.BifrostSystemOneRequest) (*schemas.BifrostSystemOneResponse, *schemas.BifrostError) {
			return nil, &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Message: "unauthorized"}}
		})
		_, err := plugin.classifyComplexityTextViaJev(t.Context(), testJevClassifierConfig(), "classify me", nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, complexity.ErrJevKeyMissing)
	})

	t.Run("429 degrades without fail-closed", func(t *testing.T) {
		status := 429
		plugin := &RoutingPlugin{}
		plugin.SetSystemOneRequestExecutor(func(*schemas.BifrostContext, *schemas.BifrostSystemOneRequest) (*schemas.BifrostSystemOneResponse, *schemas.BifrostError) {
			return nil, &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Message: "rate limit exceeded (429)"}}
		})
		_, err := plugin.classifyComplexityTextViaJev(t.Context(), testJevClassifierConfig(), "classify me", nil)
		require.Error(t, err)
		assert.False(t, errors.Is(err, complexity.ErrJevKeyMissing))
		assert.Contains(t, err.Error(), "failed to run jev classification")
	})

	t.Run("529 degrades without fail-closed", func(t *testing.T) {
		status := 529
		plugin := &RoutingPlugin{}
		plugin.SetSystemOneRequestExecutor(func(*schemas.BifrostContext, *schemas.BifrostSystemOneRequest) (*schemas.BifrostSystemOneResponse, *schemas.BifrostError) {
			return nil, &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Message: "typesafe is temporarily overloaded (529)"}}
		})
		_, err := plugin.classifyComplexityTextViaJev(t.Context(), testJevClassifierConfig(), "classify me", nil)
		require.Error(t, err)
		assert.False(t, errors.Is(err, complexity.ErrJevKeyMissing))
	})
}

func TestClassifyJevComplexity(t *testing.T) {
	newPlugin := func(fn complexity.SystemOneFunc) *RoutingPlugin {
		plugin := &RoutingPlugin{jevClassifier: complexity.NewJevClassifier(nil)}
		cfg := complexity.DefaultAnalyzerConfig()
		cfg.Jev = testJevClassifierConfig()
		plugin.jevClassifier.Configure(&cfg)
		plugin.jevClassifier.SetSystemOneFunc(fn)
		return plugin
	}

	t.Run("publishes choice confidence not a similarity score", func(t *testing.T) {
		plugin := newPlugin(func(context.Context, *complexity.JevConfig, string, map[string]json.RawMessage) (*schemas.BifrostSystemOneResponse, error) {
			choice, _ := json.Marshal(map[string]any{"type": "choice", "choice": "SIMPLE", "confidence": 0.84})
			return &schemas.BifrostSystemOneResponse{
				Model:   "jev-1.13.0",
				Answers: map[string]json.RawMessage{"complexity_tier": choice},
			}, nil
		})
		proposal := plugin.classifyJevComplexity(schemas.NewBifrostContext(context.Background(), time.Time{}), complexity.ComplexityInput{LastUserText: "hi"})
		require.NotNil(t, proposal.Result)
		assert.Equal(t, complexity.TierSimple, proposal.Result.Tier)
		assert.Equal(t, complexity.MechanismJev, proposal.Mechanism)
		require.NotNil(t, proposal.Score)
		assert.Equal(t, 0.84, *proposal.Score)
		assert.Contains(t, proposal.LogMessage, "confidence=0.840")
		assert.Contains(t, proposal.LogMessage, "model=jev-1.13.0")
		assert.Nil(t, proposal.FailClosed)
	})

	t.Run("missing key fails closed", func(t *testing.T) {
		plugin := newPlugin(func(context.Context, *complexity.JevConfig, string, map[string]json.RawMessage) (*schemas.BifrostSystemOneResponse, error) {
			return nil, complexity.ErrJevKeyMissing
		})
		proposal := plugin.classifyJevComplexity(schemas.NewBifrostContext(context.Background(), time.Time{}), complexity.ComplexityInput{LastUserText: "hi"})
		assert.Nil(t, proposal.Result)
		assert.Equal(t, complexity.MechanismSkipped, proposal.Mechanism)
		require.Error(t, proposal.FailClosed)
		assert.ErrorIs(t, proposal.FailClosed, complexity.ErrJevKeyMissing)
	})

	t.Run("low confidence leaves tier unpublished", func(t *testing.T) {
		plugin := newPlugin(func(context.Context, *complexity.JevConfig, string, map[string]json.RawMessage) (*schemas.BifrostSystemOneResponse, error) {
			choice, _ := json.Marshal(map[string]any{"type": "choice", "choice": "COMPLEX", "confidence": 0.2})
			return &schemas.BifrostSystemOneResponse{
				Model:   "jev-1.13.0",
				Answers: map[string]json.RawMessage{"complexity_tier": choice},
			}, nil
		})
		proposal := plugin.classifyJevComplexity(schemas.NewBifrostContext(context.Background(), time.Time{}), complexity.ComplexityInput{LastUserText: "hi"})
		assert.Nil(t, proposal.Result)
		assert.Nil(t, proposal.FailClosed)
		assert.Contains(t, proposal.LogMessage, "no complexity tier is published")
	})
}
