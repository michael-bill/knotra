package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type memoryHooks struct {
	mu    sync.Mutex
	ops   map[string]OperationState
	calls map[string]int
	files map[string][]byte
}

func newHooks() *memoryHooks {
	return &memoryHooks{ops: map[string]OperationState{}, calls: map[string]int{}, files: map[string][]byte{}}
}
func (h *memoryHooks) Reserve(_ context.Context, k string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls[k]++
	return nil
}
func (h *memoryHooks) BeginOperation(_ context.Context, o Operation) (OperationState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if state, ok := h.ops[o.ID]; ok {
		return state, nil
	}
	h.ops[o.ID] = OperationState{Started: true}
	return OperationState{}, nil
}
func (h *memoryHooks) CompleteOperation(_ context.Context, id string, data json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ops[id] = OperationState{Started: true, Completed: true, Response: append(json.RawMessage(nil), data...)}
	return nil
}
func (h *memoryHooks) PutArtifact(_ context.Context, name, media string, b []byte) (contract.Artifact, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	digest := sha256.Sum256(b)
	id := hex.EncodeToString(digest[:])
	h.files[id] = append([]byte(nil), b...)
	return contract.Artifact{ID: id, Name: name, MediaType: media, Size: int64(len(b)), SHA256: id}, nil
}
func (h *memoryHooks) GetArtifact(_ context.Context, id string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.files[id]
	if !ok {
		return nil, errors.New("unknown artifact")
	}
	return append([]byte(nil), b...), nil
}

func testRequest(endpoint string) Request {
	p := &contract.Pipeline{Spec: contract.Spec{
		Models:    map[string]contract.Model{"model": {Connection: "local"}},
		Sandboxes: map[string]contract.SandboxResource{"box": {Profile: "local"}},
	}}
	profile := contract.Profile{Spec: contract.ProfileSpec{
		Models: map[string]contract.ModelConnection{"local": {Provider: "ollama", Model: "qwen3.5:9b", BaseURL: endpoint}},
		Sandboxes: map[string]contract.SandboxProfile{"local": {
			Image:        "python:3.13-alpine",
			Resources:    contract.Resources{CPU: 1, MemoryMiB: 256, DiskMiB: 64, Pids: 64},
			Network:      contract.Network{Mode: "none"},
			AllowedTools: []string{"process.exec", "files.read", "files.write"},
		}},
	}}
	return Request{
		RunID: "run", InstanceID: "node", Attempt: 1, Pipeline: "main.yaml", ScopeID: "root",
		Plan:   &contract.Plan{Root: "main.yaml", Pipelines: map[string]*contract.Pipeline{"main.yaml": p}, Profile: profile},
		Inputs: contract.Values{},
		Node:   contract.Node{Type: "llm", Outputs: map[string]contract.Port{"answer": {Schema: json.RawMessage(`{"type":"integer"}`)}}, LLM: &contract.LLMNode{Model: "model", Prompt: contract.TextSource{Text: "Return answer 42."}}},
	}
}

