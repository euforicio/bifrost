package cursor

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/providers/cursor/cursorpb"
	"github.com/maximhq/bifrost/core/schemas"
	"google.golang.org/protobuf/proto"
)

type credentialFixture struct {
	mu    sync.Mutex
	calls []bool
}

type silentLogger struct{}
type silentLogEvent struct{}

func (silentLogger) Debug(string, ...any)                   {}
func (silentLogger) Info(string, ...any)                    {}
func (silentLogger) Warn(string, ...any)                    {}
func (silentLogger) Error(string, ...any)                   {}
func (silentLogger) Fatal(string, ...any)                   {}
func (silentLogger) SetLevel(schemas.LogLevel)              {}
func (silentLogger) SetOutputType(schemas.LoggerOutputType) {}
func (silentLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return silentLogEvent{}
}
func (silentLogEvent) Str(string, string) schemas.LogEventBuilder  { return silentLogEvent{} }
func (silentLogEvent) Int(string, int) schemas.LogEventBuilder     { return silentLogEvent{} }
func (silentLogEvent) Int64(string, int64) schemas.LogEventBuilder { return silentLogEvent{} }
func (silentLogEvent) Send()                                       {}

func (fixture *credentialFixture) ResolveProviderCredential(_ context.Context, provider schemas.ModelProvider, credentialID string, forceRefresh bool) (schemas.ResolvedProviderCredential, error) {
	fixture.mu.Lock()
	fixture.calls = append(fixture.calls, forceRefresh)
	fixture.mu.Unlock()
	token := "expired"
	if forceRefresh {
		token = "fresh"
	}
	return schemas.ResolvedProviderCredential{AccessToken: token, ExtraHeaders: map[string]string{"X-Account-Binding": credentialID}}, nil
}

func newProtocolProvider(t *testing.T, handler http.Handler) (*CursorProvider, *httptest.Server) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	provider, err := NewCursorProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, InsecureSkipVerify: true}}, silentLogger{})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return provider, server
}

func writeServerMessage(t *testing.T, writer io.Writer, message *cursorpb.AgentServerMessage) {
	t.Helper()
	payload, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, len(payload)+5)
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	if _, err := writer.Write(frame); err != nil {
		t.Fatal(err)
	}
}

func writeEndFrame(t *testing.T, writer io.Writer) {
	t.Helper()
	if _, err := writer.Write([]byte{2, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
}

func textRequest(model, cacheKey, text string) *schemas.BifrostResponsesRequest {
	role := schemas.ResponsesInputMessageRoleUser
	return &schemas.BifrostResponsesRequest{Model: model, Input: []schemas.ResponsesMessage{{Role: &role, Content: &schemas.ResponsesMessageContent{ContentStr: &text}}}, Params: &schemas.ResponsesParameters{PromptCacheKey: &cacheKey}}
}

func TestReadBoundedBodyRejectsOversizeResponse(t *testing.T) {
	_, err := readBoundedBody(strings.NewReader("12345"), 4)
	if err == nil {
		t.Fatal("readBoundedBody() error = nil, want size limit error")
	}
}

func TestRetainedBridgeExpiresAndCloses(t *testing.T) {
	originalRetention := cursorBridgeRetention
	cursorBridgeRetention = 5 * time.Millisecond
	defer func() { cursorBridgeRetention = originalRetention }()

	reader, writer := io.Pipe()
	cancelled := make(chan struct{})
	bridge := &cursorBridge{
		reader:        reader,
		writer:        writer,
		cancel:        func() { close(cancelled) },
		heartbeatDone: make(chan struct{}),
		lastUsed:      time.Now(),
	}
	provider := &CursorProvider{bridges: make(map[string]*cursorBridge)}
	provider.retainBridge("account\x00conversation", bridge)

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("retained bridge did not expire")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.bridges) != 0 {
		t.Fatalf("retained bridges = %d, want 0", len(provider.bridges))
	}
}

func TestCursorListModelsUsesHTTP2AndRefreshesOnce(t *testing.T) {
	resolver := &credentialFixture{}
	requests := 0
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.ProtoMajor != 2 {
			t.Errorf("protocol = %s", request.Proto)
		}
		if request.Header.Get("X-Cursor-Client-Type") != "cli" {
			t.Errorf("missing client type")
		}
		if request.Header.Get("X-Account-Binding") != "account-a" {
			t.Errorf("missing account binding")
		}
		if requests == 1 {
			if request.Header.Get("Authorization") != "Bearer expired" {
				t.Errorf("first token was not cached token")
			}
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Header.Get("Authorization") != "Bearer fresh" {
			t.Errorf("refresh token not used")
		}
		payload, _ := proto.Marshal(&cursorpb.GetUsableModelsResponse{Models: []*cursorpb.ModelDetails{{ModelId: "composer-1", DisplayName: "Composer 1"}}})
		_, _ = writer.Write(payload)
	}))
	defer server.Close()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.ListModels(ctx, []schemas.Key{{ID: "account-a", Value: *schemas.NewSecretVar(""), CredentialResolver: resolver}}, &schemas.BifrostListModelsRequest{Unfiltered: true})
	if bifrostErr != nil {
		t.Fatal(bifrostErr)
	}
	if len(response.Data) != 1 || response.Data[0].ID != "cursor/composer-1" {
		t.Fatalf("models = %#v", response.Data)
	}
	resolver.mu.Lock()
	calls := append([]bool(nil), resolver.calls...)
	resolver.mu.Unlock()
	if len(calls) != 2 || calls[0] || !calls[1] {
		t.Fatalf("resolver calls = %v", calls)
	}
}

