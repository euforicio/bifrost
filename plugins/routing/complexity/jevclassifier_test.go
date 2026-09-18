package complexity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testJevPrimaryAnalyzerConfig() *AnalyzerConfig {
	cfg := DefaultAnalyzerConfig()
	cfg.Jev = &JevConfig{Provider: "typesafe", Model: "jev-latest", MinConfidence: 0.6}
	return &cfg
}

func testJevFallbackAnalyzerConfig() *AnalyzerConfig {
	cfg := DefaultAnalyzerConfig()
	cfg.Semantic = &SemanticConfig{
		Provider:       "openai",
		EmbeddingModel: "text-embedding-3-small",
		Fallback:       configstore.ComplexitySemanticFallbackJev,
	}
	cfg.Jev = &JevConfig{Provider: "typesafe", Model: "jev-latest", MinConfidence: 0.6}
	return &cfg
}

func systemOneResponse(model, tier string, confidence float64, frontier *float64) *schemas.BifrostSystemOneResponse {
	choiceRaw, _ := json.Marshal(map[string]any{
		"type":          "choice",
		"choice":        tier,
		"confidence":    confidence,
		"probabilities": map[string]float64{strings.ToUpper(tier): confidence},
	})
	answers := map[string]json.RawMessage{jevQuestionComplexityTier: choiceRaw}
	if frontier != nil {
		noulRaw, _ := json.Marshal(map[string]any{"type": "noul", "noul": *frontier})
		answers[jevQuestionNeedsFrontier] = noulRaw
	}
	return &schemas.BifrostSystemOneResponse{Model: model, Answers: answers}
}

func TestJevComplexityQuestionsAreContrastiveChoicePlusNoul(t *testing.T) {
	questions, err := jevComplexityQuestions()
	require.NoError(t, err)
	require.Contains(t, questions, jevQuestionComplexityTier)
	require.Contains(t, questions, jevQuestionNeedsFrontier)

	var choice map[string]any
	require.NoError(t, json.Unmarshal(questions[jevQuestionComplexityTier], &choice))
	assert.Equal(t, "choice", choice["type"])
	criteria, ok := choice["criteria"].(map[string]any)
	require.True(t, ok)
	for _, tier := range []string{TierSimple, TierMedium, TierComplex} {
		entry, ok := criteria[tier].(map[string]any)
		require.True(t, ok, tier)
		assert.NotEmpty(t, entry["what"])
		assert.NotEmpty(t, entry["not_for"])
		assert.NotEmpty(t, entry["examples"])
	}

	var noul map[string]any
	require.NoError(t, json.Unmarshal(questions[jevQuestionNeedsFrontier], &noul))
	assert.Equal(t, "noul", noul["type"])
}

func TestComposeJevResult(t *testing.T) {
	t.Run("maps uppercase and lowercase choice", func(t *testing.T) {
		for _, raw := range []string{"SIMPLE", "simple", "Simple"} {
			result, err := composeJevResult(systemOneResponse("jev-1.13.0", raw, 0.9, nil), 0.6)
			require.NoError(t, err)
			assert.Equal(t, TierSimple, result.Tier)
			assert.Equal(t, "jev-1.13.0", result.Model)
			assert.Equal(t, 0.9, result.Confidence)
			assert.False(t, result.Escalated)
		}
	})

	t.Run("low confidence is unpublished", func(t *testing.T) {
		_, err := composeJevResult(systemOneResponse("jev-1.13.0", "COMPLEX", 0.41, nil), 0.6)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrJevLowConfidence)
	})

	t.Run("escalates medium plus high frontier noul", func(t *testing.T) {
		frontier := 0.8
		result, err := composeJevResult(systemOneResponse("jev-1.13.0", "MEDIUM", 0.7, &frontier), 0.6)
		require.NoError(t, err)
		assert.Equal(t, TierComplex, result.Tier)
		assert.True(t, result.Escalated)
		require.NotNil(t, result.Frontier)
		assert.Equal(t, 0.8, *result.Frontier)
	})

	t.Run("does not escalate a confident medium", func(t *testing.T) {
		frontier := 0.9
		result, err := composeJevResult(systemOneResponse("jev-1.13.0", "MEDIUM", 0.85, &frontier), 0.6)
		require.NoError(t, err)
		assert.Equal(t, TierMedium, result.Tier)
		assert.False(t, result.Escalated)
	})

	t.Run("unknown choice is rejected", func(t *testing.T) {
		_, err := composeJevResult(systemOneResponse("jev-1.13.0", "REASONING", 0.9, nil), 0.6)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "named no complexity tier")
	})
}

