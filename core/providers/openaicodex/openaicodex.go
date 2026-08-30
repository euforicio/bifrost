// Package openaicodex implements ChatGPT account-backed access to the OpenAI
// Codex Responses endpoint.
package openaicodex

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/providers/openai"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const (
	defaultResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	// This provider implements the request contract shipped by Codex 0.144.0.
	// The account model endpoint uses this value to hide models that require a
	// newer wire contract than the caller implements.
	codexClientVersion = "0.144.0"
	maxModelsBodyBytes = 8 << 20
)

// OpenAICodexProvider implements account-backed OpenAI Codex Responses calls.
type OpenAICodexProvider struct {
	unsupportedProvider
	logger              schemas.Logger
	client              *fasthttp.Client
	streamingClient     *fasthttp.Client
	responsesURL        string
	modelsURL           string
	networkConfig       schemas.NetworkConfig
	sendBackRawRequest  bool
	sendBackRawResponse bool
	modelMu             sync.RWMutex
	responsesLiteModels map[string]bool
}

// NewOpenAICodexProvider creates an OpenAI Codex provider.
func NewOpenAICodexProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*OpenAICodexProvider, error) {
	config.CheckAndSetDefaults()
	requestTimeout := time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds)
	client := &fasthttp.Client{
		ReadTimeout:         requestTimeout,
		WriteTimeout:        requestTimeout,
		MaxConnsPerHost:     config.NetworkConfig.MaxConnsPerHost,
		MaxIdleConnDuration: time.Second * time.Duration(config.NetworkConfig.KeepAliveTimeoutInSeconds),
		MaxConnWaitTimeout:  requestTimeout,
		MaxConnDuration:     time.Second * time.Duration(schemas.DefaultMaxConnDurationInSeconds),
		MaxResponseBodySize: maxModelsBodyBytes,
		ConnPoolStrategy:    fasthttp.FIFO,
	}
	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client, config.NetworkConfig.AllowPrivateNetwork)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)

	responsesURL := strings.TrimSpace(config.NetworkConfig.BaseURL)
	if responsesURL == "" {
		responsesURL = defaultResponsesURL
	}

	return &OpenAICodexProvider{
		unsupportedProvider: unsupportedProvider{providerKey: schemas.OpenAICodex},
		logger:              logger,
		client:              client,
		streamingClient:     providerUtils.BuildStreamingClient(client),
		responsesURL:        responsesURL,
		modelsURL:           strings.TrimSuffix(responsesURL, "/responses") + "/models",
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
		responsesLiteModels: map[string]bool{
			"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
		},
	}, nil
}

func (provider *OpenAICodexProvider) GetProviderKey() schemas.ModelProvider {
	return schemas.OpenAICodex
}

func (provider *OpenAICodexProvider) resolveHeaders(ctx context.Context, key schemas.Key, forceRefresh bool) (map[string]string, *schemas.BifrostError) {
	if key.CredentialResolver == nil {
		return nil, providerUtils.NewConfigurationError("credential_resolver is required for openai-codex")
	}
	credential, err := key.CredentialResolver.ResolveProviderCredential(ctx, schemas.OpenAICodex, key.ID, forceRefresh)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to resolve openai-codex credential", err)
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return nil, providerUtils.NewConfigurationError("resolved openai-codex access token is empty")
	}

	headers := make(map[string]string, len(credential.ExtraHeaders)+4)
	maps.Copy(headers, credential.ExtraHeaders)
	headers["Authorization"] = "Bearer " + credential.AccessToken
	if credential.AccountID != "" {
		headers["ChatGPT-Account-ID"] = credential.AccountID
	}
	headers["originator"] = "bifrost"
	headers["User-Agent"] = "bifrost"
	return headers, nil
}

func isUnauthorized(err *schemas.BifrostError) bool {
	return err != nil && err.StatusCode != nil && *err.StatusCode == fasthttp.StatusUnauthorized
}

type codexModel struct {
	Slug             string  `json:"slug"`
	DisplayName      string  `json:"display_name"`
	Description      *string `json:"description"`
	SupportedInAPI   bool    `json:"supported_in_api"`
	Visibility       string  `json:"visibility"`
	UseResponsesLite bool    `json:"use_responses_lite"`
}

