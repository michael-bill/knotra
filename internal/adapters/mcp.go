package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpSession struct {
	mu      sync.Mutex
	session *mcp.ClientSession
	sandbox *sandbox
	cancel  context.CancelFunc
}

func (s *mcpSession) close() error {
	if s.cancel != nil {
		defer s.cancel()
	}
	err := s.session.Close()
	if s.sandbox != nil {
		s.sandbox.close()
	}
	return err
}

type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

const maxMCPResponseBytes = 32 << 20

type boundedResponse struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedResponse) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("MCP response exceeds byte limit")
		}
		return 0, err
	}
	if int64(len(data)) > b.remaining {
		data = data[:b.remaining]
	}
	n, err := b.ReadCloser.Read(data)
	b.remaining -= int64(n)
	return n, err
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for k, v := range t.headers {
		clone.Header[k] = append([]string(nil), v...)
	}
	response, err := t.base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxMCPResponseBytes {
		_ = response.Body.Close()
		return nil, fmt.Errorf("MCP response exceeds byte limit")
	}
	response.Body = &boundedResponse{ReadCloser: response.Body, remaining: maxMCPResponseBytes}
	return response, nil
}

func (r *Runner) connectMCP(ctx context.Context, req Request, connection contract.MCPConnection) (*mcpSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "knotra", Version: "0.1.0"}, nil)
	var transport mcp.Transport
	s := &mcpSession{}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	connected := false
	defer func() {
		if !connected {
			cancel()
		}
	}()
	switch connection.Transport {
	case "streamable_http":
		headers := http.Header{}
		for k, c := range connection.Headers {
			value, err := r.credential(req.Plan.Profile, c)
			if err != nil {
				return nil, err
			}
			headers.Set(k, value)
		}
		base := http.DefaultTransport
		if r.HTTPClient != nil && r.HTTPClient.Transport != nil {
			base = r.HTTPClient.Transport
		}
		httpClient := &http.Client{Transport: headerTransport{base: base, headers: headers}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		transport = &mcp.StreamableClientTransport{Endpoint: connection.URL, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: maxMCPResponseBytes}
	case "stdio":
		profile, ok := req.Plan.Profile.Spec.Sandboxes[connection.Sandbox]
		if !ok {
			return nil, fmt.Errorf("MCP sandbox missing")
		}
		env := []string{}
		for k, c := range connection.Env {
			if c.SecretRef != "" && !slices.Contains(profile.AllowedSecrets, c.SecretRef) {
				return nil, fmt.Errorf("MCP environment secret is not allowed")
			}
			value, err := r.credential(req.Plan.Profile, c)
			if err != nil {
				return nil, err
			}
			env = append(env, k+"="+value)
		}
		isolated := req
		isolated.service = true
		isolated.Inputs = contract.Values{}
		var err error
		s.sandbox, err = r.newSandbox(ctx, isolated, profile, env)
		if err != nil {
			return nil, err
		}
		argv := []string{}
		if r.DockerHost != "" {
			argv = append(argv, "--host", r.DockerHost)
		}
		argv = append(argv, "exec", "-i", "--user", "65532:65532", "--workdir", "/workspace", s.sandbox.id)
		argv = append(argv, connection.Command...)
		transport = &mcp.CommandTransport{Command: exec.CommandContext(lifetime, "docker", argv...)}
	default:
		return nil, fmt.Errorf("unsupported MCP transport %q", connection.Transport)
	}
	var err error
	s.session, err = client.Connect(ctx, transport, nil)
	if err != nil {
		if s.sandbox != nil {
			s.sandbox.close()
		}
		return nil, fmt.Errorf("MCP connection failed: %w", err)
	}
	connected = true
	return s, nil
}

func (r *Runner) session(ctx context.Context, req Request, alias string) (*mcpSession, func(), error) {
	resource, ok := pipeline(req).Spec.MCP[alias]
	if !ok {
		return nil, nil, fmt.Errorf("unknown MCP alias %q", alias)
	}
	connection := req.Plan.Profile.Spec.MCP[resource.Connection]
	if resource.Session != "run" {
		s, err := r.connectMCP(ctx, req, connection)
		if err != nil {
			return nil, nil, err
		}
		return s, func() { _ = s.close() }, nil
	}
	if !connection.AllowRunSession {
		return nil, nil, fmt.Errorf("run MCP session is not permitted")
	}
	key := req.RunID + "/" + req.ScopeID + "/" + alias
	cache := r.cache()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if s := cache.sessions[key]; s != nil {
		return s, func() {}, nil
	}
	op := Operation{ID: operationID(Request{RunID: req.RunID, InstanceID: req.ScopeID}, "session/"+alias, false), Effect: "unknown"}
	state, err := r.Hooks.BeginOperation(ctx, op)
	if err != nil {
		return nil, nil, err
	}
	if state.Started {
		return nil, nil, &Failure{Code: "OUTCOME_UNKNOWN", Message: "run-scoped MCP session was lost; state cannot be reconstructed", OperationID: op.ID, Unknown: true}
	}
	initialization, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := r.connectMCP(initialization, req, connection)
	if err != nil {
		return nil, nil, err
	}
	cache.sessions[key] = s
	return s, func() {}, nil
}