func TestCursorListModelsKeepsHealthyAccountsWhenAnotherFails(t *testing.T) {
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer broken" {
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = writer.Write([]byte("account unavailable"))
			return
		}
		payload, _ := proto.Marshal(&cursorpb.GetUsableModelsResponse{Models: []*cursorpb.ModelDetails{{ModelId: "composer-1", DisplayName: "Composer 1"}}})
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.ListModels(ctx, []schemas.Key{
		{ID: "broken-account", Value: *schemas.NewSecretVar("broken"), Models: schemas.WhiteList{"*"}},
		{ID: "healthy-account", Value: *schemas.NewSecretVar("healthy"), Models: schemas.WhiteList{"*"}},
	}, &schemas.BifrostListModelsRequest{Provider: schemas.CursorProvider})
	if bifrostErr != nil {
		t.Fatalf("ListModels() error = %v", bifrostErr)
	}
	if len(response.Data) != 1 || response.Data[0].ID != "cursor/composer-1" {
		t.Fatalf("models = %#v", response.Data)
	}
	if len(response.KeyStatuses) != 2 {
		t.Fatalf("key statuses = %#v", response.KeyStatuses)
	}
	statuses := map[string]schemas.KeyStatusType{}
	for _, status := range response.KeyStatuses {
		statuses[status.KeyID] = status.Status
	}
	if statuses["healthy-account"] != schemas.KeyStatusSuccess || statuses["broken-account"] != schemas.KeyStatusListModelsFailed {
		t.Fatalf("key statuses = %#v", statuses)
	}
}

