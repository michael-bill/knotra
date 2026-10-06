package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

// The pure v1 decisions are shared with the legacy Temporal backend. They use
// explicit values and timestamps and never perform external I/O.
func BindPorts(ports map[string]contract.Port, scope contract.Scope, skipMissing bool) (contract.Values, error) {
	values := contract.Values{}

	for _, name := range keys(ports) {
		port := ports[name]
		if port.Bind == nil {
			return nil, failure("PLAN_INVALID", "port binding missing: "+name)
		}
		value, present, err := contract.EvalBinding(*port.Bind, scope)
		if err != nil {
			return nil, failure("BINDING_INVALID", name+": "+err.Error())
		}
		if !present {
			if port.IsRequired() {
				if skipMissing {
					return nil, nil
				}
				return nil, failure("OUTPUT_UNAVAILABLE", "required output is missing: "+name)
			}
			continue
		}
		if err := contract.ValidateValue(port, value); err != nil {
			return nil, failure("VALUE_INVALID", name+": "+err.Error())
		}
		values[name] = value
	}

	return values, nil
}

func WithoutArtifactPaths(values contract.Values) contract.Values {
	result := make(contract.Values, len(values))

	for _, name := range keys(values) {
		value := values[name]
		if value.Artifacts != nil {
			value.Artifacts = append([]contract.Artifact(nil), value.Artifacts...)

			for index := range value.Artifacts {
				value.Artifacts[index].Path = ""
			}
		}
		result[name] = value
	}

	return result
}

func ExecutionPolicy(defaults, own contract.Execution) contract.Execution {
	result := defaults
	if own.Timeout != "" {
		result.Timeout = own.Timeout
	}
	if own.OnUnknownOutcome != "" {
		result.OnUnknownOutcome = own.OnUnknownOutcome
	}
	result.Retry = &contract.Retry{MaxAttempts: 1, Backoff: "1s"}
	if defaults.Retry != nil {
		if defaults.Retry.MaxAttempts > 0 {
			result.Retry.MaxAttempts = defaults.Retry.MaxAttempts
		}
		if defaults.Retry.Backoff != "" {
			result.Retry.Backoff = defaults.Retry.Backoff
		}
	}
	if own.Retry != nil {
		if own.Retry.MaxAttempts > 0 {
			result.Retry.MaxAttempts = own.Retry.MaxAttempts
		}
		if own.Retry.Backoff != "" {
			result.Retry.Backoff = own.Retry.Backoff
		}
	}
	if result.OnUnknownOutcome == "" {
		result.OnUnknownOutcome = "pause"
	}
	return result
}

func NodeDeadline(now, parent time.Time, node contract.Node) time.Time {
	duration := 30 * time.Minute

	switch node.Type {
	case "human":
		duration = 24 * time.Hour
	case "switch":
		duration = time.Minute
	case "loop", "foreach", "pipeline":
		duration = parent.Sub(now)
	}

	if node.Execution.Timeout != "" {
		if specified, err := contract.Duration(node.Execution.Timeout); err == nil {
			duration = specified
		}
	}
	deadline := now.Add(duration)
	if parent.Before(deadline) {
		return parent
	}
	return deadline
}

func RunSwitch(node contract.Node, args contract.Values) (contract.Values, error) {
	if node.Switch == nil {
		return nil, failure("PLAN_INVALID", "missing switch configuration")
	}
	route := node.Switch.Default

	for _, branch := range node.Switch.Cases {
		match, present, err := contract.EvalBool(branch.When, contract.Scope{Args: args})
		if err != nil {
			return nil, failure("CONDITION_INVALID", err.Error())
		}
		if !present {
			return nil, failure("CONDITION_INVALID", "switch condition is missing")
		}
		if match {
			route = branch.Name
			break
		}
	}

	return contract.Values{"route": jsonValue(route)}, nil
}

