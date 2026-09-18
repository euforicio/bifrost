package typesafe

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// ToTypeSafeSystemOneRequest converts a Bifrost System One request into the
// documented TypeSafe wire body. Pure: no HTTP, no logging.
func ToTypeSafeSystemOneRequest(request *schemas.BifrostSystemOneRequest) (*TypeSafeSystemOneRequest, error) {
	if request == nil {
		return nil, nil
	}
	questions := make(map[string]TypeSafeSystemOneQuestion, len(request.Questions))
	for id, raw := range request.Questions {
		var question TypeSafeSystemOneQuestion
		if len(raw) == 0 {
			continue
		}
		if err := sonic.Unmarshal(raw, &question); err != nil {
			return nil, err
		}
		questions[id] = question
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = defaultModel
	}
	return &TypeSafeSystemOneRequest{
		State:     request.State,
		Model:     model,
		Questions: questions,
	}, nil
}

// ToBifrostSystemOneResponse converts a TypeSafe wire response into the Bifrost
// envelope. Pure: no HTTP, no logging. Answers stay as documented JSON objects.
func ToBifrostSystemOneResponse(response *TypeSafeSystemOneResponse) (*schemas.BifrostSystemOneResponse, error) {
	if response == nil {
		return nil, nil
	}
	answers := make(map[string]json.RawMessage, len(response.Answers))
	for id, answer := range response.Answers {
		raw, err := providerUtils.MarshalSorted(answer)
		if err != nil {
			return nil, err
		}
		answers[id] = raw
	}
	out := &schemas.BifrostSystemOneResponse{
		Model:   response.Model,
		Answers: answers,
	}
	if response.Usage != nil {
		out.Usage = &schemas.SystemOneUsage{
			InputTokens:  response.Usage.InputTokens,
			OutputTokens: response.Usage.OutputTokens,
		}
	}
	return out, nil
}

// SystemOne evaluates typed questions against a state via POST /v1/systemone.
func (provider *TypeSafeProvider) SystemOne(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSystemOneRequest) (*schemas.BifrostSystemOneResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.TypeSafe, provider.customProviderConfig, schemas.SystemOneRequest); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, providerUtils.NewBifrostOperationError("system one request is nil", nil)
	}
	if len(request.Questions) == 0 {
		return nil, providerUtils.NewBifrostOperationError("system one request requires at least one question", nil)
	}

	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToTypeSafeSystemOneRequest(request)
		},
	)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.buildRequestURL(ctx, systemOnePath, schemas.SystemOneRequest))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	if apiKey := key.Value.GetValue(); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.SetBody(jsonData)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.EnrichError(ctx, parseTypeSafeError(resp), jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}

	var typesafeResponse TypeSafeSystemOneResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(
		ctx,
		resp.Body(),
		&typesafeResponse,
		jsonData,
		sendBackRawRequest,
		sendBackRawResponse,
	)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}

	bifrostResp, err := ToBifrostSystemOneResponse(&typesafeResponse)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err)
	}
	if bifrostResp == nil {
		return nil, providerUtils.NewBifrostOperationError("empty typesafe system one response", nil)
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	bifrostResp.ExtraFields.ProviderResponseHeaders = providerUtils.ExtractProviderResponseHeaders(resp)
	if sendBackRawRequest {
		bifrostResp.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}
