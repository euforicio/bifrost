package cursor

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/maximhq/bifrost/core/providers/cursor/cursorpb"
	"github.com/maximhq/bifrost/core/schemas"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func buildRunRequest(request *schemas.BifrostResponsesRequest, modelID string) (*cursorpb.AgentRunRequest, map[string][]byte, error) {
	if err := validateCursorRequest(request); err != nil {
		return nil, nil, err
	}
	blobs := make(map[string][]byte)
	store := func(data []byte) []byte {
		digest := sha256.Sum256(data)
		blobs[hex.EncodeToString(digest[:])] = append([]byte(nil), data...)
		return digest[:]
	}

	root := make([][]byte, 0, len(request.Input)+1)
	systemContext := cursorInstructions(request)
	if systemContext == "" {
		systemContext = "You are a helpful assistant."
	}
	systemJSON, _ := json.Marshal(map[string]any{"role": "system", "content": systemContext})
	root = append(root, store(systemJSON))

	activeUserIndex := -1
	for i := len(request.Input) - 1; i >= 0; i-- {
		if role, text := responseMessageText(request.Input[i]); role == "user" && text != "" {
			activeUserIndex = i
			break
		}
	}
	activeUser := ""
	for i, item := range request.Input {
		role, text := responseMessageText(item)
		if text == "" || role == "system" {
			continue
		}
		if i == activeUserIndex {
			activeUser = text
			continue
		}
		data, _ := json.Marshal(map[string]any{"role": role, "content": []map[string]any{{"type": "text", "text": text}}})
		root = append(root, store(data))
	}

	action := &cursorpb.ConversationAction{}
	if activeUser != "" {
		action.Action = &cursorpb.ConversationAction_UserMessageAction{UserMessageAction: &cursorpb.UserMessageAction{UserMessage: &cursorpb.UserMessage{Text: activeUser, MessageId: randomID()}}}
	} else {
		action.Action = &cursorpb.ConversationAction_ResumeAction{ResumeAction: &cursorpb.ResumeAction{}}
	}
	wireModel := modelID
	if wireModel == "auto" {
		wireModel = "default"
	}
	details := &cursorpb.ModelDetails{ModelId: wireModel, DisplayModelId: wireModel, DisplayName: modelID, DisplayNameShort: modelID}
	// Every new upstream stream gets an opaque conversation identity. A prompt
	// cache key is a cache hint, not a session key, and concurrent requests may
	// legitimately reuse it.
	conversationID := randomID()
	return &cursorpb.AgentRunRequest{
		ConversationState: &cursorpb.ConversationStateStructure{RootPromptMessagesJson: root},
		Action:            action, ModelDetails: details, McpTools: buildMCPTools(request),
		ConversationId: &conversationID, RequestedModel: &cursorpb.RequestedModel{ModelId: wireModel},
	}, blobs, nil
}

// validateCursorRequest prevents the bridge from silently dropping content or
// hosted tools that Cursor's AgentService wire format cannot represent.
func validateCursorRequest(request *schemas.BifrostResponsesRequest) error {
	for _, item := range request.Input {
		if item.Content == nil {
			continue
		}
		for _, block := range item.Content.ContentBlocks {
			switch block.Type {
			case schemas.ResponsesInputMessageContentBlockTypeText, schemas.ResponsesOutputMessageContentTypeText:
				continue
			default:
				return fmt.Errorf("cursor does not support Responses content block type %q", block.Type)
			}
		}
	}
	if request.Params != nil {
		for _, tool := range request.Params.Tools {
			if tool.Type != schemas.ResponsesToolTypeFunction {
				return fmt.Errorf("cursor does not support Responses tool type %q", tool.Type)
			}
		}
	}
	return nil
}