type codexModelsResponse struct {
	Models *[]codexModel `json:"models"`
}

func (provider *OpenAICodexProvider) listModelsByKey(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	modelsURL, parseErr := url.Parse(provider.modelsURL)
	if parseErr != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to build openai-codex models URL", parseErr)
	}
	query := modelsURL.Query()
	query.Set("client_version", codexClientVersion)
	modelsURL.RawQuery = query.Encode()

	for attempt := 0; attempt < 2; attempt++ {
		headers, resolveErr := provider.resolveHeaders(ctx, key, attempt == 1)
		if resolveErr != nil {
			return nil, resolveErr
		}

		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(modelsURL.String())
		req.Header.SetMethod(http.MethodGet)
		req.Header.SetContentType("application/json")
		for name, value := range headers {
			req.Header.Set(name, value)
		}

		latency, requestErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		wait()
		fasthttp.ReleaseRequest(req)
		if requestErr != nil {
			fasthttp.ReleaseResponse(resp)
			return nil, providerUtils.SetErrorLatency(requestErr, latency)
		}
		providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
		ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)
		if resp.StatusCode() == fasthttp.StatusUnauthorized && attempt == 0 {
			fasthttp.ReleaseResponse(resp)
			continue
		}
		if resp.StatusCode() != fasthttp.StatusOK {
			bifrostErr := providerUtils.SetErrorLatency(openai.ParseOpenAIError(resp), latency)
			fasthttp.ReleaseResponse(resp)
			return nil, bifrostErr
		}

		body := resp.Body()
		if len(body) > maxModelsBodyBytes {
			fasthttp.ReleaseResponse(resp)
			return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, fmt.Errorf("openai-codex model response exceeds %d bytes", maxModelsBodyBytes)), latency)
		}
		decoded := codexModelsResponse{}
		if err := json.Unmarshal(body, &decoded); err != nil {
			fasthttp.ReleaseResponse(resp)
			return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err), latency)
		}
		if decoded.Models == nil {
			fasthttp.ReleaseResponse(resp)
			return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, fmt.Errorf("openai-codex model response is missing a models array")), latency)
		}
		fasthttp.ReleaseResponse(resp)

		openAIModels := &openai.OpenAIListModelsResponse{Data: make([]openai.OpenAIModel, 0, len(*decoded.Models))}
		metadata := make(map[string]codexModel, len(*decoded.Models))
		provider.modelMu.Lock()
		for _, model := range *decoded.Models {
			if strings.TrimSpace(model.Slug) != "" {
				provider.responsesLiteModels[model.Slug] = model.UseResponsesLite
			}
			if !model.SupportedInAPI || model.Visibility != "list" || strings.TrimSpace(model.Slug) == "" {
				continue
			}
			openAIModels.Data = append(openAIModels.Data, openai.OpenAIModel{ID: model.Slug, Object: "model", OwnedBy: "openai"})
			metadata[model.Slug] = model
		}
		provider.modelMu.Unlock()
		response := openAIModels.ToBifrostListModelsResponse(schemas.OpenAICodex, key.Models, key.BlacklistedModels, key.Aliases, request.Unfiltered)
		for i := range response.Data {
			slug := strings.TrimPrefix(response.Data[i].ID, string(schemas.OpenAICodex)+"/")
			if model, ok := metadata[slug]; ok {
				response.Data[i].Name = schemas.Ptr(model.DisplayName)
				response.Data[i].Description = model.Description
			}
			response.Data[i].SupportedMethods = []string{string(schemas.ResponsesRequest), string(schemas.ResponsesStreamRequest)}
		}
		response.ExtraFields.Latency = latency.Milliseconds()
		response.ExtraFields.RoutingInfo.Provider = schemas.OpenAICodex
		response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
		return response, nil
	}
	panic("unreachable")
}

// ListModels fetches the account-scoped Codex model catalog and merges results
// across configured accounts while retaining per-key status information.
func (provider *OpenAICodexProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	return providerUtils.HandleMultipleListModelsRequests(ctx, keys, request, provider.listModelsByKey)
}

func (provider *OpenAICodexProvider) isResponsesLiteModel(model string) bool {
	model = strings.TrimPrefix(model, string(schemas.OpenAICodex)+"/")
	provider.modelMu.RLock()
	useLite := provider.responsesLiteModels[model]
	provider.modelMu.RUnlock()
	return useLite
}

