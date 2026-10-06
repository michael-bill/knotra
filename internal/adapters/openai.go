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

func openAIRequest(c contract.ModelConnection, messages []message, tools []functionTool, structured bool) (map[string]any, error) {
	input := []any{}
	for _, msg := range messages {
		switch {
		case msg.Role == "assistant" && len(msg.ProviderContext) > 0:
			var items []json.RawMessage
			if json.Unmarshal(msg.ProviderContext, &items) != nil {
				return nil, invalidModelResponse()
			}
			for _, item := range items {
				input = append(input, item)
			}
		case msg.Role == "tool":
			if msg.ToolCallID == "" {
				return nil, fmt.Errorf("OpenAI tool result lacks call ID")
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": msg.ToolCallID, "output": msg.Content})
		default:
			input = append(input, map[string]any{"role": msg.Role, "content": msg.Content})
		}
	}
	body := map[string]any{"model": c.Model, "input": input, "stream": true, "store": false, "include": []string{"reasoning.encrypted_content"}}
	for name, value := range c.Parameters {
		if name == "reasoning_effort" {
			body["reasoning"] = map[string]any{"effort": value}
		} else {
			body[name] = value
		}
	}
	if len(tools) > 0 {
		definitions := []any{}
		for _, tool := range tools {
			// Knotra validates the full schema locally. Non-strict tools preserve
			// optional fields and JSON Schema features outside OpenAI's subset.
			definitions = append(definitions, map[string]any{"type": "function", "name": tool.Function.Name, "description": tool.Function.Description, "parameters": tool.Function.Parameters, "strict": false})
		}
		body["tools"] = definitions
		body["parallel_tool_calls"] = false
		if structured {
			body["tool_choice"] = map[string]any{"type": "function", "name": structuredOutputTool}
		}
	}
	return body, nil
}

type openAIResponse struct {
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
	Usage  struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func normalizeOpenAI(response openAIResponse) (chatResponse, error) {
	if response.Status != "completed" {
		return chatResponse{}, invalidModelResponse()
	}
	result := chatResponse{Done: true, DoneReason: "stop", Message: message{Role: "assistant"}, PromptEvalCount: response.Usage.InputTokens, EvalCount: response.Usage.OutputTokens}
	ids := map[string]bool{}
	for _, raw := range response.Output {
		var item struct {
			Type, Role, Status, Name, Arguments string
			CallID                              string                        `json:"call_id"`
			Content                             []struct{ Type, Text string } `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return chatResponse{}, invalidModelResponse()
		}
		switch item.Type {
		case "message":
			if item.Role != "assistant" || (item.Status != "" && item.Status != "completed") {
				return chatResponse{}, invalidModelResponse()
			}
			for _, part := range item.Content {
				if part.Type != "output_text" {
					return chatResponse{}, invalidModelResponse()
				}
				result.Message.Content += part.Text
			}
		case "function_call":
			if item.CallID == "" || item.Name == "" || ids[item.CallID] || !json.Valid([]byte(item.Arguments)) {
				return chatResponse{}, invalidModelResponse()
			}
			ids[item.CallID] = true
			call := toolCall{ID: item.CallID}
			call.Function.Name, call.Function.Arguments = item.Name, json.RawMessage(item.Arguments)
			result.Message.ToolCalls = append(result.Message.ToolCalls, call)
		case "reasoning":
			// Encrypted reasoning is preserved for the next turn, never observed.
		default:
			return chatResponse{}, invalidModelResponse()
		}
	}
	if result.Message.Content == "" && len(result.Message.ToolCalls) == 0 {
		return chatResponse{}, invalidModelResponse()
	}
	context, err := json.Marshal(response.Output)
	if err != nil {
		return chatResponse{}, err
	}
	result.Message.ProviderContext = context
	return result, nil
}

func readOpenAIResponse(ctx context.Context, res *http.Response, started time.Time, observation *executionObservation, id string, step int) (chatResponse, time.Duration, error) {
	if !isEventStream(res) {
		var response openAIResponse
		if err := readModelJSON(res.Body, &response); err != nil {
			return chatResponse{}, 0, err
		}
		result, err := normalizeOpenAI(response)
		return result, 0, err
	}
	var text, arguments strings.Builder
	structuredItems := map[int]bool{}
	return readModelEvents(ctx, res.Body, started, observation, id, step, func(data []byte) (string, *chatResponse, error) {
		var event struct {
			Type, Delta string
			Response    openAIResponse
			OutputIndex int `json:"output_index"`
			Item        struct{ Type, Name string }
		}
		if json.Unmarshal(data, &event) != nil {
			return "", nil, invalidModelResponse()
		}
		switch event.Type {
		case "response.output_item.added":
			structuredItems[event.OutputIndex] = event.Item.Type == "function_call" && event.Item.Name == structuredOutputTool
		case "response.function_call_arguments.delta":
			if structuredItems[event.OutputIndex] {
				_, _ = arguments.WriteString(event.Delta)
				return event.Delta, nil, nil
			}
		case "response.output_text.delta":
			_, _ = text.WriteString(event.Delta)
			return event.Delta, nil, nil
		case "response.completed":
			result, err := normalizeOpenAI(event.Response)
			if err != nil {
				return "", nil, err
			}
			if text.Len() > 0 && text.String() != result.Message.Content {
				return "", nil, invalidModelResponse()
			}
			visible := ""
			if len(result.Message.ToolCalls) == 1 && result.Message.ToolCalls[0].Function.Name == structuredOutputTool {
				visible = string(result.Message.ToolCalls[0].Function.Arguments)
			}
			if arguments.Len() > 0 && arguments.String() != visible {
				return "", nil, invalidModelResponse()
			}
			return "", &result, nil
		case "response.failed", "response.incomplete", "error":
			return "", nil, invalidModelResponse()
		}
		return "", nil, nil
	})
}
