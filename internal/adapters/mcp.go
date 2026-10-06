package adapters

import (
	"bytes"
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

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

type mcpSession struct {
	mu            sync.Mutex
	session       *mcp.ClientSession
	sandbox       *sandbox
	cancel        context.CancelFunc
	deleteMu      sync.Mutex
	deleteRequest *http.Request
	deleteErr     error
	httpClient    *http.Client
	resource      execution.ResourceRecord
	hooks         Hooks
}

func (s *mcpSession) close() error {
	if s.cancel != nil {
		defer s.cancel()
	}
	s.deleteMu.Lock()
	request, deletionErr := s.deleteRequest, s.deleteErr
	s.deleteMu.Unlock()
	err := s.session.Close()
	// The SDK closes locally once and caches its result. Retry only the same
	// remote DELETE; a failed cleanup must never initialize a replacement session.
	if request != nil && deletionErr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		response, retryErr := s.httpClient.Do(request.Clone(ctx))
		if response != nil {
			_ = response.Body.Close()
		}
		err = retryErr
	} else {
		s.deleteMu.Lock()
		if s.deleteRequest != nil {
			err = s.deleteErr
		}
		s.deleteMu.Unlock()
	}
	if s.sandbox != nil {
		err = errors.Join(err, s.sandbox.close())
	}
	if err == nil && s.resource.Ownership.WorkerID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err = s.hooks.CloseResource(ctx, s.resource.ID)
	}
	return err
}

type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
	session *mcpSession
}

type mcpCallContextKey struct{}

type mcpCallEvidence struct {
	files        *execution.OutcomeFiles
	resource     execution.ResourceRecord
	id           json.RawMessage
	admit        func() error
	admissionErr error
}

