package bedrock

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestCustomProviderPreservesMultimodalInputAndSupportedWebSearch(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, time.Time{})
	ctx.SetValue(schemas.BifrostContextKeyIsCustomProvider, true)
	ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.Bedrock)

	req := &schemas.BifrostResponsesRequest{
		Provider: schemas.ModelProvider("company-bedrock"),
		Model:    "amazon.nova-2-lite-v1:0",
		Input: []schemas.ResponsesMessage{{
			Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
			Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{
				{Type: schemas.ResponsesInputMessageContentBlockTypeText, Text: schemas.Ptr("Describe this image and search for context")},
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

	result, err := ToBedrockResponsesRequest(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 1)
	require.Len(t, result.Messages[0].Content, 2)
	require.NotNil(t, result.Messages[0].Content[1].Image)
	require.Equal(t, "png", result.Messages[0].Content[1].Image.Format)
	require.NotNil(t, result.Messages[0].Content[1].Image.Source.Bytes)
	require.NotEmpty(t, *result.Messages[0].Content[1].Image.Source.Bytes)
	require.NotNil(t, result.ToolConfig)
	require.Len(t, result.ToolConfig.Tools, 1)
	require.NotNil(t, result.ToolConfig.Tools[0].SystemTool)
	require.Equal(t, BedrockSystemToolNovaGrounding, result.ToolConfig.Tools[0].SystemTool.Name)
}