func TestCursorListModelsMergesFiltersDeduplicatesAndPaginatesAccounts(t *testing.T) {
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		models := []*cursorpb.ModelDetails{
			{ModelId: "composer-1", DisplayName: "Composer 1"},
		}
		if request.Header.Get("Authorization") == "Bearer account-a" {
			models = append(models, &cursorpb.ModelDetails{ModelId: "alpha", DisplayName: "Alpha"})
		} else {
			models = append(models, &cursorpb.ModelDetails{ModelId: "blocked-beta", DisplayName: "Blocked Beta"})
		}
		payload, _ := proto.Marshal(&cursorpb.GetUsableModelsResponse{Models: models})
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	keys := []schemas.Key{
		{ID: "account-a", Value: *schemas.NewSecretVar("account-a"), Models: schemas.WhiteList{"*"}},
		{ID: "account-b", Value: *schemas.NewSecretVar("account-b"), Models: schemas.WhiteList{"*"}, BlacklistedModels: schemas.BlackList{"blocked-beta"}},
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	first, bifrostErr := provider.ListModels(ctx, keys, &schemas.BifrostListModelsRequest{Provider: schemas.CursorProvider, PageSize: 1})
	if bifrostErr != nil {
		t.Fatalf("first ListModels() error = %v", bifrostErr)
	}
	if len(first.Data) != 1 || first.Data[0].ID != "cursor/alpha" || first.NextPageToken == "" {
		t.Fatalf("first page = %#v", first)
	}
	if len(first.KeyStatuses) != 2 {
		t.Fatalf("first key statuses = %#v", first.KeyStatuses)
	}

	second, bifrostErr := provider.ListModels(ctx, keys, &schemas.BifrostListModelsRequest{
		Provider: schemas.CursorProvider, PageSize: 1, PageToken: first.NextPageToken,
	})
	if bifrostErr != nil {
		t.Fatalf("second ListModels() error = %v", bifrostErr)
	}
	if len(second.Data) != 1 || second.Data[0].ID != "cursor/composer-1" || second.NextPageToken != "" {
		t.Fatalf("second page = %#v", second)
	}
}

func TestCursorResponsesMapsReasoningTextAndHeartbeat(t *testing.T) {
	originalInterval := cursorHeartbeatInterval
	cursorHeartbeatInterval = 5 * time.Millisecond
	defer func() { cursorHeartbeatInterval = originalInterval }()
	heartbeatSeen := make(chan struct{}, 1)
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.ProtoMajor != 2 {
			t.Errorf("protocol = %s", request.Proto)
		}
		_, payload, err := readCursorFrame(request.Body)
		if err != nil {
			t.Errorf("read run frame: %v", err)
			return
		}
		var first cursorpb.AgentClientMessage
		if proto.Unmarshal(payload, &first) != nil || first.GetRunRequest() == nil {
			t.Error("missing run request")
			return
		}
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		go func() {
			_, payload, err := readCursorFrame(request.Body)
			if err == nil {
				var message cursorpb.AgentClientMessage
				if proto.Unmarshal(payload, &message) == nil && message.GetClientHeartbeat() != nil {
					heartbeatSeen <- struct{}{}
				}
			}
		}()
		time.Sleep(15 * time.Millisecond)
		writeServerMessage(t, writer, &cursorpb.AgentServerMessage{Message: &cursorpb.AgentServerMessage_InteractionUpdate{InteractionUpdate: &cursorpb.InteractionUpdate{Message: &cursorpb.InteractionUpdate_ThinkingDelta{ThinkingDelta: &cursorpb.ThinkingDeltaUpdate{Text: "thought"}}}}})
		writeServerMessage(t, writer, &cursorpb.AgentServerMessage{Message: &cursorpb.AgentServerMessage_InteractionUpdate{InteractionUpdate: &cursorpb.InteractionUpdate{Message: &cursorpb.InteractionUpdate_TextDelta{TextDelta: &cursorpb.TextDeltaUpdate{Text: "answer"}}}}})
		writeEndFrame(t, writer)
	}))
	defer server.Close()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.Responses(ctx, schemas.Key{ID: "static", Value: *schemas.NewSecretVar("token")}, textRequest("cursor/composer-1", "conversation-a", "hello"))
	if bifrostErr != nil {
		t.Fatal(bifrostErr)
	}
	if len(response.Output) != 2 {
		t.Fatalf("output = %#v", response.Output)
	}
	if response.Output[0].ResponsesReasoning == nil || response.Output[0].Summary[0].Text != "thought" {
		t.Fatalf("reasoning = %#v", response.Output[0])
	}
	if _, text := responseMessageText(response.Output[1]); text != "answer" {
		t.Fatalf("text = %q", text)
	}
	select {
	case <-heartbeatSeen:
	case <-time.After(time.Second):
		t.Fatal("heartbeat was not sent")
	}
}

