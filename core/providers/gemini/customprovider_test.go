package gemini

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestCustomProviderPreservesMultimodalInputAndWebSearch(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, time.Time{})
	ctx.SetValue(schemas.BifrostContextKeyIsCustomProvider, true)
	ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.Gemini)

	req := &schemas.BifrostResponsesRequest{
		Provider: schemas.ModelProvider("company-gemini"),
		Model:    "gemini-2.5-flash",
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

	result, err := ToGeminiResponsesRequest(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Contents, 1)
	require.Len(t, result.Contents[0].Parts, 2)
	require.NotNil(t, result.Contents[0].Parts[1].InlineData)
	require.Equal(t, "image/png", result.Contents[0].Parts[1].InlineData.MIMEType)
	require.NotEmpty(t, result.Contents[0].Parts[1].InlineData.Data)
	require.Len(t, result.Tools, 1)
	require.NotNil(t, result.Tools[0].GoogleSearch)
}
