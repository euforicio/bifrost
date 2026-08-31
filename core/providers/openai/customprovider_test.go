package openai

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestCustomProviderPreservesMultimodalInputAndWebSearch(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, time.Time{})
	ctx.SetValue(schemas.BifrostContextKeyIsCustomProvider, true)
	ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.OpenAI)

	req := &schemas.BifrostResponsesRequest{
		Provider: schemas.ModelProvider("company-openai"),
		Model:    "gpt-4.1-mini",
		Input: []schemas.ResponsesMessage{{
			Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
			Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{
				{Type: schemas.ResponsesInputMessageContentBlockTypeText, Text: schemas.Ptr("Describe this image and search for context")},
				{
					Type: schemas.ResponsesInputMessageContentBlockTypeImage,
					ResponsesInputMessageContentBlockImage: &schemas.ResponsesInputMessageContentBlockImage{
						ImageURL: schemas.Ptr("https://example.com/image.png"),
					},
				},
			}},
		}},
		Params: &schemas.ResponsesParameters{Tools: []schemas.ResponsesTool{{
			Type:                   schemas.ResponsesToolTypeWebSearch,
			ResponsesToolWebSearch: &schemas.ResponsesToolWebSearch{},
		}}},
	}

	result := ToOpenAIResponsesRequest(ctx, req)
	require.NotNil(t, result)
	require.Len(t, result.Input.OpenAIResponsesRequestInputArray, 1)
	message := result.Input.OpenAIResponsesRequestInputArray[0]
	require.NotNil(t, message.Content)
	require.Len(t, message.Content.ContentBlocks, 2)
	require.NotNil(t, message.Content.ContentBlocks[1].ResponsesInputMessageContentBlockImage)
	require.Equal(t, "https://example.com/image.png", *message.Content.ContentBlocks[1].ResponsesInputMessageContentBlockImage.ImageURL)
	require.Len(t, result.Tools, 1)
	require.Equal(t, schemas.ResponsesToolTypeWebSearch, result.Tools[0].Type)
}

func TestCustomProviderPreservesChatPromptCacheAndWebSearch(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, time.Time{})
	ctx.SetValue(schemas.BifrostContextKeyIsCustomProvider, true)
	ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.OpenAI)
	promptCacheKey := "conversation-42"

	result := ToOpenAIChatRequest(ctx, &schemas.BifrostChatRequest{
		Provider: schemas.ModelProvider("company-openai"),
		Model:    "gpt-4.1-mini",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("Search the web")},
		}},
		Params: &schemas.ChatParameters{
			PromptCacheKey:   &promptCacheKey,
			WebSearchOptions: &schemas.ChatWebSearchOptions{},
		},
	})

	require.NotNil(t, result)
	require.Equal(t, promptCacheKey, *result.ChatParameters.PromptCacheKey)
	require.NotNil(t, result.ChatParameters.WebSearchOptions)
}
