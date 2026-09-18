package typesafe

import (
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

const (
	defaultBaseURL = "https://api.typesafe.ai"
	systemOnePath  = "/v1/systemone"
	listModelsPath = "/v1/models"

	defaultModel        = "jev-latest"
	modelJev113         = "jev-1.13.0"
	modelJevPreview     = "jev-preview"
	maxStateChars       = 24000
	statusOverloaded    = 529
)

func (provider *TypeSafeProvider) buildRequestURL(ctx *schemas.BifrostContext, defaultPath string, requestType schemas.RequestType) string {
	path, isCompleteURL := providerUtils.GetRequestPath(ctx, defaultPath, provider.customProviderConfig, requestType)
	if isCompleteURL {
		return path
	}
	return provider.networkConfig.BaseURL + path
}

func truncateState(state string) string {
	state = strings.TrimSpace(state)
	if len(state) <= maxStateChars {
		return state
	}
	return state[:maxStateChars]
}