func TestJevClassifierLifecycle(t *testing.T) {
	classifier := NewJevClassifier(nil)

	t.Run("unconfigured classifies to nil,nil", func(t *testing.T) {
		result, err := classifier.Classify(context.Background(), ComplexityInput{LastUserText: "hello"})
		require.NoError(t, err)
		assert.Nil(t, result)
		assert.False(t, classifier.IsConfigured())
		assert.False(t, classifier.PrimaryEnabled())
		assert.False(t, classifier.FallbackEnabled())
		assert.Equal(t, JevStatusDisabled, classifier.Status().State)
	})

	t.Run("primary enabled without semantic", func(t *testing.T) {
		classifier.Configure(testJevPrimaryAnalyzerConfig())
		assert.True(t, classifier.IsConfigured())
		assert.True(t, classifier.PrimaryEnabled())
		assert.False(t, classifier.FallbackEnabled())
		assert.Equal(t, JevStatusReady, classifier.Status().State)
	})

	t.Run("fallback enabled with semantic.fallback=jev", func(t *testing.T) {
		classifier.Configure(testJevFallbackAnalyzerConfig())
		assert.True(t, classifier.IsConfigured())
		assert.False(t, classifier.PrimaryEnabled())
		assert.True(t, classifier.FallbackEnabled())
	})
}

func TestJevClassifierClassify(t *testing.T) {
	t.Run("returns composed tier and logs versioned model", func(t *testing.T) {
		var gotState string
		var gotQuestions map[string]json.RawMessage
		classifier := NewJevClassifier(nil)
		classifier.Configure(testJevPrimaryAnalyzerConfig())
		classifier.SetSystemOneFunc(func(_ context.Context, jev *JevConfig, state string, questions map[string]json.RawMessage) (*schemas.BifrostSystemOneResponse, error) {
			gotState = state
			gotQuestions = questions
			assert.Equal(t, "typesafe", string(jev.Provider))
			assert.Equal(t, "jev-latest", jev.Model)
			return systemOneResponse("jev-1.13.0", "COMPLEX", 0.77, nil), nil
		})
		result, err := classifier.Classify(context.Background(), ComplexityInput{
			LastUserText:   "balance testing rules and staffing",
			PriorUserTexts: []string{"ignore this history"},
			SystemText:     "you are a helpful assistant",
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, TierComplex, result.Tier)
		assert.Equal(t, 0.77, result.Confidence)
		assert.Equal(t, "jev-1.13.0", result.Model)
		assert.Equal(t, "balance testing rules and staffing", gotState)
		require.Contains(t, gotQuestions, jevQuestionComplexityTier)
		require.Contains(t, gotQuestions, jevQuestionNeedsFrontier)
	})

	t.Run("blank input skips the provider call", func(t *testing.T) {
		called := false
		classifier := NewJevClassifier(nil)
		classifier.Configure(testJevPrimaryAnalyzerConfig())
		classifier.SetSystemOneFunc(func(context.Context, *JevConfig, string, map[string]json.RawMessage) (*schemas.BifrostSystemOneResponse, error) {
			called = true
			return nil, errors.New("should not run")
		})
		result, err := classifier.Classify(context.Background(), ComplexityInput{LastUserText: "   "})
		require.NoError(t, err)
		assert.Nil(t, result)
		assert.False(t, called)
	})

	t.Run("propagates key-missing fail-closed", func(t *testing.T) {
		classifier := NewJevClassifier(nil)
		classifier.Configure(testJevPrimaryAnalyzerConfig())
		classifier.SetSystemOneFunc(func(context.Context, *JevConfig, string, map[string]json.RawMessage) (*schemas.BifrostSystemOneResponse, error) {
			return nil, ErrJevKeyMissing
		})
		_, err := classifier.Classify(context.Background(), ComplexityInput{LastUserText: "hello"})
		assert.ErrorIs(t, err, ErrJevKeyMissing)
	})

	t.Run("truncates oversized state in code", func(t *testing.T) {
		var gotState string
		classifier := NewJevClassifier(nil)
		classifier.Configure(testJevPrimaryAnalyzerConfig())
		classifier.SetSystemOneFunc(func(_ context.Context, _ *JevConfig, state string, _ map[string]json.RawMessage) (*schemas.BifrostSystemOneResponse, error) {
			gotState = state
			return systemOneResponse("jev-1.13.0", "SIMPLE", 0.9, nil), nil
		})
		_, err := classifier.Classify(context.Background(), ComplexityInput{LastUserText: strings.Repeat("x", maxJevStateChars+50)})
		require.NoError(t, err)
		assert.Equal(t, maxJevStateChars, len(gotState))
	})
}
