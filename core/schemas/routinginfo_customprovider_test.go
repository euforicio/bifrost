package schemas

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildRoutingInfoIncludesCustomBaseProvider(t *testing.T) {
	ctx := NewBifrostContext(nil, time.Time{})
	ctx.SetValue(BifrostContextKeyBaseProviderType, OpenAI)

	info := BuildRoutingInfo(ctx, ModelProvider("company-openai"), "gpt-4.1-mini", Key{Name: "account-a"})
	require.Equal(t, ModelProvider("company-openai"), info.Provider)
	require.Equal(t, OpenAI, info.BaseProvider)
	require.Equal(t, "account-a", info.Key)

	standard := BuildRoutingInfo(ctx, OpenAI, "gpt-4.1-mini", Key{})
	require.Empty(t, standard.BaseProvider)
}