func (r *Runner) callMCP(ctx context.Context, req Request, s *mcpSession, alias, name string, args json.RawMessage, index string) (json.RawMessage, error) {
	resource := pipeline(req).Spec.MCP[alias]
	connection := req.Plan.Profile.Spec.MCP[resource.Connection]
	if !slices.Contains(connection.AllowedTools, name) {
		return nil, fmt.Errorf("MCP tool is not allowed")
	}
	snapshot, ok := req.Plan.MCPTools[resource.Connection][name]
	if !ok {
		return nil, fmt.Errorf("MCP tool was not snapshotted at admission")
	}
	policy := connection.ToolPolicies[name]
	if policy.Effect == "" {
		policy.Effect = "unknown"
	}
	logicalID := operationID(req, "mcp/"+alias+"/"+name+"/"+index, req.Node.Type == "agent")
	id := operationID(req, "mcp/"+alias+"/"+name+"/"+index, true)
	decoded, err := contract.DecodeJSON(args)
	if err != nil {
		return nil, err
	}
	obj, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("MCP arguments must be an object")
	}
	if policy.IdempotencyArgument != "" {
		if err = insertIdempotency(obj, policy.IdempotencyArgument, logicalID); err != nil {
			return nil, err
		}
	}
	complete, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	if err = contract.ValidateValue(contract.Port{Schema: snapshot.InputSchema}, contract.Value{JSON: complete}); err != nil {
		return nil, fmt.Errorf("MCP input schema: %w", err)
	}
	op := Operation{ID: id, Kind: "tool", Effect: policy.Effect}
	if policy.IdempotencyArgument != "" {
		op.IdempotencyKey = logicalID
	}
	response, err := r.operation(ctx, op, func() (json.RawMessage, error) {
		if s == nil {
			var close func()
			var err error
			s, close, err = r.session(ctx, req, alias)
			if err != nil {
				var f *Failure
				if errors.As(err, &f) {
					return nil, err
				}
				return nil, &Failure{Code: "TOOL_CONNECT_FAILED", Message: err.Error(), Retryable: true}
			}
			defer close()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := verifyLiveTool(ctx, s.session, name, snapshot); err != nil {
			return nil, failure("MCP_SCHEMA_CHANGED", err)
		}
		result, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: obj})
		if err != nil {
			if resource.Session == "run" {
				return nil, &Failure{Code: "OUTCOME_UNKNOWN", Message: "run-scoped MCP call failed; session state is not known", Unknown: true}
			}
			return nil, err
		}
		return json.Marshal(result)
	})
	if err != nil {
		return nil, err
	}
	if len(snapshot.OutputSchema) > 0 && string(snapshot.OutputSchema) != "null" {
		var result struct {
			Structured json.RawMessage `json:"structuredContent"`
			IsError    bool            `json:"isError"`
		}
		if err = json.Unmarshal(response, &result); err != nil {
			return nil, err
		}
		if !result.IsError {
			if len(result.Structured) == 0 {
				return nil, failure("OUTPUT_INVALID", fmt.Errorf("MCP outputSchema requires structuredContent"))
			}
			if err = contract.ValidateValue(contract.Port{Schema: snapshot.OutputSchema}, contract.Value{JSON: result.Structured}); err != nil {
				return nil, failure("OUTPUT_INVALID", err)
			}
		}
	}
	return response, nil
}

// Admission pins schemas, but MCP has no atomic compare-and-call operation.
// Recheck immediately before dispatch and keep calls in a shared session
// serialized. Replaying a journalled response never reaches this discovery.
func verifyLiveTool(ctx context.Context, session *mcp.ClientSession, name string, snapshot contract.ToolSnapshot) error {
	count, size := 0, 0
	seen := map[string]bool{}
	found := false
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("cannot verify live MCP tool: %w", err)
		}
		count++
		if seen[tool.Name] {
			return fmt.Errorf("MCP catalog contains duplicate tool %q", tool.Name)
		}
		seen[tool.Name] = true
		input, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return err
		}
		output, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			return err
		}
		size += len(input) + len(output) + len(tool.Description) + len(tool.Name)
		if count > 4096 || size > 8<<20 {
			return fmt.Errorf("MCP catalog exceeds discovery limits")
		}
		if tool.Name != name {
			continue
		}
		found = true
		if !sameSchema(input, snapshot.InputSchema) || !sameSchema(output, snapshot.OutputSchema) {
			return fmt.Errorf("MCP tool %q schema differs from admission snapshot", name)
		}
	}
	if !found {
		return fmt.Errorf("MCP tool %q is no longer available", name)
	}
	return nil
}

func sameSchema(left, right json.RawMessage) bool {
	var a, b any
	if len(left) > 0 {
		if err := json.Unmarshal(left, &a); err != nil {
			return false
		}
	}
	if len(right) > 0 {
		if err := json.Unmarshal(right, &b); err != nil {
			return false
		}
	}
	return reflect.DeepEqual(a, b)
}

