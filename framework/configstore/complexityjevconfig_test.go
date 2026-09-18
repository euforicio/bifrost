package configstore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testJevConfig() *ComplexityJevConfig {
	return &ComplexityJevConfig{
		Provider: "typesafe",
		Model:    "jev-latest",
	}
}

func testJevFallbackAnalyzerConfig() *ComplexityAnalyzerConfig {
	cfg := testComplexityAnalyzerConfig()
	cfg.Semantic = testSemanticConfig()
	cfg.Semantic.Fallback = ComplexitySemanticFallbackJev
	cfg.Jev = testJevConfig()
	return cfg
}

func testJevPrimaryAnalyzerConfig() *ComplexityAnalyzerConfig {
	cfg := testComplexityAnalyzerConfig()
	cfg.Jev = testJevConfig()
	return cfg
}

func TestComplexityJevConfigTimeoutDecoding(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    time.Duration
		wantErr bool
	}{
		{name: "duration string", payload: `{"timeout":"1.5s"}`, want: 1500 * time.Millisecond},
		{name: "number is milliseconds", payload: `{"timeout":1500}`, want: 1500 * time.Millisecond},
		{name: "absent keeps zero", payload: `{}`, want: 0},
		{name: "null keeps zero", payload: `{"timeout":null}`, want: 0},
		{name: "negative number rejected", payload: `{"timeout":-5}`, wantErr: true},
		{name: "bad string rejected", payload: `{"timeout":"soon"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg ComplexityJevConfig
			err := json.Unmarshal([]byte(tt.payload), &cfg)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.Timeout)
		})
	}
}

func TestComplexityJevConfigRejectsUnknownFields(t *testing.T) {
	var cfg ComplexityJevConfig
	err := json.Unmarshal([]byte(`{"provider":"typesafe","model":"jev-latest","similarity":0.4}`), &cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown jev complexity field")
}

func TestComplexityJevConfigNormalizedDefaults(t *testing.T) {
	cfg := (&ComplexityJevConfig{
		Provider: " TypeSafe ",
		Model:    " jev-latest ",
	}).normalized()
	assert.Equal(t, "typesafe", string(cfg.Provider))
	assert.Equal(t, "jev-latest", cfg.Model)
	assert.Equal(t, DefaultComplexityJevTimeout, cfg.Timeout)
	assert.Equal(t, DefaultComplexityJevMinConfidence, cfg.MinConfidence)
	assert.Equal(t, DefaultComplexityJevMessageHistoryCount, cfg.MessageHistoryCount)
}

func TestComplexityJevConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ComplexityJevConfig)
		wantErr string
	}{
		{name: "valid", mutate: func(*ComplexityJevConfig) {}},
		{name: "missing provider", mutate: func(c *ComplexityJevConfig) { c.Provider = "" }, wantErr: "requires a provider"},
		{name: "missing model", mutate: func(c *ComplexityJevConfig) { c.Model = "" }, wantErr: "requires a model"},
		{name: "history other than one", mutate: func(c *ComplexityJevConfig) { c.MessageHistoryCount = 2 }, wantErr: "message_history_count"},
		{name: "confidence at one", mutate: func(c *ComplexityJevConfig) { c.MinConfidence = 1 }, wantErr: "min_confidence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testJevConfig().normalized()
			tt.mutate(cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestComplexityJevFallbackAndPrimaryValidation(t *testing.T) {
	t.Run("jev fallback requires jev block", func(t *testing.T) {
		cfg := testSemanticAnalyzerConfig()
		cfg.Semantic.Fallback = ComplexitySemanticFallbackJev
		normalized := cfg.Normalized()
		err := normalized.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires a jev config block")
	})

	t.Run("enabled fallback reports enabled", func(t *testing.T) {
		normalized := testJevFallbackAnalyzerConfig().Normalized()
		require.NoError(t, normalized.Validate())
		assert.True(t, normalized.JevFallbackEnabled())
		assert.False(t, normalized.JevPrimaryEnabled())
	})

	t.Run("jev without semantic is primary", func(t *testing.T) {
		normalized := testJevPrimaryAnalyzerConfig().Normalized()
		require.NoError(t, normalized.Validate())
		assert.True(t, normalized.JevPrimaryEnabled())
		assert.False(t, normalized.JevFallbackEnabled())
	})

	t.Run("session may use jev without semantic", func(t *testing.T) {
		cfg := testJevPrimaryAnalyzerConfig()
		cfg.Session = &ComplexitySessionConfig{Enabled: true}
		normalized := cfg.Normalized()
		require.NoError(t, normalized.Validate())
		assert.True(t, normalized.SessionRoutingEnabled())
	})

	t.Run("session without semantic or jev is rejected", func(t *testing.T) {
		cfg := testComplexityAnalyzerConfig()
		cfg.Session = &ComplexitySessionConfig{Enabled: true}
		normalized := cfg.Normalized()
		err := normalized.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "semantic or jev")
	})
}

func TestDecodeComplexityAnalyzerConfigRoundTripsJev(t *testing.T) {
	cfg := testJevFallbackAnalyzerConfig()
	cfg.Jev.MinConfidence = 0.55
	normalized := cfg.Normalized()

	decoded, err := roundTripComplexityAnalyzerConfig(t, normalized)
	require.NoError(t, err)
	assert.Equal(t, ComplexitySemanticFallbackJev, decoded.Semantic.Fallback)
	require.NotNil(t, decoded.Jev)
	assert.Equal(t, "jev-latest", decoded.Jev.Model)
	assert.Equal(t, 0.55, decoded.Jev.MinConfidence)
	assert.Equal(t, DefaultComplexityJevTimeout, decoded.Jev.Timeout)
}
