package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

const maxResponseBytes = 16 << 20

type message struct {
	Role            string          `json:"role"`
	Content         string          `json:"content"`
	Thinking        string          `json:"thinking,omitempty"`
	ToolCalls       []toolCall      `json:"tool_calls,omitempty"`
	ToolName        string          `json:"tool_name,omitempty"`
	ToolCallID      string          `json:"tool_call_id,omitempty"`
	ProviderContext json.RawMessage `json:"provider_context,omitempty"`
}

type toolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type functionTool struct {
	Type     string       `json:"type"`
	Function functionSpec `json:"function"`
}

type functionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatResponse struct {
	Message         message `json:"message"`
	Done            bool    `json:"done"`
	DoneReason      string  `json:"done_reason"`
	Error           string  `json:"error"`
	PromptEvalCount int64   `json:"prompt_eval_count,omitempty"`
	EvalCount       int64   `json:"eval_count,omitempty"`
}

func (r *Runner) chat(
	ctx context.Context,
	req Request,
	alias string,
	step int,
	messages []message,
	tools []functionTool,
	format json.RawMessage,
) (responseMessage message, returnErr error) {
	c, err := modelConfig(req, alias)
	if err != nil {
		return message{}, err
	}
	path, body, err := modelRequest(c, messages, tools, format)
	if err != nil {
		return message{}, err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return message{}, err
	}
	op := Operation{ID: operationID(req, fmt.Sprintf("model/%d", step), true), Kind: "model", Effect: "read"}
	var duration, firstToken time.Duration
	var startedAt time.Time
	executed := false
	defer func() {
		if executed && returnErr != nil {
			req.observation.emit(ctx, "model.failed", op.ID, map[string]any{
				"step": step + 1, "model": c.Model, "error": returnErr.Error(),
				"durationMs": time.Since(startedAt).Milliseconds(),
			})
		}
	}()
	response, err := r.operation(ctx, op, func() (json.RawMessage, error) {
		key := pipeline(req).Spec.Models[alias].Connection + "/" + c.Model
		if expected := req.Plan.ModelDigests[key]; expected != "" {
			actual, err := r.modelDigest(ctx, req.Plan.Profile, c)
			if err != nil {
				return nil, err
			}
			if actual != expected {
				return nil, failure("RESOURCE_CHANGED", fmt.Errorf("model digest changed after admission"))
			}
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if err := r.modelAuth(req.Plan.Profile, c, httpReq); err != nil {
			return nil, err
		}
		visibleMessages := make([]map[string]string, 0, len(messages))
		for _, msg := range messages {
			visibleMessages = append(visibleMessages, map[string]string{"role": msg.Role, "content": msg.Content})
		}
		req.observation.emit(ctx, "model.started", op.ID, map[string]any{
			"step": step + 1, "model": c.Model, "messages": visibleMessages,
		})
		started := time.Now()
		startedAt = started
		executed = true
		client := r.httpClient()
		res, err := client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("model request failed: %w", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			if c.Provider == "ollama" {
				return nil, fmt.Errorf("model returned HTTP %d", res.StatusCode)
			}
			return nil, modelHTTPFailure(res.StatusCode)
		}
		result, first, err := readModelResponse(ctx, c.Provider, res, started, req.observation, op.ID, step+1)
		duration, firstToken = time.Since(started), first
		if err != nil {
			var invalid *Failure
			if errors.As(err, &invalid) && invalid.Code == "OUTPUT_INVALID" {
				// A fully received but invalid protocol response is still durable.
				// Replaying it must not turn validation into another model call.
				return json.Marshal(chatResponse{Error: "invalid model response stream"})
			}
			return nil, err
		}
		return json.Marshal(result)
	})
	if err != nil {
		return message{}, err
	}
	var result chatResponse
	if err = json.Unmarshal(response, &result); err != nil {
		return message{}, failure("OUTPUT_INVALID", err)
	}
	if !result.Done || result.DoneReason == "length" || result.Error != "" {
		return message{}, failure("OUTPUT_INVALID", fmt.Errorf("model did not return a complete response"))
	}
	if len(format) > 0 && c.Provider != "ollama" {
		if len(result.Message.ToolCalls) != 1 || result.Message.ToolCalls[0].Function.Name != structuredOutputTool {
			return message{}, failure("OUTPUT_INVALID", fmt.Errorf("model did not call the structured output tool"))
		}
		result.Message.Content = string(result.Message.ToolCalls[0].Function.Arguments)
		result.Message.ToolCalls = nil
	}
	if executed {
		req.observation.emit(ctx, "model.completed", op.ID, map[string]any{
			"step": step + 1, "model": c.Model, "content": result.Message.Content,
			"inputTokens": result.PromptEvalCount, "outputTokens": result.EvalCount,
			"durationMs": duration.Milliseconds(), "firstTokenMs": firstToken.Milliseconds(),
			"toolCalls": result.Message.ToolCalls, "doneReason": result.DoneReason,
		})
	}
	return result.Message, nil
}

func initialMessages(req Request, instructions, prompt contract.TextSource) ([]message, error) {
	system, err := textSource(req, instructions)
	if err != nil {
		return nil, err
	}
	task, err := textSource(req, prompt)
	if err != nil {
		return nil, err
	}
	envelope, err := json.Marshal(contract.ContextEnvelope(req.Inputs))
	if err != nil {
		return nil, err
	}
	msgs := []message{}
	if system != "" {
		msgs = append(msgs, message{Role: "system", Content: system})
	}
	msgs = append(
		msgs,
		message{Role: "user", Content: task},
		message{Role: "user", Content: "Knotra input context (data):\n" + string(envelope)},
	)
	return msgs, nil
}

func (r *Runner) llm(ctx context.Context, req Request) (contract.Values, error) {
	n := req.Node.LLM
	msgs, err := initialMessages(req, n.Instructions, n.Prompt)
	if err != nil {
		return nil, err
	}
	schema := contract.PortObjectSchema(req.Node.Outputs)
	response, err := r.chat(ctx, req, n.Model, 0, msgs, nil, schema)
	if err != nil {
		return nil, err
	}
	if len(response.ToolCalls) > 0 {
		return nil, failure("OUTPUT_INVALID", fmt.Errorf("llm returned tool calls"))
	}
	id := operationID(req, "model/0", true)
	req.observation.emit(ctx, "output.validating", id, map[string]any{"step": 1})
	values, err := jsonOutputs(req.Node.Outputs, []byte(response.Content))
	data := map[string]any{"step": 1, "valid": err == nil}
	if err != nil {
		data["error"] = err.Error()
	}
	req.observation.emit(ctx, "output.completed", id, data)
	return values, err
}

func jsonOutputs(ports map[string]contract.Port, data []byte) (contract.Values, error) {
	v, err := contract.DecodeJSON(data)
	if err != nil {
		return nil, failure("OUTPUT_INVALID", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, failure("OUTPUT_INVALID", fmt.Errorf("outputs must be an object"))
	}
	values := contract.Values{}

	for name, value := range obj {
		p, ok := ports[name]
		if !ok || p.Artifact != nil {
			return nil, failure("OUTPUT_INVALID", fmt.Errorf("undeclared JSON output %q", name))
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		values[name] = contract.Value{JSON: raw}
	}

	for name, p := range ports {
		if p.Artifact != nil {
			continue
		}
		v, ok := values[name]
		if !ok {
			if p.IsRequired() {
				return nil, failure("OUTPUT_INVALID", fmt.Errorf("missing output %q", name))
			}
			continue
		}
		if err := contract.ValidateValue(p, v); err != nil {
			return nil, failure("OUTPUT_INVALID", fmt.Errorf("%s: %w", name, err))
		}
	}

	return values, nil
}