// responsesLiteTools matches the current account API contract: ordinary
// function/custom tools live under the reserved functions namespace while
// native namespace and hosted tools remain top-level entries.
func responsesLiteTools(tools []schemas.ResponsesTool) []schemas.ResponsesTool {
	result := make([]schemas.ResponsesTool, 0, len(tools))
	functions := schemas.ResponsesTool{
		Type:                   schemas.ResponsesToolTypeNamespace,
		Name:                   schemas.Ptr("functions"),
		Description:            schemas.Ptr(""),
		ResponsesToolNamespace: &schemas.ResponsesToolNamespace{},
	}
	functionsIndex := -1
	for _, tool := range tools {
		switch {
		case tool.Type == schemas.ResponsesToolTypeFunction || tool.Type == schemas.ResponsesToolTypeCustom:
			if functionsIndex < 0 {
				functionsIndex = len(result)
			}
			functions.Tools = append(functions.Tools, tool)
		case tool.Type == schemas.ResponsesToolTypeNamespace && tool.Name != nil && *tool.Name == "functions" && tool.ResponsesToolNamespace != nil:
			if functionsIndex < 0 {
				functionsIndex = len(result)
			}
			if tool.Description != nil && strings.TrimSpace(*tool.Description) != "" {
				functions.Description = schemas.Ptr(*tool.Description)
			}
			functions.Tools = append(functions.Tools, tool.Tools...)
		default:
			result = append(result, tool)
		}
	}
	if len(functions.Tools) > 0 {
		result = append(result, schemas.ResponsesTool{})
		copy(result[functionsIndex+1:], result[functionsIndex:])
		result[functionsIndex] = functions
	}
	return result
}

func normalizeCodexResponsesRequest(request *openai.OpenAIResponsesRequest, useResponsesLite bool) *openai.OpenAIResponsesRequest {
	if request == nil {
		return nil
	}
	normalized := *request
	params := request.ResponsesParameters
	params.Background = nil
	params.Conversation = nil
	params.MaxOutputTokens = nil
	params.MaxToolCalls = nil
	params.Metadata = nil
	params.PreviousResponseID = nil
	params.PromptCacheRetention = nil
	params.PromptCacheOptions = nil
	params.SafetyIdentifier = nil
	params.Temperature = nil
	params.TopLogProbs = nil
	params.TopP = nil
	params.Truncation = nil
	params.User = nil
	params.IncludeServerSideToolInvocations = nil
	params.ContextManagement = nil
	params.ExtraParams = nil

	if len(params.Include) == 0 {
		params.Include = []string{"reasoning.encrypted_content"}
	}
	if params.ParallelToolCalls == nil {
		params.ParallelToolCalls = schemas.Ptr(true)
	}
	params.ToolChoice = &schemas.ResponsesToolChoice{ResponsesToolChoiceStr: schemas.Ptr("auto")}
	params.Store = schemas.Ptr(false)
	if useResponsesLite {
		liteTools := responsesLiteTools(params.Tools)
		rawTools, err := json.Marshal(liteTools)
		if err == nil {
			input := append([]schemas.ResponsesMessage(nil), request.Input.OpenAIResponsesRequestInputArray...)
			if request.Input.OpenAIResponsesRequestInputStr != nil {
				input = append(input, schemas.ResponsesMessage{
					Type:    schemas.Ptr(schemas.ResponsesMessageTypeMessage),
					Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
					Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr(*request.Input.OpenAIResponsesRequestInputStr)},
				})
			}
			prefix := []schemas.ResponsesMessage{{
				Type:            schemas.Ptr(schemas.ResponsesMessageTypeAdditionalTools),
				Role:            schemas.Ptr(schemas.ResponsesInputMessageRoleDeveloper),
				AdditionalTools: rawTools,
			}}
			if params.Instructions != nil && *params.Instructions != "" {
				prefix = append(prefix, schemas.ResponsesMessage{
					Type: schemas.Ptr(schemas.ResponsesMessageTypeMessage),
					Role: schemas.Ptr(schemas.ResponsesInputMessageRoleDeveloper),
					Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{{
						Type: schemas.ResponsesInputMessageContentBlockTypeText,
						Text: schemas.Ptr(*params.Instructions),
					}}},
				})
			}
			normalized.Input = openai.OpenAIResponsesRequestInput{OpenAIResponsesRequestInputArray: append(prefix, input...)}
			params.Instructions = nil
			params.Tools = nil
			params.ParallelToolCalls = schemas.Ptr(false)
			if params.Reasoning == nil {
				params.Reasoning = &schemas.ResponsesParametersReasoning{}
			} else {
				reasoning := *params.Reasoning
				params.Reasoning = &reasoning
			}
			params.Reasoning.Context = schemas.Ptr("all_turns")
		}
	}
	normalized.ResponsesParameters = params
	normalized.Stream = schemas.Ptr(true)
	normalized.ExtraParams = nil
	return &normalized
}

