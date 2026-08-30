package bifrost

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestIsCustomProviderConfig(t *testing.T) {
	require.False(t, isCustomProviderConfig(nil))
	require.False(t, isCustomProviderConfig(&schemas.ProviderConfig{}))
	require.True(t, isCustomProviderConfig(&schemas.ProviderConfig{
		CustomProviderConfig: &schemas.CustomProviderConfig{BaseProviderType: schemas.OpenAI},
	}))
}
