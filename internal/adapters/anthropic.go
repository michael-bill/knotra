package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

func anthropicRequest(c contract.ModelConnection, messages []message, tools []functionTool, structured bool) (map[string]any, error) {
	var systems []string
	history := []anthropicMessage{}
	for _, msg := range messages {
		if msg.Role == "system" {
			systems = append(systems, msg.Content)
			continue
		}
		role := msg.Role
		blocks := []any{}
		switch {
		case msg.Role == "assistant" && len(msg.ProviderContext) > 0:
			var content []json.RawMessage
			if json.Unmarshal(msg.ProviderContext, &content) != nil {
				return nil, invalidModelResponse()
			}
			for _, block := range content {
				blocks = append(blocks, block)
			}
		case msg.Role == "tool":
			if msg.ToolCallID == "" {
				return nil, fmt.Errorf("anthropic tool result lacks call ID")
			}
			role = "user"
			var result struct {
				IsError bool `json:"isError"`
			}
			_ = json.Unmarshal([]byte(msg.Content), &result)
			isError := result.IsError || strings.HasPrefix(msg.Content, "Invalid arguments:") || strings.HasPrefix(msg.Content, "Validation error:")
			blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": msg.ToolCallID, "content": msg.Content, "is_error": isError})
		default:
			blocks = append(blocks, map[string]any{"type": "text", "text": msg.Content})
		}
		if len(history) > 0 && history[len(history)-1].Role == role {
			history[len(history)-1].Content = append(history[len(history)-1].Content, blocks...)
		} else {
			history = append(history, anthropicMessage{Role: role, Content: blocks})
		}
	}
	body := map[string]any{"model": c.Model, "messages": history, "stream": true, "max_tokens": 4096}
	if len(systems) > 0 {
		body["system"] = strings.Join(systems, "\n\n")
	}
	for name, value := range c.Parameters {
		body[name] = value
	}
	if len(tools) > 0 {
		definitions := []any{}
		for _, tool := range tools {
			definitions = append(definitions, map[string]any{"name": tool.Function.Name, "description": tool.Function.Description, "input_schema": tool.Function.Parameters})
		}
		body["tools"] = definitions
		choice := map[string]any{"type": "auto", "disable_parallel_tool_use": true}
		if structured {
			choice["type"], choice["name"] = "tool", structuredOutputTool
		}
		body["tool_choice"] = choice
	}
	return body, nil
}

type anthropicResponse struct {
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func normalizeAnthropic(response anthropicResponse) (chatResponse, error) {
	if response.Type != "message" || response.Role != "assistant" {
		return chatResponse{}, invalidModelResponse()
	}
	switch response.StopReason {
	case "end_turn", "tool_use", "stop_sequence":
	default:
		return chatResponse{}, invalidModelResponse()
	}
	result := chatResponse{Done: true, DoneReason: response.StopReason, Message: message{Role: "assistant"}, PromptEvalCount: response.Usage.InputTokens + response.Usage.CacheCreationInputTokens + response.Usage.CacheReadInputTokens, EvalCount: response.Usage.OutputTokens}
	ids := map[string]bool{}
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			result.Message.Content += block.Text
		case "tool_use":
			if block.ID == "" || block.Name == "" || ids[block.ID] || !json.Valid(block.Input) {
				return chatResponse{}, invalidModelResponse()
			}
			ids[block.ID] = true
			call := toolCall{ID: block.ID}
			call.Function.Name, call.Function.Arguments = block.Name, block.Input
			result.Message.ToolCalls = append(result.Message.ToolCalls, call)
		case "thinking", "redacted_thinking":
			// Private blocks, including signatures, belong only to conversation state.
		default:
			return chatResponse{}, invalidModelResponse()
		}
	}
	if response.StopReason == "tool_use" && len(result.Message.ToolCalls) == 0 {
		return chatResponse{}, invalidModelResponse()
	}
	if response.StopReason != "tool_use" && len(result.Message.ToolCalls) > 0 {
		return chatResponse{}, invalidModelResponse()
	}
	if result.Message.Content == "" && len(result.Message.ToolCalls) == 0 {
		return chatResponse{}, invalidModelResponse()
	}
	data, err := json.Marshal(response.Content)
	if err != nil {
		return chatResponse{}, err
	}
	result.Message.ProviderContext = data
	return result, nil
}

