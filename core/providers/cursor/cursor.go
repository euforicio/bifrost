// Package cursor implements account-backed Cursor AgentService access.
package cursor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/providers/cursor/cursorpb"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"google.golang.org/protobuf/proto"
)

const (
	defaultBaseURL      = "https://api2.cursor.sh"
	usableModelsPath    = "/agent.v1.AgentService/GetUsableModels"
	runPath             = "/agent.v1.AgentService/Run"
	cursorClientVersion = "cli-2026.01.09-231024f"
	maxRetainedBridges  = 64
	maxModelBodyBytes   = 8 << 20
	maxErrorBodyBytes   = 1 << 20
)

var cursorBridgeRetention = 5 * time.Minute

var cursorHeartbeatInterval = 15 * time.Second

type CursorProvider struct {
	unsupportedProvider
	logger                schemas.Logger
	client                *http.Client
	streamClient          *http.Client
	baseURL               string
	networkConfig         schemas.NetworkConfig
	responseHeaderTimeout time.Duration

	mu      sync.Mutex
	bridges map[string]*cursorBridge
}

func NewCursorProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*CursorProvider, error) {
	config.CheckAndSetDefaults()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: config.NetworkConfig.InsecureSkipVerify} // #nosec G402 -- explicitly configured for private fixtures/endpoints.
	if config.NetworkConfig.CACertPEM != nil && config.NetworkConfig.CACertPEM.GetValue() != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(config.NetworkConfig.CACertPEM.GetValue())) {
			return nil, fmt.Errorf("invalid cursor CA certificate")
		}
		tlsConfig.RootCAs = pool
	}
	timeout := time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds) * time.Second
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, TLSClientConfig: tlsConfig,
		MaxConnsPerHost:       config.NetworkConfig.MaxConnsPerHost,
		MaxIdleConnsPerHost:   config.NetworkConfig.MaxConnsPerHost,
		IdleConnTimeout:       time.Duration(config.NetworkConfig.KeepAliveTimeoutInSeconds) * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	baseURL := strings.TrimRight(strings.TrimSpace(config.NetworkConfig.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &CursorProvider{
		unsupportedProvider: unsupportedProvider{providerKey: schemas.CursorProvider},
		logger:              logger, client: client, streamClient: providerUtils.BuildStreamingHTTPClient(client),
		baseURL: baseURL, networkConfig: config.NetworkConfig, responseHeaderTimeout: timeout, bridges: make(map[string]*cursorBridge),
	}, nil
}

func (provider *CursorProvider) GetProviderKey() schemas.ModelProvider { return schemas.CursorProvider }

func (provider *CursorProvider) resolveHeaders(ctx context.Context, key schemas.Key, forceRefresh bool) (http.Header, bool, *schemas.BifrostError) {
	accessToken := strings.TrimSpace(key.Value.GetValue())
	dynamic := accessToken == ""
	extra := map[string]string{}
	if dynamic {
		if key.CredentialResolver == nil {
			return nil, false, providerUtils.NewConfigurationError("credential_resolver is required when the cursor API key is empty")
		}
		credential, err := key.CredentialResolver.ResolveProviderCredential(ctx, schemas.CursorProvider, key.ID, forceRefresh)
		if err != nil {
			return nil, false, providerUtils.NewBifrostOperationError("failed to resolve cursor credential", err)
		}
		accessToken = strings.TrimSpace(credential.AccessToken)
		maps.Copy(extra, credential.ExtraHeaders)
		if accessToken == "" {
			return nil, false, providerUtils.NewConfigurationError("resolved cursor access token is empty")
		}
	}
	headers := make(http.Header, len(extra)+8)
	for name, value := range provider.networkConfig.ExtraHeaders {
		headers.Set(name, value)
	}
	for name, value := range extra {
		headers.Set(name, value)
	}
	headers.Set("Authorization", "Bearer "+accessToken)
	headers.Set("X-Cursor-Client-Version", cursorClientVersion)
	headers.Set("X-Cursor-Client-Type", "cli")
	headers.Set("X-Ghost-Mode", "true")
	headers.Set("User-Agent", "bifrost")
	headers.Set("X-Request-ID", randomID())
	return headers, dynamic, nil
}

func statusError(status int, body []byte) *schemas.BifrostError {
	message := parseConnectErrorBody(body)
	if message == "" {
		message = http.StatusText(status)
	}
	return &schemas.BifrostError{
		StatusCode: &status,
		Error:      &schemas.ErrorField{Message: message},
		ExtraFields: schemas.BifrostErrorExtraFields{
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.CursorProvider},
		},
	}
}

