package typesafe

import (
	"net/http"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func (response *TypeSafeListModelsResponse) ToBifrostListModelsResponse(
	providerKey schemas.ModelProvider,
	allowedModels schemas.WhiteList,
	blacklistedModels schemas.BlackList,
	aliases schemas.KeyAliases,
	unfiltered bool,
) *schemas.BifrostListModelsResponse {
	if response == nil {
		return &schemas.BifrostListModelsResponse{Data: []schemas.Model{}}
	}

	bifrostResponse := &schemas.BifrostListModelsResponse{
		Data: make([]schemas.Model, 0, len(response.Models)),
	}
	pipeline := &providerUtils.ListModelsPipeline{
		AllowedModels:     allowedModels,
		BlacklistedModels: blacklistedModels,
		Aliases:           aliases,
		Unfiltered:        unfiltered,
		ProviderKey:       providerKey,
		MatchFns:          providerUtils.DefaultMatchFns(),
	}
	if pipeline.ShouldEarlyExit() {
		return bifrostResponse
	}

	included := make(map[string]bool)
	for _, card := range response.Models {
		if card.Name == "" {
			continue
		}
		for _, result := range pipeline.FilterModel(card.Name) {
			name := result.ResolvedID
			model := schemas.Model{
				ID:          name,
				Name:        &name,
				Description: stringPtr(card.Description),
			}
			if result.AliasValue != "" {
				model.Alias = &result.AliasValue
			}
			bifrostResponse.Data = append(bifrostResponse.Data, model)
			included[name] = true
		}
	}
	bifrostResponse.Data = append(bifrostResponse.Data, pipeline.BackfillModels(included)...)
	return bifrostResponse
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (provider *TypeSafeProvider) listModelsByKey(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.buildRequestURL(ctx, listModelsPath, schemas.ListModelsRequest))
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")
	if apiKey := key.Value.GetValue(); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.SetErrorLatency(parseTypeSafeError(resp), latency)
	}

	var typesafeResponse TypeSafeListModelsResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(
		resp.Body(),
		&typesafeResponse,
		nil,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
	)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	response := typesafeResponse.ToBifrostListModelsResponse(
		provider.GetProviderKey(),
		key.Models,
		key.BlacklistedModels,
		key.Aliases,
		request.Unfiltered,
	)
	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerUtils.ExtractProviderResponseHeaders(resp)
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		response.ExtraFields.RawRequest = rawRequest
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponse
	}
	return response, nil
}
