package fireworks

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

type fireworksListModelsResponse struct {
	Models        []fireworksModel `json:"models"`
	NextPageToken string           `json:"nextPageToken"`
}

type fireworksModel struct {
	Name               string `json:"name"`
	DisplayName        string `json:"displayName"`
	CreateTime         string `json:"createTime"`
	Kind               string `json:"kind"`
	ContextLength      *int   `json:"contextLength"`
	SupportsImageInput bool   `json:"supportsImageInput"`
	SupportsTools      bool   `json:"supportsTools"`
}

func (provider *FireworksProvider) modelCatalogURL(pageToken string) string {
	base := strings.TrimSuffix(strings.TrimRight(provider.networkConfig.BaseURL, "/"), "/inference")
	query := url.Values{"filter": {"supports_serverless=true"}}
	query.Set("pageSize", strconv.Itoa(200))
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	return base + "/v1/accounts/fireworks/models?" + query.Encode()
}

func (provider *FireworksProvider) listModelsByKey(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	var models []fireworksModel
	var totalLatency time.Duration
	var providerHeaders map[string]string
	var rawResponses []json.RawMessage
	pageToken := ""
	complete := false
	for page := 0; page < schemas.MaxPaginationRequests; page++ {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.modelCatalogURL(pageToken))
		req.Header.SetMethod(http.MethodGet)
		req.Header.SetContentType("application/json")
		if key.Value.GetValue() != "" {
			req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
		}

		latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		wait()
		totalLatency += latency
		fasthttp.ReleaseRequest(req)
		if bifrostErr != nil {
			fasthttp.ReleaseResponse(resp)
			return nil, bifrostErr
		}
		providerHeaders = providerUtils.ExtractProviderResponseHeaders(resp)
		ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerHeaders)
		if resp.StatusCode() != fasthttp.StatusOK {
			status := resp.StatusCode()
			var upstreamError struct {
				Message string `json:"message"`
				Error   struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(resp.Body(), &upstreamError)
			message := upstreamError.Message
			if message == "" {
				message = upstreamError.Error.Message
			}
			if message == "" {
				message = http.StatusText(status)
			}
			fasthttp.ReleaseResponse(resp)
			return nil, providerUtils.SetErrorLatency(providerUtils.NewProviderAPIError(message, nil, status, nil, nil), totalLatency)
		}

		body := append([]byte(nil), resp.Body()...)
		fasthttp.ReleaseResponse(resp)
		var upstream fireworksListModelsResponse
		if err := json.Unmarshal(body, &upstream); err != nil {
			return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError("failed to decode Fireworks model catalog", err), totalLatency)
		}
		models = append(models, upstream.Models...)
		if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
			rawResponses = append(rawResponses, json.RawMessage(body))
		}
		if upstream.NextPageToken == "" {
			complete = true
			break
		}
		pageToken = upstream.NextPageToken
	}
	if !complete {
		return nil, providerUtils.NewBifrostOperationError("Fireworks model catalog exceeded the pagination safety limit", nil)
	}

	pipeline := &providerUtils.ListModelsPipeline{
		AllowedModels: key.Models, BlacklistedModels: key.BlacklistedModels,
		Aliases: key.Aliases, Unfiltered: request.Unfiltered, ProviderKey: schemas.Fireworks,
		MatchFns: providerUtils.DefaultMatchFns(),
	}
	response := &schemas.BifrostListModelsResponse{Data: []schemas.Model{}}
	included := make(map[string]bool)
	if !pipeline.ShouldEarlyExit() {
		for _, model := range models {
			for _, result := range pipeline.FilterModel(model.Name) {
				entry := schemas.Model{
					ID:   string(schemas.Fireworks) + "/" + result.ResolvedID,
					Name: schemas.Ptr(model.DisplayName), ContextLength: model.ContextLength,
					SupportedMethods: fireworksSupportedMethods(model),
				}
				if result.AliasValue != "" {
					entry.Alias = schemas.Ptr(result.AliasValue)
				}
				if created, err := time.Parse(time.RFC3339, model.CreateTime); err == nil {
					entry.Created = schemas.Ptr(created.Unix())
				}
				response.Data = append(response.Data, entry)
				included[strings.ToLower(result.ResolvedID)] = true
			}
		}
		response.Data = append(response.Data, pipeline.BackfillModels(included)...)
	}
	response.ExtraFields.Latency = totalLatency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerHeaders
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponses
	}
	return response, nil
}

func fireworksSupportedMethods(model fireworksModel) []string {
	lower := strings.ToLower(model.Name)
	if model.Kind == "EMBEDDING_MODEL" || strings.Contains(lower, "embedding") || strings.Contains(lower, "embed") {
		return []string{string(schemas.EmbeddingRequest)}
	}
	if strings.Contains(lower, "rerank") || strings.Contains(lower, "flux") || strings.Contains(lower, "image") {
		return nil
	}
	return []string{
		string(schemas.TextCompletionRequest), string(schemas.TextCompletionStreamRequest),
		string(schemas.ChatCompletionRequest), string(schemas.ChatCompletionStreamRequest),
		string(schemas.ResponsesRequest), string(schemas.ResponsesStreamRequest),
	}
}
