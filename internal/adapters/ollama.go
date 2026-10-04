package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

const maxResponseBytes = 16 << 20

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type toolCall struct {
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

var ollamaOptions = map[string]bool{
	"temperature":    true,
	"top_k":          true,
	"top_p":          true,
	"min_p":          true,
	"seed":           true,
	"num_predict":    true,
	"num_ctx":        true,
	"repeat_penalty": true,
	"repeat_last_n":  true,
	"stop":           true,
}

func modelConfig(req Request, alias string) (contract.ModelConnection, error) {
	resource, ok := pipeline(req).Spec.Models[alias]
	if !ok {
		return contract.ModelConnection{}, fmt.Errorf("unknown model alias %q", alias)
	}
	connection, ok := req.Plan.Profile.Spec.Models[resource.Connection]
	if !ok {
		return connection, fmt.Errorf("model connection missing")
	}
	if resource.Model != "" {
		connection.Model = resource.Model
	}
	params := map[string]any{}

	for k, v := range connection.Parameters {
		params[k] = v
	}

	for k, v := range resource.Parameters {
		params[k] = v
	}

	connection.Parameters = params
	if connection.Provider != "ollama" {
		return connection, fmt.Errorf("unsupported model adapter %q", connection.Provider)
	}

	for k := range connection.Auth {
		if k != "key" {
			return connection, fmt.Errorf("unsupported Ollama auth field %q", k)
		}
	}

	for k := range params {
		if !ollamaOptions[k] && k != "think" && k != "keep_alive" {
			return connection, fmt.Errorf("unsupported Ollama parameter %q", k)
		}
		if err := validateOllamaOption(k, params[k]); err != nil {
			return connection, err
		}
	}

	if connection.BaseURL == "" {
		connection.BaseURL = "http://127.0.0.1:11434"
	}
	u, err := url.Parse(connection.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return connection, fmt.Errorf("invalid Ollama baseUrl")
	}
	return connection, nil
}

func validateOllamaOption(name string, value any) error {
	invalid := func() error { return fmt.Errorf("invalid Ollama parameter %q", name) }
	if name == "think" {
		switch value.(type) {
		case bool:
			return nil
		case string:
			if value != "" {
				return nil
			}
		}

		return invalid()
	}
	if name == "keep_alive" {
		switch value.(type) {
		case string:
			return nil
		case float64, int, int64, json.Number:
			return nil
		}

		return invalid()
	}
	if name == "stop" {
		raw, _ := json.Marshal(value)
		var stops []string
		if err := json.Unmarshal(raw, &stops); err != nil || len(stops) == 0 {
			return invalid()
		}

		for _, s := range stops {
			if s == "" {
				return invalid()
			}
		}

		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return invalid()
	}
	var number float64
	if err = json.Unmarshal(raw, &number); err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return invalid()
	}

	switch name {
	case "temperature":
		if number < 0 || number > 2 {
			return invalid()
		}
	case "top_p", "min_p":
		if number < 0 || number > 1 {
			return invalid()
		}
	case "repeat_penalty":
		if number < 0 {
			return invalid()
		}
	case "num_predict", "num_ctx":
		if number != math.Trunc(number) || number < 1 || number > 1048576 {
			return invalid()
		}
	case "top_k":
		if number != math.Trunc(number) || number < 0 {
			return invalid()
		}
	case "repeat_last_n":
		if number != math.Trunc(number) || number < -1 {
			return invalid()
		}
	case "seed":
		if number != math.Trunc(number) {
			return invalid()
		}
	}

	return nil
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
	body := map[string]any{"model": c.Model, "messages": messages, "stream": true}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if len(format) > 0 {
		body["format"] = format
	}
	options := map[string]any{}

	for k, v := range c.Parameters {
		if k == "think" || k == "keep_alive" {
			body[k] = v
		} else {
			options[k] = v
		}
	}

	body["options"] = options
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
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/api/chat", bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if key, ok := c.Auth["key"]; ok {
			secret, err := r.credential(req.Plan.Profile, key)
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("Authorization", "Bearer "+secret)
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
			return nil, fmt.Errorf("model returned HTTP %d", res.StatusCode)
		}
		result, first, err := readChatStream(ctx, res.Body, started, req.observation, op.ID, step+1)
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