// The SDK emits id and method before params. Inspect only its bounded envelope
// prefix; tool arguments can be large and must never enter cleanup evidence.
func (call *mcpCallEvidence) beforeSend(request *http.Request) error {
	if request.Method != http.MethodPost || request.GetBody == nil {
		return errors.New("MCP call requires a replayable SDK envelope")
	}
	body, err := request.GetBody()
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(body, 4096))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errors.New("invalid MCP request envelope")
	}
	var id json.RawMessage
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return err
		}
		switch field {
		case "id":
			if err := decoder.Decode(&id); err != nil {
				return err
			}
		case "method":
			var method string
			if err := decoder.Decode(&method); err != nil {
				return err
			}
			if method != "tools/call" {
				return nil // SDK cancellation notifications share the caller's context.
			}
			if err := call.files.PutMCPCall(call.resource, id, false); err != nil {
				return err
			}
			call.id = id
			call.admissionErr = call.admit()
			return call.admissionErr
		case "jsonrpc":
			var version string
			if err := decoder.Decode(&version); err != nil || version != "2.0" {
				return errors.New("invalid MCP JSON-RPC version")
			}
		default:
			return errors.New("MCP SDK envelope lacks request identity before parameters")
		}
	}
	return errors.New("MCP request envelope has no method")
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
	if call, ok := req.Context().Value(mcpCallContextKey{}).(*mcpCallEvidence); ok {
		if err := call.beforeSend(req); err != nil {
			return nil, fmt.Errorf("cannot persist MCP call identity: %w", err)
		}
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()

	for k, v := range t.headers {
		clone.Header[k] = append([]string(nil), v...)
	}

	response, err := t.base.RoundTrip(clone)
	if req.Method == http.MethodDelete {
		// 404 means already gone; 405 leaves termination to the server under MCP.
		if err == nil && (response.StatusCode < 200 || response.StatusCode >= 300) && response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusMethodNotAllowed {
			_ = response.Body.Close()
			err = fmt.Errorf("MCP session cleanup returned HTTP %d", response.StatusCode)
			response = nil
		}
		if t.session != nil {
			t.session.deleteMu.Lock()
			t.session.deleteRequest = clone.Clone(context.Background())
			t.session.deleteErr = err
			t.session.deleteMu.Unlock()
		}
		return response, err
	}
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

func (r *Runner) mcpHTTPClient(profile contract.Profile, connection contract.MCPConnection, session *mcpSession) (*http.Client, error) {
	headers := http.Header{}
	for k, credential := range connection.Headers {
		value, err := r.credential(profile, credential)
		if err != nil {
			return nil, err
		}
		headers.Set(k, value)
	}
	base := http.DefaultTransport
	if r.HTTPClient != nil && r.HTTPClient.Transport != nil {
		base = r.HTTPClient.Transport
	}
	return &http.Client{
		Transport:     headerTransport{base: base, headers: headers, session: session},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// CleanupMCPSession cancels an unconfirmed call before deleting its session. Missing
// initialization evidence is not proof that no remote session was created.
func (r *Runner) CleanupMCPSession(ctx context.Context, resource execution.ResourceRecord, profile contract.Profile) error {
	if err := resource.Validate(); err != nil {
		return err
	}
	if resource.Kind != "mcp_http" || resource.State != "cleaning" || resource.EngineID != r.EngineID || resource.HostID != r.HostID {
		return fmt.Errorf("MCP cleanup does not match this runner and host")
	}
	if resource.MCP == nil {
		return fmt.Errorf("MCP initialization outcome is unknown; remote cleanup identity is unavailable")
	}
	if resource.MCP.SessionID == "" {
		return nil // Stateless transport has no remote session to delete.
	}
	connection, ok := profile.Spec.MCP[resource.MCP.Connection]
	if !ok || connection.Transport != "streamable_http" {
		return fmt.Errorf("MCP cleanup connection is absent from admitted profile")
	}
	client, err := r.mcpHTTPClient(profile, connection, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if r.ResourceEvidence != nil {
		id, err := r.ResourceEvidence.GetMCPCall(resource)
		if err != nil {
			return err
		}
		if len(id) != 0 {
			data, err := json.Marshal(struct {
				JSONRPC string              `json:"jsonrpc"`
				Method  string              `json:"method"`
				Params  mcp.CancelledParams `json:"params"`
			}{JSONRPC: "2.0", Method: "notifications/cancelled", Params: mcp.CancelledParams{RequestID: id}})
			if err != nil {
				return err
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.URL, bytes.NewReader(data))
			if err != nil {
				return err
			}
			request.Header.Set("Mcp-Session-Id", resource.MCP.SessionID)
			request.Header.Set("Mcp-Protocol-Version", resource.MCP.ProtocolVersion)
			request.Header.Set("Mcp-Method", "notifications/cancelled")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			response, err := client.Do(request)
			if err != nil {
				return err
			}
			_ = response.Body.Close()
			if response.StatusCode == http.StatusNotFound {
				return nil
			}
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return fmt.Errorf("MCP cancellation returned HTTP %d", response.StatusCode)
			}
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, connection.URL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Mcp-Session-Id", resource.MCP.SessionID)
	request.Header.Set("Mcp-Protocol-Version", resource.MCP.ProtocolVersion)
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	return err
}

func (r *Runner) connectMCP(ctx context.Context, req Request, name string, connection contract.MCPConnection) (*mcpSession, error) {
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
		httpClient, err := r.mcpHTTPClient(req.Plan.Profile, connection, s)
		if err != nil {
			return nil, err
		}
		s.httpClient = httpClient
		transport = &mcp.StreamableClientTransport{
			Endpoint:             connection.URL,
			HTTPClient:           httpClient,
			MaxRetries:           -1,
			DisableStandaloneSSE: true,
			MaxEventSize:         maxMCPResponseBytes,
		}
		if r.Hooks != nil {
			lifetime := "attempt"
			if req.service {
				lifetime = "run"
			}
			s.resource, err = r.Hooks.RegisterResource(ctx, uuid.NewString(), "mcp_http", lifetime)
			if err != nil {
				return nil, err
			}
			s.hooks = r.Hooks
			if s.resource.Ownership.WorkerID != "" && (s.resource.EngineID != r.EngineID || s.resource.HostID != r.HostID || r.ResourceEvidence == nil) {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				closeErr := s.hooks.CloseResource(cleanup, s.resource.ID)
				stop()
				return nil, errors.Join(fmt.Errorf("MCP runner requires matching engine/host and durable cleanup storage"), closeErr)
			}
		}
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
			_ = s.sandbox.close()
		}
		return nil, fmt.Errorf("MCP connection failed: %w", err)
	}
	if s.resource.Ownership.WorkerID != "" {
		s.resource.MCP = &execution.MCPSessionRecord{Connection: name, SessionID: s.session.ID(), ProtocolVersion: s.session.InitializeResult().ProtocolVersion}
		if err := r.ResourceEvidence.PutMCPSession(s.resource); err != nil {
			_ = s.close()
			return nil, fmt.Errorf("cannot save MCP cleanup evidence: %w", err)
		}
		evidence, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		err = s.hooks.RecordMCPSession(evidence, s.resource.ID, *s.resource.MCP)
		stop()
		if err != nil {
			_ = s.close()
			return nil, fmt.Errorf("cannot persist MCP cleanup identity: %w", err)
		}
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
	req.service = resource.Session == "run"
	if resource.Session != "run" {
		s, err := r.connectMCP(ctx, req, resource.Connection, connection)
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
	op := Operation{
		ID:     operationID(Request{RunID: req.RunID, InstanceID: req.ScopeID}, "session/"+alias, false),
		Effect: "unknown",
	}
	state, err := r.Hooks.BeginOperation(ctx, op)
	if err != nil {
		return nil, nil, err
	}
	if state.Started {
		return nil, nil, &Failure{
			Code:        "OUTCOME_UNKNOWN",
			Message:     "run-scoped MCP session was lost; state cannot be reconstructed",
			OperationID: op.ID,
			Unknown:     true,
		}
	}
	initialization, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := r.connectMCP(initialization, req, resource.Connection, connection)
	if err != nil {
		return nil, nil, err
	}
	cache.save(key, s)
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
	response, err := r.operation(ctx, op, func(admit func() error) (json.RawMessage, error) {
		if s == nil {
			var release func()
			var err error
			s, release, err = r.session(ctx, req, alias)
			if err != nil {
				var f *Failure
				if errors.As(err, &f) {
					return nil, err
				}
				return nil, &Failure{Code: "TOOL_CONNECT_FAILED", Message: err.Error(), Retryable: true}
			}
			defer release()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := verifyLiveTool(ctx, s.session, name, snapshot); err != nil {
			return nil, failure("MCP_SCHEMA_CHANGED", err)
		}
		var call *mcpCallEvidence
		callCtx := ctx
		if s.resource.Ownership.WorkerID != "" {
			call = &mcpCallEvidence{files: r.ResourceEvidence, resource: s.resource, admit: admit}
			callCtx = context.WithValue(ctx, mcpCallContextKey{}, call)
		} else if err := admit(); err != nil {
			return nil, err
		}
		result, err := s.session.CallTool(callCtx, &mcp.CallToolParams{Name: name, Arguments: obj})
		if err != nil {
			if call != nil && call.admissionErr != nil {
				return nil, call.admissionErr
			}
			if resource.Session == "run" {
				return nil, &Failure{
					Code:    "OUTCOME_UNKNOWN",
					Message: "run-scoped MCP call failed; session state is not known",
					Unknown: true,
				}
			}
			return nil, err
		}
		if call != nil && len(call.id) != 0 {
			// A failed completion marker leaves conservative cancellation evidence;
			// do not discard a confirmed tool response or cause a second execution.
			_ = call.files.PutMCPCall(call.resource, call.id, true)
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
		req := Request{
			Plan:     plan,
			Pipeline: path,
			Node:     contract.Node{Inputs: map[string]contract.Port{}},
			Inputs:   contract.Values{},
		}
		if err := r.prepareModels(ctx, req); err != nil {
			return err
		}

		for _, resource := range p.Spec.MCP {
			if _, ok := plan.MCPTools[resource.Connection]; ok {
				continue
			}
			connection := plan.Profile.Spec.MCP[resource.Connection]
			s, err := r.connectMCP(ctx, req, resource.Connection, connection)
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
