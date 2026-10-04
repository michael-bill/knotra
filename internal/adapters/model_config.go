package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

const structuredOutputTool = "knotra_output"

func modelConfig(req Request, alias string) (contract.ModelConnection, error) {
	resource, ok := pipeline(req).Spec.Models[alias]
	if !ok {
		return contract.ModelConnection{}, fmt.Errorf("unknown model alias %q", alias)
	}
	c, ok := req.Plan.Profile.Spec.Models[resource.Connection]
	if !ok {
		return c, fmt.Errorf("model connection missing")
	}
	if resource.Model != "" {
		c.Model = resource.Model
	}
	params := map[string]any{}
	for name, value := range c.Parameters {
		params[name] = value
	}
	for name, value := range resource.Parameters {
		params[name] = value
	}
	c.Parameters = params

	defaults := map[string]string{
		"ollama":    "http://127.0.0.1:11434",
		"openai":    "https://api.openai.com/v1",
		"anthropic": "https://api.anthropic.com/v1",
	}
	base, ok := defaults[c.Provider]
	if !ok {
		return c, fmt.Errorf("unsupported model adapter %q", c.Provider)
	}
	if c.Model == "" || (c.Provider != "ollama" && strings.ContainsAny(c.Model, "/?#")) {
		return c, fmt.Errorf("invalid %s model identifier", c.Provider)
	}
	for name := range c.Auth {
		if name != "key" {
			return c, fmt.Errorf("unsupported %s auth field %q", c.Provider, name)
		}
	}
	if c.Provider != "ollama" {
		if _, ok := c.Auth["key"]; !ok {
			return c, fmt.Errorf("%s requires auth.key", c.Provider)
		}
	}
	for name, value := range params {
		if c.Provider == "ollama" {
			if !ollamaOptions[name] && name != "think" && name != "keep_alive" {
				return c, fmt.Errorf("unsupported Ollama parameter %q", name)
			}
			if err := validateOllamaOption(name, value); err != nil {
				return c, err
			}
		} else if err := validateCloudOption(c.Provider, name, value); err != nil {
			return c, err
		}
	}
	if c.Provider == "anthropic" {
		_, temperature := params["temperature"]
		_, topP := params["top_p"]
		if temperature && topP {
			return c, fmt.Errorf("Anthropic accepts either temperature or top_p, not both")
		}
	}
	if c.BaseURL == "" {
		c.BaseURL = base
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return c, fmt.Errorf("invalid %s baseUrl", c.Provider)
	}
	return c, nil
}

func validateCloudOption(provider, name string, value any) error {
	invalid := func() error { return fmt.Errorf("invalid %s parameter %q", provider, name) }
	if provider == "openai" && name == "reasoning_effort" {
		s, ok := value.(string)
		if ok {
			for _, allowed := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
				if s == allowed {
					return nil
				}
			}
		}
		return invalid()
	}
	if provider == "anthropic" && name == "stop_sequences" {
		data, _ := json.Marshal(value)
		var stops []string
		if json.Unmarshal(data, &stops) != nil || len(stops) == 0 {
			return invalid()
		}
		for _, stop := range stops {
			if stop == "" {
				return invalid()
			}
		}
		return nil
	}
	maximum := "max_output_tokens"
	if provider == "anthropic" {
		maximum = "max_tokens"
	}
	if name != "temperature" && name != "top_p" && name != maximum && !(provider == "anthropic" && name == "top_k") {
		return fmt.Errorf("unsupported %s parameter %q", provider, name)
	}
	data, err := json.Marshal(value)
	var number float64
	if err != nil || json.Unmarshal(data, &number) != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return invalid()
	}
	switch name {
	case "temperature":
		upper := 2.0
		if provider == "anthropic" {
			upper = 1
		}
		if number < 0 || number > upper {
			return invalid()
		}
	case "top_p":
		if number < 0 || number > 1 {
			return invalid()
		}
	case "top_k":
		if number < 0 || number != math.Trunc(number) || number > 1048576 {
			return invalid()
		}
	default:
		if number < 1 || number != math.Trunc(number) || number > 1048576 {
			return invalid()
		}
	}
	return nil
}

func (r *Runner) modelAuth(profile contract.Profile, c contract.ModelConnection, req *http.Request) error {
	if c.Provider == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	key, ok := c.Auth["key"]
	if !ok {
		return nil
	}
	secret, err := r.credential(profile, key)
	if err != nil {
		return err
	}
	if secret == "" {
		return fmt.Errorf("model credential is empty")
	}
	if c.Provider == "anthropic" {
		req.Header.Set("x-api-key", secret)
	} else {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	return nil
}

func modelHTTPFailure(status int) error {
	code := "MODEL_REQUEST_REJECTED"
	retryable := status == 408 || status == 429 || status >= 500
	switch status {
	case 401, 403:
		code = "MODEL_AUTH_FAILED"
	case 429:
		code = "MODEL_RATE_LIMITED"
	}
	// Error bodies are untrusted and may echo credentials or private prompts.
	return &Failure{Code: code, Message: fmt.Sprintf("model returned HTTP %d", status), Retryable: retryable}
}

func modelRequest(c contract.ModelConnection, messages []message, tools []functionTool, format json.RawMessage) (string, any, error) {
	if c.Provider == "ollama" {
		return "/api/chat", ollamaRequest(c, messages, tools, format), nil
	}
	if len(format) > 0 {
		tools = []functionTool{toolDefinition(structuredOutputTool, "Return the declared JSON outputs using this tool.", format)}
	}
	if c.Provider == "openai" {
		body, err := openAIRequest(c, messages, tools, len(format) > 0)
		return "/responses", body, err
	}
	body, err := anthropicRequest(c, messages, tools, len(format) > 0)
	return "/messages", body, err
}

func readModelResponse(ctx context.Context, provider string, res *http.Response, started time.Time, observation *executionObservation, id string, step int) (chatResponse, time.Duration, error) {
	if provider == "ollama" {
		return readChatStream(ctx, res.Body, started, observation, id, step)
	}
	if provider == "openai" {
		return readOpenAIResponse(ctx, res, started, observation, id, step)
	}
	return readAnthropicResponse(ctx, res, started, observation, id, step)
}
