package cursor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/providers/cursor/cursorpb"
	"github.com/maximhq/bifrost/core/schemas"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

type cursorPendingExec struct {
	id         uint32
	execID     string
	toolCallID string
}

type cursorBridge struct {
	reader         io.ReadCloser
	writer         *io.PipeWriter
	frames         *cursorFrameWriter
	cancel         context.CancelFunc
	heartbeatDone  chan struct{}
	expiryTimer    *time.Timer
	closeOnce      sync.Once
	blobs          map[string][]byte
	tools          *cursorpb.McpTools
	cloudRule      string
	pending        cursorPendingExec
	continuationID string
	outputTokens   int64
	totalTokens    int64
	emittedOutput  int64
	emittedInput   int64
	lastUsed       time.Time
}

type cursorEventEmitter func(*schemas.BifrostResponsesStreamResponse) error

func (bridge *cursorBridge) process(ctx context.Context, model string, emit cursorEventEmitter) (bool, error) {
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			bridge.close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	sequence := 0
	emitEvent := func(event *schemas.BifrostResponsesStreamResponse) error {
		sequence++
		event.SequenceNumber = sequence
		event.ExtraFields.ChunkIndex = sequence
		return emit(event)
	}
	for {
		flags, payload, readErr := readCursorFrame(bridge.reader)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && ctx.Err() == nil {
				return false, nil
			}
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, readErr
		}
		if flags&2 != 0 {
			if err := parseEndStream(payload); err != nil {
				return false, err
			}
			return false, emitEvent(bridge.completedEvent(model))
		}
		var message cursorpb.AgentServerMessage
		if err := proto.Unmarshal(payload, &message); err != nil {
			return false, err
		}
		if update := message.GetInteractionUpdate(); update != nil {
			if delta := update.GetTextDelta(); delta != nil && delta.Text != "" {
				value := delta.Text
				if err := emitEvent(&schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeOutputTextDelta, Delta: &value}); err != nil {
					return false, err
				}
			}
			if delta := update.GetThinkingDelta(); delta != nil && delta.Text != "" {
				value := delta.Text
				if err := emitEvent(&schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeReasoningSummaryTextDelta, Delta: &value}); err != nil {
					return false, err
				}
			}
			if delta := update.GetTokenDelta(); delta != nil {
				bridge.outputTokens += int64(delta.Tokens)
			}
		}
		if checkpoint := message.GetConversationCheckpointUpdate(); checkpoint != nil && checkpoint.GetTokenDetails() != nil {
			bridge.totalTokens = int64(checkpoint.GetTokenDetails().GetUsedTokens())
		}
		if kv := message.GetKvServerMessage(); kv != nil {
			if err := bridge.handleKV(kv); err != nil {
				return false, err
			}
		}
		if exec := message.GetExecServerMessage(); exec != nil {
			if exec.GetRequestContextArgs() != nil {
				if err := bridge.sendRequestContext(exec); err != nil {
					return false, err
				}
				continue
			}
			if mcp := exec.GetMcpArgs(); mcp != nil {
				arguments := decodeMCPArgs(mcp.Args)
				raw, _ := schemas.MarshalSorted(arguments)
				callID := mcp.ToolCallId
				name := strings.TrimSpace(mcp.ToolName)
				if name == "" {
					name = strings.TrimSpace(mcp.Name)
				}
				itemType := schemas.ResponsesMessageTypeFunctionCall
				status := schemas.ResponsesResponseStatusCompleted
				itemID := "fc_" + callID
				argumentsJSON := string(raw)
				item := &schemas.ResponsesMessage{
					ID: &itemID, Type: &itemType, Status: &status,
					ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Name: &name, Arguments: &argumentsJSON},
				}
				if err := emitEvent(&schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeOutputItemDone, Item: item}); err != nil {
					return false, err
				}
				bridge.pending = cursorPendingExec{id: exec.Id, execID: exec.ExecId, toolCallID: callID}
				if err := emitEvent(bridge.completedEvent(model)); err != nil {
					return false, err
				}
				bridge.lastUsed = time.Now()
				return true, nil
			}
			return false, fmt.Errorf("cursor requested unsupported native exec operation")
		}
	}
}