func TestCursorResponsesRefreshesExactlyOnceAfterUnauthorized(t *testing.T) {
	resolver := &credentialFixture{}
	requests := 0
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		_, _, err := readCursorFrame(request.Body)
		if err != nil {
			t.Errorf("read run frame: %v", err)
			return
		}
		if request.Header.Get("X-Account-Binding") != "account-b" {
			t.Errorf("credential ID was not forwarded")
		}
		if requests == 1 {
			if request.Header.Get("Authorization") != "Bearer expired" {
				t.Errorf("first authorization = %q", request.Header.Get("Authorization"))
			}
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Header.Get("Authorization") != "Bearer fresh" {
			t.Errorf("refreshed authorization = %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writeServerMessage(t, writer, &cursorpb.AgentServerMessage{Message: &cursorpb.AgentServerMessage_InteractionUpdate{InteractionUpdate: &cursorpb.InteractionUpdate{Message: &cursorpb.InteractionUpdate_TextDelta{TextDelta: &cursorpb.TextDeltaUpdate{Text: "fresh response"}}}}})
		writeEndFrame(t, writer)
	}))
	defer server.Close()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	response, bifrostErr := provider.Responses(ctx, schemas.Key{ID: "account-b", Value: *schemas.NewSecretVar(""), CredentialResolver: resolver}, textRequest("cursor/composer-1", "refresh-conversation", "hello"))
	if bifrostErr != nil {
		t.Fatal(bifrostErr)
	}
	if len(response.Output) != 1 {
		t.Fatalf("output = %#v", response.Output)
	}
	resolver.mu.Lock()
	calls := append([]bool(nil), resolver.calls...)
	resolver.mu.Unlock()
	if len(calls) != 2 || calls[0] || !calls[1] {
		t.Fatalf("resolver calls = %v", calls)
	}
}

func TestCursorToolCallResultContinuesSameConnectStream(t *testing.T) {
	resultSeen := make(chan string, 1)
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _, err := readCursorFrame(request.Body)
		if err != nil {
			t.Errorf("read run frame: %v", err)
			return
		}
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		writeServerMessage(t, writer, &cursorpb.AgentServerMessage{
			Message: &cursorpb.AgentServerMessage_ExecServerMessage{
				ExecServerMessage: &cursorpb.ExecServerMessage{
					Id: 7, ExecId: "exec-7",
					Message: &cursorpb.ExecServerMessage_McpArgs{
						McpArgs: &cursorpb.McpArgs{ToolName: "lookup", ToolCallId: "call-7"},
					},
				},
			},
		})
		writer.(http.Flusher).Flush()
		_, payload, err := readCursorFrame(request.Body)
		if err != nil {
			t.Errorf("read tool result: %v", err)
			return
		}
		var client cursorpb.AgentClientMessage
		if proto.Unmarshal(payload, &client) != nil || client.GetExecClientMessage().GetMcpResult().GetSuccess() == nil {
			t.Error("missing MCP result")
			return
		}
		resultSeen <- client.GetExecClientMessage().GetMcpResult().GetSuccess().GetContent()[0].GetText().GetText()
		writeServerMessage(t, writer, &cursorpb.AgentServerMessage{Message: &cursorpb.AgentServerMessage_InteractionUpdate{InteractionUpdate: &cursorpb.InteractionUpdate{Message: &cursorpb.InteractionUpdate_TextDelta{TextDelta: &cursorpb.TextDeltaUpdate{Text: "continued"}}}}})
		writeEndFrame(t, writer)
	}))
	defer server.Close()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{ID: "static", Value: *schemas.NewSecretVar("token")}
	request := textRequest("cursor/composer-1", "tool-conversation", "use a tool")
	first, bifrostErr := provider.Responses(ctx, key, request)
	if bifrostErr != nil {
		t.Fatal(bifrostErr)
	}
	if len(first.Output) != 1 || first.Output[0].CallID == nil || *first.Output[0].CallID != "call-7" {
		t.Fatalf("tool output = %#v", first.Output)
	}
	provider.mu.Lock()
	retained := provider.bridges[bridgeKey(key.ID, *first.ID)]
	provider.mu.Unlock()
	if retained == nil || retained.pending.toolCallID != "call-7" {
		t.Fatalf("retained bridge = %#v", retained)
	}
	typeValue := schemas.ResponsesMessageTypeFunctionCallOutput
	callID, output := "call-7", "tool value"
	request.Params.PreviousResponseID = first.ID
	request.Input = append(request.Input, schemas.ResponsesMessage{Type: &typeValue, ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Output: &schemas.ResponsesToolMessageOutputStruct{ResponsesToolCallOutputStr: &output}}})
	second, bifrostErr := provider.Responses(ctx, key, request)
	if bifrostErr != nil {
		t.Fatal(bifrostErr)
	}
	if len(second.Output) != 1 {
		t.Fatalf("continued output = %#v", second.Output)
	}
	if _, text := responseMessageText(second.Output[0]); text != "continued" {
		t.Fatalf("continued text = %q", text)
	}
	select {
	case got := <-resultSeen:
		if got != output {
			t.Fatalf("result = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("tool result not observed")
	}
}

func TestCursorStreamCancellationClosesUpstreamAndMarksEnd(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _, _ = readCursorFrame(request.Body)
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
		cancelled <- struct{}{}
	}))
	defer server.Close()
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	passThrough := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, bifrostErr
	}
	stream, bifrostErr := provider.ResponsesStream(ctx, passThrough, nil, schemas.Key{ID: "static", Value: *schemas.NewSecretVar("token")}, textRequest("cursor/composer-1", "cancel-conversation", "wait"))
	if bifrostErr != nil {
		t.Fatal(bifrostErr)
	}
	cancel()
	for range stream {
	}
	if ended, _ := ctx.Value(schemas.BifrostContextKeyStreamEndIndicator).(bool); !ended {
		t.Fatal("stream end indicator not set")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream request was not cancelled")
	}
}