func TestLLMStrictOutputAndReplay(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["stream"]) != "false" || body["format"] == nil {
			t.Error("model request lacks deterministic protocol fields")
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"{\"answer\":42}"},"done":true,"done_reason":"stop"}`)
	}))
	defer server.Close()
	hooks := newHooks()
	runner := &Runner{Hooks: hooks}
	req := testRequest(server.URL)
	for i := 0; i < 2; i++ {
		values, err := runner.Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if string(values["answer"].JSON) != "42" {
			t.Fatalf("unexpected value: %s", values["answer"].JSON)
		}
	}
	if requests != 1 || hooks.calls["model"] != 1 {
		t.Fatal("durable replay repeated model request")
	}
}
func TestOutputRejectsMarkdownUnknownAndMissing(t *testing.T) {
	for _, data := range []string{"```json\n{\"answer\":42}\n```", `{"answer":42,"extra":1}`, `{}`, `{"answer":"42"}`, `{"answer":42}{}`} {
		t.Run(data, func(t *testing.T) {
			if _, err := jsonOutputs(testRequest("").Node.Outputs, []byte(data)); err == nil {
				t.Fatal("invalid output accepted")
			}
		})
	}
}
func TestJournalPreventsDuplicateEffects(t *testing.T) {
	hooks := newHooks()
	r := &Runner{Hooks: hooks}
	op := Operation{ID: "write", Kind: "tool", Effect: "write"}
	_, _ = hooks.BeginOperation(context.Background(), op)
	called := false
	_, err := r.operation(context.Background(), op, func() (json.RawMessage, error) { called = true; return nil, nil })
	var f *Failure
	if !errors.As(err, &f) || !f.Unknown || called {
		t.Fatal("started write was reissued")
	}
}
func TestIdempotencyProjection(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"meta":{"type":"object","properties":{"key":{"type":"string"},"name":{"type":"string"}},"required":["key","name"]}},"required":["meta"]}`)
	projected, err := projectIdempotency(schema, "/meta/key")
	if err != nil {
		t.Fatal(err)
	}
	if err = contract.ValidateValue(contract.Port{Schema: projected}, contract.Value{JSON: json.RawMessage(`{"meta":{"name":"a"}}`)}); err != nil {
		t.Fatal(err)
	}
	obj := map[string]any{"meta": map[string]any{"name": "a"}}
	if err = insertIdempotency(obj, "/meta/key", "stable"); err != nil {
		t.Fatal(err)
	}
	if err = insertIdempotency(obj, "/meta/key", "other"); err == nil {
		t.Fatal("reserved field accepted")
	}
}

func TestMCPDiscoveryCallAndReplay(t *testing.T) {
	calls := 0
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "sum", Description: "Sum two numbers"}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		A int `json:"a"`
		B int `json:"b"`
	}) (*mcp.CallToolResult, map[string]int, error) {
		calls++
		return nil, map[string]int{"sum": args.A + args.B}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	req := testRequest("")
	req.Plan.Profile.Spec.Sandboxes = nil
	req.Plan.Pipelines["main.yaml"].Spec.Sandboxes = nil
	req.Plan.Pipelines["main.yaml"].Spec.Models = nil
	req.Plan.Profile.Spec.MCP = map[string]contract.MCPConnection{"local": {Transport: "streamable_http", URL: httpServer.URL, AllowedTools: []string{"sum"}, ToolPolicies: map[string]contract.ToolPolicy{"sum": {Effect: "read"}}}}
	req.Plan.Pipelines["main.yaml"].Spec.MCP = map[string]contract.MCPResource{"tools": {Connection: "local"}}
	req.Node = contract.Node{Type: "tool", Tool: &contract.ToolNode{Server: "tools", Name: "sum"}, Outputs: map[string]contract.Port{"result": {Schema: json.RawMessage(`{"type":"object","properties":{"sum":{"const":7}},"required":["sum"]}`)}}}
	req.ToolArguments = json.RawMessage(`{"a":3,"b":4}`)
	r := &Runner{Hooks: newHooks()}
	if err := r.Prepare(context.Background(), req.Plan); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		values, err := r.Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(values["result"].JSON), "7") {
			t.Fatal("wrong MCP result")
		}
		httpServer.Close()
	}
	if calls != 1 {
		t.Fatal("MCP replay duplicated call")
	}
}

func TestRunSessionSharedAndLostStateIsExplicit(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	calls := 0
	mcp.AddTool(server, &mcp.Tool{Name: "read"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]int, error) {
		calls++
		return nil, map[string]int{"number": calls}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	req := testRequest("")
	req.Plan.Profile.Spec.Sandboxes = nil
	req.Plan.Pipelines["main.yaml"].Spec.Sandboxes = nil
	req.Plan.Pipelines["main.yaml"].Spec.Models = nil
	req.Plan.Profile.Spec.MCP = map[string]contract.MCPConnection{"local": {Transport: "streamable_http", URL: httpServer.URL, AllowedTools: []string{"read"}, AllowRunSession: true, ToolPolicies: map[string]contract.ToolPolicy{"read": {Effect: "read"}}}}
	req.Plan.Pipelines["main.yaml"].Spec.MCP = map[string]contract.MCPResource{"tools": {Connection: "local", Session: "run"}}
	req.Node = contract.Node{Type: "tool", Tool: &contract.ToolNode{Server: "tools", Name: "read"}, Outputs: map[string]contract.Port{"result": {Schema: json.RawMessage(`{"type":"object"}`)}}}
	req.ToolArguments = json.RawMessage(`{}`)
	hooks := newHooks()
	base := &Runner{}
	defer base.Close()
	runner := base.WithHooks(hooks)
	if err := runner.Prepare(context.Background(), req.Plan); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		req.InstanceID = fmt.Sprint(i)
		if _, err := base.WithHooks(hooks).Execute(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || len(base.cache().sessions) != 1 {
		t.Fatal("run session was not shared")
	}
	req.InstanceID = "after-crash"
	_, err := (&Runner{Hooks: hooks}).Execute(context.Background(), req)
	var failure *Failure
	if !errors.As(err, &failure) || !failure.Unknown || calls != 2 {
		t.Fatalf("lost session silently recovered: %v, calls=%d", err, calls)
	}
	base.ReleaseRun(req.RunID)
	if len(base.cache().sessions) != 0 {
		t.Fatal("run session leaked")
	}
}

func integrationRunner(t *testing.T) *Runner {
	t.Helper()
	helper := os.Getenv("KNOTRA_TEST_HELPER")
	if helper == "" {
		t.Skip("set KNOTRA_TEST_HELPER to run Docker integration tests")
	}
	helper, err := filepath.Abs(helper)
	if err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("KNOTRA_TEST_WORKDIR")
	if root == "" {
		root = "../../.knotra/test-work"
	}
	return &Runner{Hooks: newHooks(), HelperPath: helper, WorkDir: root, DockerHost: os.Getenv("DOCKER_HOST"), FirewallImage: os.Getenv("KNOTRA_TEST_FIREWALL_IMAGE")}
}
func TestDockerCodeAndArtifacts(t *testing.T) {
	r := integrationRunner(t)
	req := testRequest("")
	req.Node = contract.Node{Type: "code", Sandbox: "box", Code: &contract.CodeNode{Command: []string{"python", "-c", `import json,os;json.dump({"answer":42},open(os.environ["KNOTRA_OUTPUT_JSON"],"w"));open("report.txt","w").write("done")`}}, Outputs: map[string]contract.Port{"answer": {Schema: json.RawMessage(`{"const":42}`)}, "report": {Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}, Collect: &contract.Collect{Path: "report.txt", MediaType: "text/plain"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := 0; i < 2; i++ {
		values, err := r.Execute(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if len(values["report"].Artifacts) != 1 || string(values["answer"].JSON) != "42" {
			t.Fatalf("wrong outputs: %#v", values)
		}
	}
}
func TestDockerAgentFileFinish(t *testing.T) {
	r := integrationRunner(t)
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		turn++
		if turn == 1 {
			fmt.Fprint(w, `{"message":{"role":"assistant","tool_calls":[{"function":{"name":"knotra_files_write","arguments":{"path":"report.txt","content":"hello"}}}]},"done":true}`)
		} else {
			fmt.Fprint(w, `{"message":{"role":"assistant","tool_calls":[{"function":{"name":"knotra_finish","arguments":{"answer":42}}}]},"done":true}`)
		}
	}))
	defer server.Close()
	req := testRequest(server.URL)
	req.Node.Type = "agent"
	req.Node.LLM = nil
	req.Node.Sandbox = "box"
	req.Node.Agent = &contract.AgentNode{Model: "model", Prompt: contract.TextSource{Text: "Write report."}, MaxSteps: 3}
	req.Node.Tools = &contract.ToolGrants{Sandbox: []string{"files.write"}}
	req.Node.Outputs["report"] = contract.Port{Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}, Collect: &contract.Collect{Path: "report.txt", MediaType: "text/plain"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := 0; i < 2; i++ {
		if _, err := r.Execute(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if turn != 2 {
		t.Fatal("agent result was not replayed")
	}
}