func (bridge *cursorBridge) completedEvent(model string) *schemas.BifrostResponsesStreamResponse {
	cumulativeTotal := max(bridge.totalTokens, bridge.outputTokens)
	cumulativeOutput := max(int64(0), bridge.outputTokens)
	cumulativeInput := max(int64(0), cumulativeTotal-cumulativeOutput)
	input := max(int64(0), cumulativeInput-bridge.emittedInput)
	output := max(int64(0), cumulativeOutput-bridge.emittedOutput)
	total := input + output
	bridge.emittedInput = cumulativeInput
	bridge.emittedOutput = cumulativeOutput
	status := schemas.ResponsesResponseStatusCompleted
	responseID := bridge.continuationID
	return &schemas.BifrostResponsesStreamResponse{
		Type: schemas.ResponsesStreamResponseTypeCompleted,
		Response: &schemas.BifrostResponsesResponse{
			ID: &responseID, Object: "response", Model: model, Status: &status, Output: []schemas.ResponsesMessage{},
			Usage: &schemas.ResponsesResponseUsage{InputTokens: int(input), OutputTokens: int(output), TotalTokens: int(total)},
		},
	}
}

func (bridge *cursorBridge) sendToolResult(output string) error {
	content := &cursorpb.McpToolResultContentItem{Content: &cursorpb.McpToolResultContentItem_Text{Text: &cursorpb.McpTextContent{Text: output}}}
	result := &cursorpb.McpResult{Result: &cursorpb.McpResult_Success{Success: &cursorpb.McpSuccess{Content: []*cursorpb.McpToolResultContentItem{content}}}}
	exec := &cursorpb.ExecClientMessage{Id: bridge.pending.id, ExecId: bridge.pending.execID, Message: &cursorpb.ExecClientMessage_McpResult{McpResult: result}}
	bridge.pending = cursorPendingExec{}
	return writeCursorFrame(bridge.frames, &cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ExecClientMessage{ExecClientMessage: exec}})
}

func (bridge *cursorBridge) handleKV(message *cursorpb.KvServerMessage) error {
	client := &cursorpb.KvClientMessage{Id: message.Id}
	if get := message.GetGetBlobArgs(); get != nil {
		client.Message = &cursorpb.KvClientMessage_GetBlobResult{GetBlobResult: &cursorpb.GetBlobResult{BlobData: bridge.blobs[hex.EncodeToString(get.BlobId)]}}
	}
	if set := message.GetSetBlobArgs(); set != nil {
		bridge.blobs[hex.EncodeToString(set.BlobId)] = append([]byte(nil), set.BlobData...)
		client.Message = &cursorpb.KvClientMessage_SetBlobResult{SetBlobResult: &cursorpb.SetBlobResult{}}
	}
	return writeCursorFrame(bridge.frames, &cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_KvClientMessage{KvClientMessage: client}})
}

func (bridge *cursorBridge) sendRequestContext(exec *cursorpb.ExecServerMessage) error {
	requestContext := &cursorpb.RequestContext{Tools: bridge.tools.GetMcpTools()}
	if bridge.cloudRule != "" {
		requestContext.CloudRule = &bridge.cloudRule
	}
	result := &cursorpb.RequestContextResult{Result: &cursorpb.RequestContextResult_Success{Success: &cursorpb.RequestContextSuccess{RequestContext: requestContext}}}
	client := &cursorpb.ExecClientMessage{Id: exec.Id, ExecId: exec.ExecId, Message: &cursorpb.ExecClientMessage_RequestContextResult{RequestContextResult: result}}
	return writeCursorFrame(bridge.frames, &cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ExecClientMessage{ExecClientMessage: client}})
}

func decodeMCPArgs(values map[string][]byte) map[string]any {
	out := make(map[string]any, len(values))
	for key, raw := range values {
		var value structpb.Value
		if proto.Unmarshal(raw, &value) == nil {
			out[key] = value.AsInterface()
		} else {
			out[key] = string(raw)
		}
	}
	return out
}

func (bridge *cursorBridge) close() {
	bridge.closeOnce.Do(func() {
		if bridge.expiryTimer != nil {
			bridge.expiryTimer.Stop()
		}
		close(bridge.heartbeatDone)
		bridge.cancel()
		_ = bridge.writer.Close()
		_ = bridge.reader.Close()
	})
}

func parseConnectErrorBody(body []byte) string {
	var value map[string]any
	if json.Unmarshal(body, &value) == nil {
		if message, ok := value["message"].(string); ok && message != "" {
			return message
		}
	}
	return strings.TrimSpace(string(body))
}