func cursorInstructions(request *schemas.BifrostResponsesRequest) string {
	parts := make([]string, 0, 2)
	if request.Params != nil && request.Params.Instructions != nil {
		if value := strings.TrimSpace(*request.Params.Instructions); value != "" {
			parts = append(parts, value)
		}
	}
	for _, item := range request.Input {
		role, text := responseMessageText(item)
		if role == "system" && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func responseMessageText(item schemas.ResponsesMessage) (string, string) {
	if item.Type != nil && *item.Type == schemas.ResponsesMessageTypeFunctionCall {
		name, arguments := "", ""
		if item.ResponsesToolMessage != nil {
			if item.Name != nil {
				name = *item.Name
			}
			if item.Arguments != nil {
				arguments = *item.Arguments
			}
		}
		return "assistant", "[Tool Call] " + name + " " + arguments
	}
	if item.Type != nil && *item.Type == schemas.ResponsesMessageTypeFunctionCallOutput {
		if output, ok := functionOutput(item); ok {
			return "user", "[Tool Result]\n" + output
		}
	}
	role := ""
	if item.Role != nil {
		role = string(*item.Role)
	}
	if role == "developer" || role == "system" {
		role = "system"
	}
	if item.Content == nil {
		return role, ""
	}
	if item.Content.ContentStr != nil {
		return role, *item.Content.ContentStr
	}
	parts := make([]string, 0, len(item.Content.ContentBlocks))
	for _, block := range item.Content.ContentBlocks {
		if block.Text != nil && *block.Text != "" {
			parts = append(parts, *block.Text)
		}
	}
	return role, strings.Join(parts, "\n")
}

func buildMCPTools(request *schemas.BifrostResponsesRequest) *cursorpb.McpTools {
	out := &cursorpb.McpTools{}
	if request.Params == nil {
		return out
	}
	for _, tool := range request.Params.Tools {
		if tool.Type != schemas.ResponsesToolTypeFunction || tool.Name == nil || strings.TrimSpace(*tool.Name) == "" {
			continue
		}
		schema := []byte(`{"type":"object","properties":{}}`)
		if tool.ResponsesToolFunction != nil && tool.ResponsesToolFunction.Parameters != nil {
			if encoded, err := schemas.MarshalSorted(tool.ResponsesToolFunction.Parameters); err == nil {
				schema = encoded
			}
		}
		var value structpb.Value
		if protojson.Unmarshal(schema, &value) != nil {
			continue
		}
		encoded, err := proto.Marshal(&value)
		if err != nil {
			continue
		}
		description := ""
		if tool.Description != nil {
			description = *tool.Description
		}
		name := strings.TrimSpace(*tool.Name)
		out.McpTools = append(out.McpTools, &cursorpb.McpToolDefinition{
			Name: name, Description: description, InputSchema: encoded,
			ProviderIdentifier: "bifrost", ToolName: name,
		})
	}
	return out
}

func functionOutput(item schemas.ResponsesMessage) (string, bool) {
	if item.Type == nil || *item.Type != schemas.ResponsesMessageTypeFunctionCallOutput || item.ResponsesToolMessage == nil || item.Output == nil {
		return "", false
	}
	if item.Output.ResponsesToolCallOutputStr != nil {
		return *item.Output.ResponsesToolCallOutputStr, true
	}
	if item.Output.ResponsesFunctionToolCallOutputBlocks != nil {
		encoded, err := schemas.MarshalSorted(item.Output.ResponsesFunctionToolCallOutputBlocks)
		return string(encoded), err == nil
	}
	return "", false
}

func matchingFunctionOutput(items []schemas.ResponsesMessage, callID string) (string, bool) {
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.ResponsesToolMessage == nil || item.CallID == nil || *item.CallID != callID {
			continue
		}
		if output, ok := functionOutput(item); ok {
			return output, true
		}
	}
	return "", false
}

type cursorFrameWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *cursorFrameWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(data)
}

func writeCursorFrame(writer io.Writer, message proto.Message) error {
	payload, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	_, err = writer.Write(frame)
	return err
}

func readCursorFrame(reader io.Reader) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > 64<<20 {
		return 0, nil, errors.New("cursor frame exceeds 64 MiB")
	}
	payload := make([]byte, size)
	_, err := io.ReadFull(reader, payload)
	return header[0], payload, err
}

func firstDataFrame(data []byte) ([]byte, error) {
	flags, payload, err := readCursorFrame(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if flags&2 != 0 {
		return nil, errors.New("cursor response contains no data frame")
	}
	return payload, nil
}

func parseEndStream(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var end struct {
		Error *struct{ Code, Message string } `json:"error"`
	}
	if json.Unmarshal(payload, &end) == nil && end.Error != nil {
		return fmt.Errorf("cursor Connect error %s: %s", end.Error.Code, end.Error.Message)
	}
	return nil
}

func randomID() string {
	data := make([]byte, 16)
	_, _ = rand.Read(data)
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", data[:4], data[4:6], data[6:8], data[8:10], data[10:])
}