func readAnthropicResponse(ctx context.Context, res *http.Response, started time.Time, observation *executionObservation, id string, step int) (chatResponse, time.Duration, error) {
	if !isEventStream(res) {
		var response anthropicResponse
		if err := readModelJSON(res.Body, &response); err != nil {
			return chatResponse{}, 0, err
		}
		result, err := normalizeAnthropic(response)
		return result, 0, err
	}
	var response anthropicResponse
	startedMessage, receivedStop := false, false
	open := map[int]bool{}
	arguments := map[int]string{}
	return readModelEvents(ctx, res.Body, started, observation, id, step, func(data []byte) (string, *chatResponse, error) {
		var event struct {
			Type    string
			Index   int
			Message anthropicResponse
			Block   anthropicBlock `json:"content_block"`
			Delta   struct {
				Type, Text, Thinking, Signature string
				PartialJSON                     string `json:"partial_json"`
				StopReason                      string `json:"stop_reason"`
			}
			Usage struct {
				OutputTokens int64 `json:"output_tokens"`
			}
		}
		if json.Unmarshal(data, &event) != nil {
			return "", nil, invalidModelResponse()
		}
		switch event.Type {
		case "ping":
			return "", nil, nil
		case "message_start":
			if startedMessage || len(event.Message.Content) != 0 {
				return "", nil, invalidModelResponse()
			}
			response, startedMessage = event.Message, true
		case "content_block_start":
			if !startedMessage || receivedStop || event.Index != len(response.Content) {
				return "", nil, invalidModelResponse()
			}
			response.Content = append(response.Content, event.Block)
			open[event.Index] = true
			if event.Block.Type == "text" {
				return event.Block.Text, nil, nil
			}
		case "content_block_delta":
			if !open[event.Index] {
				return "", nil, invalidModelResponse()
			}
			block := &response.Content[event.Index]
			switch event.Delta.Type {
			case "text_delta":
				if block.Type != "text" {
					return "", nil, invalidModelResponse()
				}
				block.Text += event.Delta.Text
				return event.Delta.Text, nil, nil
			case "input_json_delta":
				if block.Type != "tool_use" {
					return "", nil, invalidModelResponse()
				}
				arguments[event.Index] += event.Delta.PartialJSON
				if block.Name == structuredOutputTool {
					return event.Delta.PartialJSON, nil, nil
				}
			case "thinking_delta":
				if block.Type != "thinking" {
					return "", nil, invalidModelResponse()
				}
				block.Thinking += event.Delta.Thinking
			case "signature_delta":
				if block.Type != "thinking" {
					return "", nil, invalidModelResponse()
				}
				block.Signature += event.Delta.Signature
			default:
				return "", nil, invalidModelResponse()
			}
		case "content_block_stop":
			if !open[event.Index] {
				return "", nil, invalidModelResponse()
			}
			delete(open, event.Index)
			if data, ok := arguments[event.Index]; ok {
				response.Content[event.Index].Input = json.RawMessage(data)
			}
		case "message_delta":
			if !startedMessage || len(open) > 0 {
				return "", nil, invalidModelResponse()
			}
			if event.Delta.StopReason != "" {
				response.StopReason, receivedStop = event.Delta.StopReason, true
			}
			response.Usage.OutputTokens = event.Usage.OutputTokens
		case "message_stop":
			if !startedMessage || !receivedStop || len(open) > 0 {
				return "", nil, invalidModelResponse()
			}
			result, err := normalizeAnthropic(response)
			return "", &result, err
		case "error":
			return "", nil, invalidModelResponse()
		}
		return "", nil, nil
	})
}