func TestCursorConcurrentSharedCacheKeyContinuationsStayIsolated(t *testing.T) {
	var sequence atomic.Int32
	var conversationMu sync.Mutex
	conversationIDs := map[string]bool{}
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, payload, err := readCursorFrame(request.Body)
		if err != nil {
			t.Errorf("read run frame: %v", err)
			return
		}
		var client cursorpb.AgentClientMessage
		if proto.Unmarshal(payload, &client) != nil || client.GetRunRequest() == nil {
			t.Error("missing run request")
			return
		}
		conversationMu.Lock()
		conversationIDs[client.GetRunRequest().GetConversationId()] = true
		conversationMu.Unlock()
		index := sequence.Add(1)
		callID := fmt.Sprintf("call-%d", index)
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		writeServerMessage(t, writer, &cursorpb.AgentServerMessage{
			Message: &cursorpb.AgentServerMessage_ExecServerMessage{
				ExecServerMessage: &cursorpb.ExecServerMessage{
					Id: uint32(index), ExecId: fmt.Sprintf("exec-%d", index),
					Message: &cursorpb.ExecServerMessage_McpArgs{
						McpArgs: &cursorpb.McpArgs{ToolName: "lookup", ToolCallId: callID},
					},
				},
			},
		})
		writer.(http.Flusher).Flush()
		_, payload, err = readCursorFrame(request.Body)
		if err != nil {
			t.Errorf("read %s result: %v", callID, err)
			return
		}
		client.Reset()
		if proto.Unmarshal(payload, &client) != nil || client.GetExecClientMessage() == nil {
			t.Errorf("missing result for %s", callID)
			return
		}
		got := client.GetExecClientMessage().GetMcpResult().GetSuccess().GetContent()[0].GetText().GetText()
		if got != "result-"+callID {
			t.Errorf("%s received %q", callID, got)
		}
		writeServerMessage(t, writer, &cursorpb.AgentServerMessage{Message: &cursorpb.AgentServerMessage_InteractionUpdate{InteractionUpdate: &cursorpb.InteractionUpdate{Message: &cursorpb.InteractionUpdate_TextDelta{TextDelta: &cursorpb.TextDeltaUpdate{Text: "done-" + callID}}}}})
		writeEndFrame(t, writer)
	}))
	defer server.Close()

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{ID: "shared-account", Value: *schemas.NewSecretVar("token")}
	initial := make([]*schemas.BifrostResponsesResponse, 2)
	var initialWG sync.WaitGroup
	for i := range initial {
		initialWG.Add(1)
		go func(index int) {
			defer initialWG.Done()
			response, bifrostErr := provider.Responses(ctx, key, textRequest("cursor/composer-1", "shared-cache-key", fmt.Sprintf("request-%d", index)))
			if bifrostErr != nil {
				t.Errorf("initial %d: %v", index, bifrostErr)
				return
			}
			initial[index] = response
		}(i)
	}
	initialWG.Wait()
	if initial[0] == nil || initial[1] == nil || initial[0].ID == nil || initial[1].ID == nil || *initial[0].ID == *initial[1].ID {
		t.Fatalf("continuation IDs are not unique: %#v", initial)
	}
	conversationMu.Lock()
	conversationCount := len(conversationIDs)
	conversationMu.Unlock()
	if conversationCount != 2 {
		t.Fatalf("upstream conversation IDs = %d", conversationCount)
	}

	var continuationWG sync.WaitGroup
	for i, first := range initial {
		continuationWG.Add(1)
		go func(index int, first *schemas.BifrostResponsesResponse) {
			defer continuationWG.Done()
			callID := *first.Output[0].CallID
			request := textRequest("cursor/composer-1", "shared-cache-key", "continue")
			request.Params.PreviousResponseID = first.ID
			itemType := schemas.ResponsesMessageTypeFunctionCallOutput
			output := "result-" + callID
			request.Input = append(request.Input, schemas.ResponsesMessage{Type: &itemType, ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Output: &schemas.ResponsesToolMessageOutputStruct{ResponsesToolCallOutputStr: &output}}})
			response, bifrostErr := provider.Responses(ctx, key, request)
			if bifrostErr != nil {
				t.Errorf("continuation %d: %v", index, bifrostErr)
				return
			}
			if len(response.Output) != 1 {
				t.Errorf("continuation %d output = %#v", index, response.Output)
				return
			}
			if _, text := responseMessageText(response.Output[0]); text != "done-"+callID {
				t.Errorf("continuation %d text = %q", index, text)
			}
		}(i, first)
	}
	continuationWG.Wait()
}