func (provider *CursorProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	return providerUtils.HandleMultipleListModelsRequests(ctx, keys, request, provider.listModelsResponseByKey)
}

func (provider *CursorProvider) listModelsResponseByKey(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	response := &schemas.BifrostListModelsResponse{Data: []schemas.Model{}}
	models, err := provider.listModelsByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		if !request.Unfiltered && (!key.Models.IsUnrestricted() && !key.Models.IsAllowed(model.GetModelId()) || key.BlacklistedModels.IsBlocked(model.GetModelId())) {
			continue
		}
		id := string(schemas.CursorProvider) + "/" + model.GetModelId()
		if id == "cursor/" {
			continue
		}
		name := model.GetDisplayName()
		response.Data = append(response.Data, schemas.Model{ID: id, Name: &name, SupportedMethods: []string{string(schemas.ResponsesRequest), string(schemas.ResponsesStreamRequest)}})
	}
	return response, nil
}

func (provider *CursorProvider) listModelsByKey(ctx context.Context, key schemas.Key) ([]*cursorpb.ModelDetails, *schemas.BifrostError) {
	payload, marshalErr := proto.Marshal(&cursorpb.GetUsableModelsRequest{})
	if marshalErr != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to encode cursor model request", marshalErr)
	}
	for attempt := 0; attempt < 2; attempt++ {
		headers, dynamic, resolveErr := provider.resolveHeaders(ctx, key, attempt == 1)
		if resolveErr != nil {
			return nil, resolveErr
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.baseURL+usableModelsPath, bytes.NewReader(payload))
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError("failed to create cursor model request", err)
		}
		req.Header = headers
		req.Header.Set("Content-Type", "application/proto")
		resp, err := provider.client.Do(req)
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError("cursor model request failed", err)
		}
		body, readErr := readBoundedBody(resp.Body, maxModelBodyBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, providerUtils.NewBifrostOperationError("failed to read cursor model response", readErr)
		}
		if resp.StatusCode == http.StatusUnauthorized && dynamic && attempt == 0 {
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, statusError(resp.StatusCode, body)
		}
		wire := body
		if framed, frameErr := firstDataFrame(body); frameErr == nil {
			wire = framed
		}
		var decoded cursorpb.GetUsableModelsResponse
		if err := proto.Unmarshal(wire, &decoded); err != nil {
			return nil, providerUtils.NewBifrostOperationError("failed to decode cursor model response", err)
		}
		return decoded.Models, nil
	}
	panic("unreachable")
}

func bridgeKey(keyID, continuationID string) string {
	return keyID + "\x00" + continuationID
}

func requestContinuationID(request *schemas.BifrostResponsesRequest) string {
	if request.Params == nil || request.Params.PreviousResponseID == nil {
		return ""
	}
	return strings.TrimSpace(*request.Params.PreviousResponseID)
}

func hasFunctionOutput(items []schemas.ResponsesMessage) bool {
	for _, item := range items {
		if item.Type != nil && *item.Type == schemas.ResponsesMessageTypeFunctionCallOutput {
			return true
		}
	}
	return false
}

func (provider *CursorProvider) takeBridge(id string) *cursorBridge {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	bridge := provider.bridges[id]
	delete(provider.bridges, id)
	if bridge != nil && bridge.expiryTimer != nil {
		bridge.expiryTimer.Stop()
		bridge.expiryTimer = nil
	}
	return bridge
}

func (provider *CursorProvider) retainBridge(id string, bridge *cursorBridge) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if old := provider.bridges[id]; old != nil {
		old.close()
	}
	if len(provider.bridges) >= maxRetainedBridges {
		var oldestID string
		var oldest time.Time
		for candidateID, candidate := range provider.bridges {
			if oldestID == "" || candidate.lastUsed.Before(oldest) {
				oldestID, oldest = candidateID, candidate.lastUsed
			}
		}
		provider.bridges[oldestID].close()
		delete(provider.bridges, oldestID)
	}
	provider.bridges[id] = bridge
	bridge.expiryTimer = time.AfterFunc(cursorBridgeRetention, func() {
		provider.mu.Lock()
		if provider.bridges[id] != bridge {
			provider.mu.Unlock()
			return
		}
		delete(provider.bridges, id)
		provider.mu.Unlock()
		bridge.close()
	})
}

