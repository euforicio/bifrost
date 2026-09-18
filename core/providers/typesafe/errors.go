package typesafe

import (
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func parseTypeSafeError(resp *fasthttp.Response) *schemas.BifrostError {
	if resp == nil {
		return providerUtils.NewBifrostOperationError("empty typesafe error response", nil)
	}

	var body TypeSafeErrorBody
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &body)
	if bifrostErr == nil {
		bifrostErr = &schemas.BifrostError{
			IsBifrostError: false,
			Error:          &schemas.ErrorField{},
		}
	}
	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}

	status := resp.StatusCode()
	if bifrostErr.StatusCode == nil {
		bifrostErr.StatusCode = &status
	}

	message := firstNonEmpty(body.Message, body.Error, body.Detail, bifrostErr.Error.Message)
	if message == "" {
		raw := strings.TrimSpace(string(resp.Body()))
		if raw != "" {
			var asString string
			if err := sonic.Unmarshal(resp.Body(), &asString); err == nil {
				raw = asString
			}
			message = raw
		}
	}
	if message == "" {
		switch status {
		case fasthttp.StatusUnauthorized:
			message = "authentication failed: unauthorized (401) - check your API key"
		case 422:
			message = "request body failed validation (422)"
		case fasthttp.StatusTooManyRequests:
			message = "rate limit exceeded (429)"
		case statusOverloaded:
			message = "typesafe is temporarily overloaded (529)"
		default:
			message = fmt.Sprintf("typesafe api error (HTTP %d)", status)
		}
	}
	bifrostErr.Error.Message = message
	return bifrostErr
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
