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

func TestNormalizeXAIChatStreamUsageIncludesReasoning(t *testing.T) {
	response := &schemas.BifrostChatResponse{Usage: &schemas.BifrostLLMUsage{
		PromptTokens:            220,
		CompletionTokens:        5,
		TotalTokens:             276,
		CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{ReasoningTokens: 51},
	}}
	if got := normalizeXAIChatStreamResponse(response); got != response {
		t.Fatal("normalization must preserve the response")
	}
	if response.Usage.CompletionTokens != 56 {
		t.Fatalf("completion tokens = %d, want visible plus reasoning = 56", response.Usage.CompletionTokens)
	}
	normalizeXAIChatStreamResponse(response)
	if response.Usage.CompletionTokens != 56 {
		t.Fatalf("repeated normalization double-counted reasoning: %d", response.Usage.CompletionTokens)
	}
}