func BindWith(bindings map[string]contract.Binding, scope contract.Scope) (contract.Values, error) {
	values := contract.Values{}

	for _, name := range keys(bindings) {
		value, present, err := contract.EvalBinding(bindings[name], scope)
		if err != nil {
			return nil, failure("BINDING_INVALID", name+": "+err.Error())
		}
		if present {
			values[name] = value
		}
	}

	return values, nil
}

func Items(value contract.Value) ([]contract.Value, error) {
	if value.Collection {
		result := make([]contract.Value, len(value.Artifacts))

		for i, artifact := range value.Artifacts {
			result[i] = contract.Value{Artifacts: []contract.Artifact{artifact}}
		}

		return result, nil
	}
	if value.Artifacts != nil {
		return nil, failure("INPUT_INVALID", "foreach requires an artifact collection")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(value.JSON, &raw); err != nil || string(value.JSON) == "null" {
		return nil, failure("INPUT_INVALID", "foreach requires a JSON array")
	}
	result := make([]contract.Value, len(raw))

	for i, v := range raw {
		result[i] = contract.Value{JSON: v}
	}

	return result, nil
}

func LoopState(ports map[string]contract.Port, scope contract.Scope, initial bool) (contract.Values, error) {
	result := contract.Values{}

	for _, name := range keys(ports) {
		port := ports[name]
		binding := port.Next
		if initial {
			binding = port.Initial
		}
		if binding == nil {
			return nil, failure("PLAN_INVALID", "loop state binding absent: "+name)
		}
		value, present, err := contract.EvalBinding(*binding, scope)
		if err != nil {
			return nil, failure("BINDING_INVALID", err.Error())
		}
		if !present {
			return nil, failure("STATE_UNAVAILABLE", "loop state missing: "+name)
		}
		if err := contract.ValidateValue(port, value); err != nil {
			return nil, failure("VALUE_INVALID", name+": "+err.Error())
		}
		result[name] = value
	}

	return result, nil
}

func IntersectPermissions(parent, child *contract.Permissions) *contract.Permissions {
	if parent == nil {
		cloned := *child
		return &cloned
	}
	result := &contract.Permissions{
		Models:    intersect(parent.Models, child.Models),
		Sandboxes: intersect(parent.Sandboxes, child.Sandboxes),
		Secrets:   intersect(parent.Secrets, child.Secrets),
		MCP:       map[string][]string{},
	}

	for _, name := range keys(child.MCP) {
		result.MCP[name] = intersect(parent.MCP[name], child.MCP[name])
	}

	return result
}

func StableID(kind, address string) string {
	digest := sha256.Sum256([]byte(address))
	return kind + "_" + hex.EncodeToString(digest[:])
}

func RestrictLimits(parent, own contract.Limits) contract.Limits {
	result := contract.Limits{Timeout: parent.Timeout,
		MaxConcurrentNodes: minPositive(parent.MaxConcurrentNodes, own.MaxConcurrentNodes),
		MaxNodeInstances:   minPositive(parent.MaxNodeInstances, own.MaxNodeInstances),
		MaxModelCalls:      minPositive(parent.MaxModelCalls, own.MaxModelCalls),
		MaxToolCalls:       minPositive(parent.MaxToolCalls, own.MaxToolCalls)}
	if own.Timeout != "" {
		a, ea := contract.Duration(parent.Timeout)
		b, eb := contract.Duration(own.Timeout)
		if eb == nil && (ea != nil || b < a) {
			result.Timeout = own.Timeout
		}
	}
	return result
}

func intersect(a, b []string) []string {
	set := map[string]bool{}

	for _, value := range a {
		set[value] = true
	}

	result := []string{}

	for _, value := range b {
		if set[value] {
			result = append(result, value)
		}
	}

	return result
}

func minPositive(a, b int) int {
	if a == 0 {
		return b
	}
	if b == 0 || a < b {
		return a
	}
	return b
}

func keys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))

	for key := range values {
		result = append(result, key)
	}

	sort.Strings(result)
	return result
}

func jsonValue(value any) contract.Value {
	raw, _ := json.Marshal(value)
	return contract.Value{JSON: raw}
}

func failure(code, message string) *Failure { return &Failure{Code: code, Message: message} }