func TestCursorContinuationRequiresExactResponseAndToolBinding(t *testing.T) {
	provider := &CursorProvider{bridges: make(map[string]*cursorBridge)}
	request := textRequest("cursor/composer-1", "shared", "continue")
	unknown := "unknown-response"
	request.Params.PreviousResponseID = &unknown
	itemType := schemas.ResponsesMessageTypeFunctionCallOutput
	callID, output := "call-other", "value"
	request.Input = append(request.Input, schemas.ResponsesMessage{Type: &itemType, ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Output: &schemas.ResponsesToolMessageOutputStruct{ResponsesToolCallOutputStr: &output}}})
	_, bifrostErr := provider.prepareBridge(context.Background(), schemas.Key{ID: "account"}, request)
	if bifrostErr == nil || !strings.Contains(bifrostErr.GetErrorString(), "unknown or expired") {
		t.Fatalf("error = %v", bifrostErr)
	}

	continuationID := "known-response"
	reader, writer := io.Pipe()
	bridge := &cursorBridge{
		reader: reader, writer: writer, frames: &cursorFrameWriter{writer: writer}, cancel: func() {},
		heartbeatDone: make(chan struct{}), continuationID: continuationID,
		pending: cursorPendingExec{toolCallID: "call-required"}, lastUsed: time.Now(),
	}
	provider.retainBridge(bridgeKey("account", continuationID), bridge)
	request.Params.PreviousResponseID = &continuationID
	_, bifrostErr = provider.prepareBridge(context.Background(), schemas.Key{ID: "account"}, request)
	if bifrostErr == nil || !strings.Contains(bifrostErr.GetErrorString(), "matching function_call_output") {
		t.Fatalf("mismatched call error = %v", bifrostErr)
	}
	provider.mu.Lock()
	retained := provider.bridges[bridgeKey("account", continuationID)]
	provider.mu.Unlock()
	if retained != bridge {
		t.Fatal("mismatched tool result displaced the retained bridge")
	}
	provider.takeBridge(bridgeKey("account", continuationID)).close()
}

