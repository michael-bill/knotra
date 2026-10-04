package adapters

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/michael-bill/knotra/internal/contract"
)

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

func ollamaRequest(c contract.ModelConnection, messages []message, tools []functionTool, format json.RawMessage) map[string]any {
	body := map[string]any{"model": c.Model, "messages": messages, "stream": true}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if len(format) > 0 {
		body["format"] = format
	}
	options := map[string]any{}
	for name, value := range c.Parameters {
		if name == "think" || name == "keep_alive" {
			body[name] = value
		} else {
			options[name] = value
		}
	}
	body["options"] = options
	return body
}