func insertIdempotency(obj map[string]any, pointer, value string) error {
	if pointer == "" || pointer[0] != '/' {
		return fmt.Errorf("invalid idempotency pointer")
	}
	parts := strings.Split(pointer[1:], "/")
	current := obj
	for i, raw := range parts {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		if i == len(parts)-1 {
			if _, exists := current[part]; exists {
				return fmt.Errorf("idempotency field is reserved")
			}
			current[part] = value
			return nil
		}
		child, exists := current[part]
		if !exists {
			next := map[string]any{}
			current[part] = next
			current = next
			continue
		}
		next, ok := child.(map[string]any)
		if !ok {
			return fmt.Errorf("idempotency parent must be an object")
		}
		current = next
	}
	return nil
}

func (r *Runner) tool(ctx context.Context, req Request) (contract.Values, error) {
	n := req.Node.Tool
	raw, err := r.callMCP(ctx, req, nil, n.Server, n.Name, req.ToolArguments, "direct")
	if err != nil {
		return nil, err
	}
	var result struct {
		Structured json.RawMessage `json:"structuredContent"`
		Content    json.RawMessage `json:"content"`
		IsError    bool            `json:"isError"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.IsError {
		return nil, failure("TOOL_FAILED", fmt.Errorf("MCP tool returned isError"))
	}
	value := result.Structured
	if n.Response == "content" {
		value = result.Content
	}
	if value == nil {
		return nil, failure("OUTPUT_INVALID", fmt.Errorf("MCP response lacks selected content"))
	}
	port := req.Node.Outputs["result"]
	v := contract.Value{JSON: value}
	if err = contract.ValidateValue(port, v); err != nil {
		return nil, failure("OUTPUT_INVALID", err)
	}
	return contract.Values{"result": v}, nil
}

// Prepare snapshots remote capabilities and immutable image IDs before a run is
// accepted. It performs discovery only; tools and user commands are never called.
func (r *Runner) Prepare(ctx context.Context, plan *contract.Plan) error {
	if plan.MCPTools == nil {
		plan.MCPTools = map[string]map[string]contract.ToolSnapshot{}
	}
	if err := r.prepareSandboxes(ctx, plan); err != nil {
		return err
	}
	for path, p := range plan.Pipelines {
		req := Request{Plan: plan, Pipeline: path, Node: contract.Node{Inputs: map[string]contract.Port{}}, Inputs: contract.Values{}}
		if err := r.prepareModels(ctx, req); err != nil {
			return err
		}
		for _, resource := range p.Spec.MCP {
			if _, ok := plan.MCPTools[resource.Connection]; ok {
				continue
			}
			connection := plan.Profile.Spec.MCP[resource.Connection]
			s, err := r.connectMCP(ctx, req, connection)
			if err != nil {
				return err
			}
			snapshots := map[string]contract.ToolSnapshot{}
			seen := map[string]bool{}
			totalBytes := 0
			for tool, err := range s.session.Tools(ctx, nil) {
				if err != nil {
					_ = s.close()
					return err
				}
				if seen[tool.Name] {
					_ = s.close()
					return fmt.Errorf("MCP catalog contains duplicate tool %q", tool.Name)
				}
				seen[tool.Name] = true
				if len(seen) > 4096 {
					_ = s.close()
					return fmt.Errorf("MCP catalog exceeds discovery limits")
				}
				input, e := json.Marshal(tool.InputSchema)
				if e != nil {
					_ = s.close()
					return e
				}
				output, e := json.Marshal(tool.OutputSchema)
				if e != nil {
					_ = s.close()
					return e
				}
				totalBytes += len(input) + len(output) + len(tool.Description) + len(tool.Name)
				if len(input) > 1<<20 || len(output) > 1<<20 || totalBytes > 8<<20 {
					_ = s.close()
					return fmt.Errorf("MCP discovery exceeds capability snapshot limits")
				}
				if !slices.Contains(connection.AllowedTools, tool.Name) {
					continue
				}
				if len(snapshots) >= 1024 {
					_ = s.close()
					return fmt.Errorf("MCP discovery exceeds capability snapshot limits")
				}
				if policy := connection.ToolPolicies[tool.Name]; policy.IdempotencyArgument != "" {
					if _, e := projectIdempotency(input, policy.IdempotencyArgument); e != nil {
						_ = s.close()
						return e
					}
				}
				snapshots[tool.Name] = contract.ToolSnapshot{InputSchema: input, OutputSchema: output, Description: tool.Description}
			}
			_ = s.close()
			for _, name := range connection.AllowedTools {
				if _, ok := snapshots[name]; !ok {
					return fmt.Errorf("allowed MCP tool %q not discovered", name)
				}
			}
			plan.MCPTools[resource.Connection] = snapshots
		}
	}
	return nil
}
func urlPath(s string) string { return strings.ReplaceAll(s, "/", "%2F") }
