// Package openaicodex implements ChatGPT account-backed access to the OpenAI
// Codex Responses endpoint.
package openaicodex

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/providers/openai"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const defaultResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

// OpenAICodexProvider implements account-backed OpenAI Codex Responses calls.
type OpenAICodexProvider struct {
	unsupportedProvider
	logger              schemas.Logger
	client              *fasthttp.Client
	streamingClient     *fasthttp.Client
	responsesURL        string
	networkConfig       schemas.NetworkConfig
	sendBackRawRequest  bool
	sendBackRawResponse bool
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
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
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

// ListModels returns explicitly configured models without calling the upstream
// account endpoint. Wildcard keys cannot be expanded locally and are skipped.
func (provider *OpenAICodexProvider) ListModels(_ *schemas.BifrostContext, keys []schemas.Key, _ *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	seen := make(map[string]struct{})
	response := &schemas.BifrostListModelsResponse{Data: make([]schemas.Model, 0)}
	for _, key := range keys {
		if key.Models.IsUnrestricted() {
			continue
		}
		for _, model := range key.Models {
			if model == "" || model == "*" || key.BlacklistedModels.IsBlocked(model) {
				continue
			}
			id := fmt.Sprintf("%s/%s", schemas.OpenAICodex, model)
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			response.Data = append(response.Data, schemas.Model{ID: id})
		}
	}
	return response, nil
}

// Responses sends a non-streaming Responses API request, refreshing once when
// the account endpoint rejects the cached credential with HTTP 401.
func (provider *OpenAICodexProvider) Responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	for attempt := 0; attempt < 2; attempt++ {
		headers, resolveErr := provider.resolveHeaders(ctx, key, attempt == 1)
		if resolveErr != nil {
			return nil, resolveErr
		}
		response, bifrostErr := openai.HandleOpenAIResponsesRequest(
			ctx, provider.client, provider.responsesURL, request, headers,
			provider.networkConfig.ExtraHeaders,
			providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
			provider.GetProviderKey(), nil, nil, nil, provider.logger,
		)
		if attempt == 0 && isUnauthorized(bifrostErr) {
			continue
		}
		return response, bifrostErr
	}
	panic("unreachable")
}

// ResponsesStream sends a streaming Responses API request, refreshing once
// when the initial upstream response is HTTP 401.
func (provider *OpenAICodexProvider) ResponsesStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	for attempt := 0; attempt < 2; attempt++ {
		headers, resolveErr := provider.resolveHeaders(ctx, key, attempt == 1)
		if resolveErr != nil {
			return nil, resolveErr
		}
		response, bifrostErr := openai.HandleOpenAIResponsesStreaming(
			ctx, provider.streamingClient, provider.responsesURL, request, headers,
			provider.networkConfig.ExtraHeaders, provider.networkConfig.StreamIdleTimeoutInSeconds,
			providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
			provider.GetProviderKey(), postHookRunner, nil, nil, nil, nil, nil,
			provider.logger, postHookSpanFinalizer,
		)
		if attempt == 0 && isUnauthorized(bifrostErr) {
			continue
		}
		return response, bifrostErr
	}
	panic("unreachable")
}