func (provider *CursorProvider) prepareBridge(ctx context.Context, key schemas.Key, request *schemas.BifrostResponsesRequest) (*cursorBridge, *schemas.BifrostError) {
	continuationID := requestContinuationID(request)
	if continuationID != "" {
		id := bridgeKey(key.ID, continuationID)
		bridge := provider.takeBridge(id)
		if bridge == nil {
			return nil, providerUtils.NewConfigurationError("cursor continuation is unknown or expired")
		}
		output, ok := matchingFunctionOutput(request.Input, bridge.pending.toolCallID)
		if !ok {
			provider.retainBridge(id, bridge)
			return nil, providerUtils.NewConfigurationError("cursor continuation requires the matching function_call_output")
		}
		if err := bridge.sendToolResult(output); err != nil {
			bridge.close()
			return nil, providerUtils.NewBifrostOperationError("failed to send cursor tool result", err)
		}
		return bridge, nil
	}
	if hasFunctionOutput(request.Input) {
		return nil, providerUtils.NewConfigurationError("cursor function_call_output requires previous_response_id")
	}
	modelID := strings.TrimPrefix(request.Model, string(schemas.CursorProvider)+"/")
	wireRequest, blobs, err := buildRunRequest(request, modelID)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to build cursor request", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		headers, dynamic, resolveErr := provider.resolveHeaders(ctx, key, attempt == 1)
		if resolveErr != nil {
			return nil, resolveErr
		}
		bridge, status, startErr := provider.startBridge(ctx, headers, wireRequest, blobs, cursorNativeWebEnabled(request))
		if status == http.StatusUnauthorized && dynamic && attempt == 0 {
			continue
		}
		if startErr != nil {
			return nil, startErr
		}
		return bridge, nil
	}
	panic("unreachable")
}

func (provider *CursorProvider) startBridge(ctx context.Context, headers http.Header, wireRequest *cursorpb.AgentRunRequest, blobs map[string][]byte, nativeWebEnabled bool) (*cursorBridge, int, *schemas.BifrostError) {
	// Relay caller cancellation only through response-header establishment. Once
	// established, the Connect stream must survive the first HTTP response so a
	// tool result can resume it through previous_response_id.
	startupCtx, startupCancel := context.WithTimeout(ctx, provider.responseHeaderTimeout)
	defer startupCancel()
	upstreamCtx, cancel := context.WithCancel(context.WithoutCancel(startupCtx))
	stopStartupCancellation := context.AfterFunc(startupCtx, cancel)
	reader, writer := io.Pipe()
	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, provider.baseURL+runPath, reader)
	if err != nil {
		cancel()
		return nil, 0, providerUtils.NewBifrostOperationError("failed to create cursor run request", err)
	}
	req.Header = headers
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("TE", "trailers")
	frames := &cursorFrameWriter{writer: writer}
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- writeCursorFrame(frames, &cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_RunRequest{RunRequest: wireRequest}})
	}()
	resp, err := provider.streamClient.Do(req)
	if err != nil {
		stopStartupCancellation()
		cancel()
		_ = writer.Close()
		return nil, 0, providerUtils.NewBifrostOperationError("cursor run request failed", err)
	}
	if !stopStartupCancellation() && startupCtx.Err() != nil {
		_ = resp.Body.Close()
		cancel()
		_ = writer.Close()
		return nil, resp.StatusCode, providerUtils.NewBifrostOperationError("cursor run request cancelled during startup", startupCtx.Err())
	}
	if err := <-writeErr; err != nil {
		cancel()
		_ = resp.Body.Close()
		_ = writer.Close()
		return nil, resp.StatusCode, providerUtils.NewBifrostOperationError("failed to send cursor run frame", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readBoundedBody(resp.Body, maxErrorBodyBytes)
		_ = resp.Body.Close()
		cancel()
		_ = writer.Close()
		return nil, resp.StatusCode, statusError(resp.StatusCode, body)
	}
	bridge := &cursorBridge{reader: resp.Body, writer: writer, frames: frames, cancel: cancel, heartbeatDone: make(chan struct{}), blobs: blobs, tools: wireRequest.McpTools, nativeWebEnabled: nativeWebEnabled, continuationID: randomID(), lastUsed: time.Now()}
	go func() {
		ticker := time.NewTicker(cursorHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if writeCursorFrame(frames, &cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &cursorpb.ClientHeartbeat{}}}) != nil {
					bridge.close()
					return
				}
			case <-bridge.heartbeatDone:
				return
			}
		}
	}()
	return bridge, resp.StatusCode, nil
}

func readBoundedBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("cursor response exceeds %d bytes", limit)
	}
	return body, nil
}

type responseAccumulator struct {
	text, reasoning string
	output          []schemas.ResponsesMessage
	outputByID      map[string]int
	completed       *schemas.BifrostResponsesResponse
}

func (a *responseAccumulator) emit(event *schemas.BifrostResponsesStreamResponse) error {
	if event.Delta != nil {
		switch event.Type {
		case schemas.ResponsesStreamResponseTypeOutputTextDelta:
			a.text += *event.Delta
		case schemas.ResponsesStreamResponseTypeReasoningSummaryTextDelta:
			a.reasoning += *event.Delta
		}
	}
	if event.Item != nil {
		if event.Item.ID != nil {
			if index, ok := a.outputByID[*event.Item.ID]; ok {
				a.output[index] = *event.Item
			} else {
				if a.outputByID == nil {
					a.outputByID = make(map[string]int)
				}
				a.outputByID[*event.Item.ID] = len(a.output)
				a.output = append(a.output, *event.Item)
			}
		} else {
			a.output = append(a.output, *event.Item)
		}
	}
	if event.Response != nil {
		a.completed = event.Response
	}
	return nil
}

func (a *responseAccumulator) response(model string) *schemas.BifrostResponsesResponse {
	response := a.completed
	if response == nil {
		status := schemas.ResponsesResponseStatusCompleted
		id := randomID()
		response = &schemas.BifrostResponsesResponse{ID: &id, Object: "response", Model: model, Status: &status}
	}
	if a.reasoning != "" {
		itemType := schemas.ResponsesMessageTypeReasoning
		status := schemas.ResponsesResponseStatusCompleted
		response.Output = append(response.Output, schemas.ResponsesMessage{Type: &itemType, Status: &status, ResponsesReasoning: &schemas.ResponsesReasoning{Summary: []schemas.ResponsesReasoningSummary{{Type: schemas.ResponsesReasoningContentBlockTypeSummaryText, Text: a.reasoning}}}})
	}
	if a.text != "" {
		itemType := schemas.ResponsesMessageTypeMessage
		status := schemas.ResponsesResponseStatusCompleted
		role := schemas.ResponsesInputMessageRoleAssistant
		response.Output = append(response.Output, schemas.ResponsesMessage{Type: &itemType, Status: &status, Role: &role, Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{{Type: schemas.ResponsesOutputMessageContentTypeText, Text: &a.text}}}})
	}
	response.Output = append(response.Output, a.output...)
	return response
}

func (provider *CursorProvider) Responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	bridge, err := provider.prepareBridge(ctx, key, request)
	if err != nil {
		return nil, err
	}
	accumulator := &responseAccumulator{}
	paused, processErr := bridge.process(ctx, request.Model, accumulator.emit)
	if processErr != nil {
		bridge.close()
		return nil, providerUtils.NewBifrostOperationError("cursor response stream failed", processErr)
	}
	if paused {
		provider.retainBridge(bridgeKey(key.ID, bridge.continuationID), bridge)
	} else {
		bridge.close()
	}
	return accumulator.response(request.Model), nil
}

func (provider *CursorProvider) ResponsesStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	bridge, err := provider.prepareBridge(ctx, key, request)
	if err != nil {
		return nil, err
	}
	responseChan := make(chan *schemas.BifrostStreamChunk, schemas.DefaultStreamBufferSize)
	go func() {
		defer providerUtils.EnsureStreamFinalizerCalled(ctx, postHookSpanFinalizer)
		defer providerUtils.CloseStream(ctx, responseChan)
		paused, processErr := bridge.process(ctx, request.Model, func(event *schemas.BifrostResponsesStreamResponse) error {
			if event.Type == schemas.ResponsesStreamResponseTypeCompleted {
				ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
			}
			providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetBifrostResponseForStreamResponse(nil, nil, event, nil, nil, nil), responseChan, postHookSpanFinalizer)
			return nil
		})
		if processErr != nil {
			bridge.close()
			ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
			providerUtils.ProcessAndSendBifrostError(ctx, postHookRunner, providerUtils.NewBifrostOperationError("cursor response stream failed", processErr), responseChan, provider.logger, postHookSpanFinalizer)
			return
		}
		if paused {
			provider.retainBridge(bridgeKey(key.ID, bridge.continuationID), bridge)
		} else {
			bridge.close()
		}
	}()
	return responseChan, nil
}