// Responses presents a unary facade over the account backend's mandatory SSE
// transport and returns the terminal response.completed payload.
func (provider *OpenAICodexProvider) Responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	stream, bifrostErr := provider.ResponsesStream(ctx, func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, err
	}, nil, key, request)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var terminal *schemas.BifrostResponsesResponse
	var terminalErr *schemas.BifrostError
	var streamedOutput []schemas.ResponsesMessage
	var streamedOutputSet []bool
	for chunk := range stream {
		if chunk.BifrostError != nil {
			terminalErr = chunk.BifrostError
			continue
		}
		response := chunk.BifrostResponsesStreamResponse
		if response == nil || response.Response == nil {
			if response != nil && response.Type == schemas.ResponsesStreamResponseTypeOutputItemDone && response.Item != nil {
				if response.OutputIndex == nil {
					streamedOutput = append(streamedOutput, *response.Item)
					streamedOutputSet = append(streamedOutputSet, true)
				} else {
					for len(streamedOutput) <= *response.OutputIndex {
						streamedOutput = append(streamedOutput, schemas.ResponsesMessage{})
						streamedOutputSet = append(streamedOutputSet, false)
					}
					streamedOutput[*response.OutputIndex] = *response.Item
					streamedOutputSet[*response.OutputIndex] = true
				}
			}
			continue
		}
		if response.Type == schemas.ResponsesStreamResponseTypeCompleted || response.Type == schemas.ResponsesStreamResponseTypeIncomplete {
			response.Response.ExtraFields = response.ExtraFields
			terminal = response.Response
		}
	}
	if terminal != nil {
		if len(terminal.Output) == 0 && len(streamedOutput) > 0 {
			terminal.Output = make([]schemas.ResponsesMessage, 0, len(streamedOutput))
			for i := range streamedOutput {
				if streamedOutputSet[i] {
					terminal.Output = append(terminal.Output, streamedOutput[i])
				}
			}
		}
		return terminal, nil
	}
	if terminalErr != nil {
		return nil, terminalErr
	}
	return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseEmpty, fmt.Errorf("openai-codex stream ended without a terminal response"))
}

// ResponsesStream sends a streaming Responses API request, refreshing once
// when the initial upstream response is HTTP 401.
func (provider *OpenAICodexProvider) ResponsesStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	for attempt := 0; attempt < 2; attempt++ {
		headers, resolveErr := provider.resolveHeaders(ctx, key, attempt == 1)
		if resolveErr != nil {
			return nil, resolveErr
		}
		useResponsesLite := provider.isResponsesLiteModel(request.Model)
		if useResponsesLite {
			headers["x-openai-internal-codex-responses-lite"] = "true"
		}
		response, bifrostErr := openai.HandleOpenAIResponsesStreaming(
			ctx, provider.streamingClient, provider.responsesURL, request, headers,
			provider.networkConfig.ExtraHeaders, provider.networkConfig.StreamIdleTimeoutInSeconds,
			providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
			provider.GetProviderKey(), postHookRunner, nil, nil, func(converted *openai.OpenAIResponsesRequest) *openai.OpenAIResponsesRequest {
				return normalizeCodexResponsesRequest(converted, useResponsesLite)
			}, nil, nil,
			provider.logger, postHookSpanFinalizer,
		)
		if attempt == 0 && isUnauthorized(bifrostErr) {
			continue
		}
		return response, bifrostErr
	}
	panic("unreachable")
}