func TestCursorStartupCancellationReachesUpstreamBeforeHeaders(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	upstreamCancelled := make(chan struct{}, 1)
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		requestStarted <- struct{}{}
		<-request.Context().Done()
		upstreamCancelled <- struct{}{}
	}))
	defer server.Close()
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	done := make(chan *schemas.BifrostError, 1)
	go func() {
		_, bifrostErr := provider.Responses(ctx, schemas.Key{ID: "static", Value: *schemas.NewSecretVar("token")}, textRequest("cursor/composer-1", "startup-cancel", "wait"))
		done <- bifrostErr
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case bifrostErr := <-done:
		if bifrostErr == nil {
			t.Fatal("startup cancellation returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("startup did not honor caller cancellation")
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream startup context was not cancelled")
	}
}

func TestCursorResponseHeaderTimeoutBoundsStartup(t *testing.T) {
	provider, server := newProtocolProvider(t, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	transport, ok := provider.streamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", provider.streamClient.Transport)
	}
	transport.ResponseHeaderTimeout = 20 * time.Millisecond
	provider.responseHeaderTimeout = 20 * time.Millisecond
	started := time.Now()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Responses(ctx, schemas.Key{ID: "static", Value: *schemas.NewSecretVar("token")}, textRequest("cursor/composer-1", "header-timeout", "wait"))
	if bifrostErr == nil {
		t.Fatal("header timeout returned success")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("header timeout took %s", elapsed)
	}
}

func TestBuildRunRequestConvertsToolsAndConversation(t *testing.T) {
	request := textRequest("cursor/auto", "stable-key", "hello")
	name, description := "lookup", "look up a value"
	request.Params.Tools = []schemas.ResponsesTool{{Type: schemas.ResponsesToolTypeFunction, Name: &name, Description: &description, ResponsesToolFunction: &schemas.ResponsesToolFunction{Parameters: &schemas.ToolFunctionParameters{Type: "object"}}}}
	wire, blobs, err := buildRunRequest(request, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if wire.GetRequestedModel().GetModelId() != "default" {
		t.Fatalf("model = %q", wire.GetRequestedModel().GetModelId())
	}
	if len(wire.GetMcpTools().GetMcpTools()) != 1 || wire.GetMcpTools().GetMcpTools()[0].GetName() != name {
		t.Fatalf("tools = %#v", wire.GetMcpTools())
	}
	if len(blobs) == 0 || wire.GetConversationId() == "" {
		t.Fatal("conversation state was not built")
	}
}

func TestBuildRunRequestValidatesContentAndHostedTools(t *testing.T) {
	t.Run("image", func(t *testing.T) {
		request := textRequest("cursor/default", "stable-key", "describe this")
		imageURL := "data:image/png;base64,iVBORw0KGgo="
		request.Input[0].Content = &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{{
			Type:                                   schemas.ResponsesInputMessageContentBlockTypeImage,
			ResponsesInputMessageContentBlockImage: &schemas.ResponsesInputMessageContentBlockImage{ImageURL: &imageURL},
		}}}
		if _, _, err := buildRunRequest(request, "default"); err == nil || !strings.Contains(err.Error(), "input_image") {
			t.Fatalf("expected explicit image rejection, got %v", err)
		}
	})

	t.Run("web search", func(t *testing.T) {
		request := textRequest("cursor/default", "stable-key", "search")
		request.Params.Tools = []schemas.ResponsesTool{{Type: schemas.ResponsesToolTypeWebSearch}}
		run, _, err := buildRunRequest(request, "default")
		if err != nil {
			t.Fatalf("expected Cursor-native web search to be accepted, got %v", err)
		}
		if got := len(run.GetMcpTools().GetMcpTools()); got != 0 {
			t.Fatalf("native web search must not be projected as an MCP tool, got %d tools", got)
		}
	})

	t.Run("unsupported hosted tool", func(t *testing.T) {
		request := textRequest("cursor/default", "stable-key", "search files")
		request.Params.Tools = []schemas.ResponsesTool{{Type: schemas.ResponsesToolTypeFileSearch}}
		if _, _, err := buildRunRequest(request, "default"); err == nil || !strings.Contains(err.Error(), "file_search") {
			t.Fatalf("expected explicit file_search rejection, got %v", err)
		}
	})
}

func TestCursorNativeWebEnablesContextAndApprovesSearch(t *testing.T) {
	var frames bytes.Buffer
	bridge := &cursorBridge{
		frames:           &cursorFrameWriter{writer: &frames},
		tools:            &cursorpb.McpTools{},
		nativeWebEnabled: true,
	}
	query := &cursorpb.InteractionQuery{
		Id: 42,
		Query: &cursorpb.InteractionQuery_WebSearchRequestQuery{WebSearchRequestQuery: &cursorpb.WebSearchRequestQuery{
			Args: &cursorpb.WebSearchArgs{SearchTerm: "latest Bifrost release", ToolCallId: "ws_cursor_1"},
		}},
	}
	events, err := bridge.approveNativeWeb(query)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Item == nil || events[0].Item.Type == nil || *events[0].Item.Type != schemas.ResponsesMessageTypeWebSearchCall {
		t.Fatalf("web search start events = %#v", events)
	}
	action := events[0].Item.ResponsesToolMessage.Action.ResponsesWebSearchToolCallAction
	if action == nil || action.Query == nil || *action.Query != "latest Bifrost release" {
		t.Fatalf("web search action = %#v", action)
	}
	_, payload, err := readCursorFrame(io.NopCloser(bytes.NewReader(frames.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	var client cursorpb.AgentClientMessage
	if err := proto.Unmarshal(payload, &client); err != nil {
		t.Fatal(err)
	}
	response := client.GetInteractionResponse()
	if response == nil || response.Id != 42 || response.GetWebSearchRequestResponse().GetApproved() == nil {
		t.Fatalf("interaction response = %#v", response)
	}

	frames.Reset()
	if err := bridge.sendRequestContext(&cursorpb.ExecServerMessage{Id: 7, ExecId: "context-1"}); err != nil {
		t.Fatal(err)
	}
	_, payload, err = readCursorFrame(io.NopCloser(bytes.NewReader(frames.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	client.Reset()
	if err := proto.Unmarshal(payload, &client); err != nil {
		t.Fatal(err)
	}
	contextMessage := client.GetExecClientMessage().GetRequestContextResult().GetSuccess().GetRequestContext()
	if !contextMessage.GetWebSearchEnabled() || !contextMessage.GetWebFetchEnabled() {
		t.Fatalf("native web flags = %#v", contextMessage)
	}
	done := bridge.completeNativeWebEvents()
	if len(done) != 2 || done[1].Item == nil || done[1].Item.Status == nil || *done[1].Item.Status != schemas.ResponsesResponseStatusCompleted {
		t.Fatalf("web search completion events = %#v", done)
	}
}

func TestCursorContinuationUsageReportsPerCallDelta(t *testing.T) {
	bridge := &cursorBridge{continuationID: "response-1", totalTokens: 100, outputTokens: 20}
	first := bridge.completedEvent("gpt-5.6-sol").Response.Usage
	if first.InputTokens != 80 || first.OutputTokens != 20 || first.TotalTokens != 100 {
		t.Fatalf("first usage = %#v", first)
	}

	bridge.totalTokens = 150
	bridge.outputTokens = 30
	second := bridge.completedEvent("gpt-5.6-sol").Response.Usage
	if second.InputTokens != 40 || second.OutputTokens != 10 || second.TotalTokens != 50 {
		t.Fatalf("continuation usage = %#v", second)
	}
}
