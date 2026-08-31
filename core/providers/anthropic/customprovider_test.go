package anthropic

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestCustomProviderUsesBaseProviderCapabilitiesForWebSearch(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, time.Time{})
	ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.Anthropic)

	req := &schemas.BifrostResponsesRequest{
		Provider: schemas.ModelProvider("company-anthropic"),
		Model:    "claude-sonnet-4-6-20250514",
		Input: []schemas.ResponsesMessage{{
			Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
			Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{
				{Type: schemas.ResponsesInputMessageContentBlockTypeText, Text: schemas.Ptr("Describe this image and search the web")},
				{
					Type: schemas.ResponsesInputMessageContentBlockTypeImage,
					ResponsesInputMessageContentBlockImage: &schemas.ResponsesInputMessageContentBlockImage{
						ImageURL: schemas.Ptr("data:image/png;base64,iVBORw0KGgo="),
					},
				},
			}},
		}},
		Params: &schemas.ResponsesParameters{Tools: []schemas.ResponsesTool{{
			Type:                   schemas.ResponsesToolTypeWebSearch,
			ResponsesToolWebSearch: &schemas.ResponsesToolWebSearch{},
		}}},
	}

	result, err := ToAnthropicResponsesRequest(ctx, req)
	require.NoError(t, err)
	require.Len(t, result.Messages, 1)
	require.Len(t, result.Messages[0].Content.ContentBlocks, 2)
	require.Equal(t, AnthropicContentBlockTypeImage, result.Messages[0].Content.ContentBlocks[1].Type)
	require.NotNil(t, result.Messages[0].Content.ContentBlocks[1].Source)
	require.Len(t, result.Tools, 1)
	require.NotNil(t, result.Tools[0].Type)
	require.Equal(t, AnthropicToolTypeWebSearch20260209, *result.Tools[0].Type)
}
