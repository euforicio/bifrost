package xai

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestNormalizeXAIStreamingProviderCost(t *testing.T) {
	chatTicks := int64(25_000_000)
	chat := normalizeXAIChatStreamResponse(&schemas.BifrostChatResponse{
		Usage: &schemas.BifrostLLMUsage{CostInUsdTicks: &chatTicks},
	})
	if chat.Usage.Cost == nil || chat.Usage.Cost.TotalCost != 0.0025 {
		t.Fatalf("chat cost = %#v", chat.Usage.Cost)
	}

	responseTicks := int64(50_000_000)
	responses := normalizeXAIResponsesStreamResponse(&schemas.BifrostResponsesStreamResponse{
		Response: &schemas.BifrostResponsesResponse{
			Usage: &schemas.ResponsesResponseUsage{CostInUsdTicks: &responseTicks},
		},
	})
	if responses.Response.Usage.Cost == nil || responses.Response.Usage.Cost.TotalCost != 0.005 {
		t.Fatalf("responses cost = %#v", responses.Response.Usage.Cost)
	}
}
