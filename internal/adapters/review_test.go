package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-bill/knotra/internal/contract"
)

func TestMCPRejectsSchemaDriftBeforeExternalCall(t *testing.T) {
	for _, field := range []string{"input", "output", "removed"} {
		t.Run(field, func(t *testing.T) {
			var calls atomic.Int32
			server := mcp.NewServer(&mcp.Implementation{Name: "drift", Version: "1"}, nil)
			original := map[string]any{
				"type":       "object",
				"properties": map[string]any{"value": map[string]any{"type": "integer"}},
				"required":   []string{"value"},
			}
			handler := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return &mcp.CallToolResult{StructuredContent: map[string]any{"value": 1}}, nil
			}
			server.AddTool(&mcp.Tool{Name: "work", InputSchema: original, OutputSchema: original}, handler)
			httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
			defer httpServer.Close()
			req := testRequest("")
			req.Plan.Pipelines[req.Pipeline].Spec.Models = nil
			req.Plan.Pipelines[req.Pipeline].Spec.Sandboxes = nil
			req.Plan.Profile.Spec.MCP = map[string]contract.MCPConnection{"connection": {
				Transport:    "streamable_http",
				URL:          httpServer.URL,
				AllowedTools: []string{"work"},
				ToolPolicies: map[string]contract.ToolPolicy{"work": {Effect: "write"}},
			}}
			req.Plan.Pipelines[req.Pipeline].Spec.MCP = map[string]contract.MCPResource{"tools": {Connection: "connection"}}
			req.Node = contract.Node{
				Type:    "tool",
				Tool:    &contract.ToolNode{Server: "tools", Name: "work"},
				Outputs: map[string]contract.Port{"result": {Schema: json.RawMessage(`{"type":"object"}`)}},
			}
			req.ToolArguments = json.RawMessage(`{"value":1}`)
			runner := &Runner{Hooks: newHooks()}
			defer runner.Close()
			if err := runner.Prepare(context.Background(), req.Plan); err != nil {
				t.Fatal(err)
			}
			server.RemoveTools("work")
			changed := map[string]any{
				"type":       "object",
				"properties": map[string]any{"value": map[string]any{"type": "integer"}, "extra": map[string]any{"type": "string"}},
				"required":   []string{"value"},
			}

			switch field {
			case "input":
				server.AddTool(&mcp.Tool{Name: "work", InputSchema: changed, OutputSchema: original}, handler)
			case "output":
				server.AddTool(&mcp.Tool{Name: "work", InputSchema: original, OutputSchema: changed}, handler)
			}

			if _, err := runner.Execute(context.Background(), req); err == nil {
				t.Fatal("changed MCP schema was accepted")
			}
			if calls.Load() != 0 {
				t.Fatalf("tool with changed schema was invoked %d times", calls.Load())
			}
		})
	}
}

func TestMCPBoundedBodyRejectsOversizeWithoutTruncatingSuccess(t *testing.T) {
	for _, size := range []int{7, 8, 9} {
		reader := &boundedResponse{
			ReadCloser: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), size))),
			remaining:  8,
		}
		data, err := io.ReadAll(reader)
		if size <= 8 {
			if err != nil || len(data) != size {
				t.Fatalf("size %d: data=%d err=%v", size, len(data), err)
			}
		} else if err == nil || len(data) > 8 {
			t.Fatal("oversize body was silently truncated or buffered")
		}
	}
}

func TestChangedModelDoesNotGenerateOrRetry(t *testing.T) {
	var chats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/tags" {
			fmt.Fprint(w, `{"models":[{"name":"qwen3.5:9b","digest":"changed"}]}`)
			return
		}
		chats.Add(1)
		http.Error(w, "unexpected generation", http.StatusInternalServerError)
	}))
	defer server.Close()
	req := testRequest(server.URL)
	req.Plan.ModelDigests = map[string]string{"local/qwen3.5:9b": "admitted"}
	runner := &Runner{Hooks: newHooks()}
	_, err := runner.Execute(context.Background(), req)
	var failed *Failure
	if !errors.As(err, &failed) || failed.Code != "RESOURCE_CHANGED" || failed.Retryable || failed.Unknown || chats.Load() != 0 {
		t.Fatalf("changed model was used or retried: failure=%v chats=%d", err, chats.Load())
	}
}

func TestExecuteRejectsIncompatibleAdapterBeforeAnyOperation(t *testing.T) {
	req := testRequest("http://localhost")
	req.Plan.Runtime.AdapterVersion = "future-adapter"
	hooks := newHooks()
	runner := &Runner{Hooks: hooks}
	_, err := runner.Execute(context.Background(), req)
	var failed *Failure
	if !errors.As(err, &failed) || failed.Code != "ADAPTER_VERSION_UNSUPPORTED" || failed.Retryable || failed.Unknown || len(hooks.ops) != 0 || len(hooks.calls) != 0 {
		t.Fatalf("incompatible adapter began execution: %v", err)
	}
}

func TestDockerAgentStopsAtPermissionViolation(t *testing.T) {
	runner := integrationRunner(t)
	defer runner.Close()
	var turns atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		turns.Add(1)
		fmt.Fprint(
			w,
			`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"knotra_process_exec","arguments":{"command":["true"]}}}]},"done":true}`,
		)
	}))
	defer server.Close()
	req := testRequest(server.URL)
	req.Node.Type, req.Node.Sandbox, req.Node.LLM = "agent", "box", nil
	req.Node.Agent = &contract.AgentNode{
		Model:    "model",
		Prompt:   contract.TextSource{Text: "Finish immediately."},
		MaxSteps: 3,
	}
	req.Node.Tools = &contract.ToolGrants{Sandbox: []string{"files.read"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := runner.Execute(ctx, req)
	var failed *Failure
	if !errors.As(err, &failed) || failed.Code != "PERMISSION_DENIED" || failed.Retryable || turns.Load() != 1 {
		t.Fatalf("agent continued after permission violation: failure=%v turns=%d", err, turns.Load())
	}
}

type failingCommitHooks struct {
	*memoryHooks
	id string
}

func (h failingCommitHooks) CompleteOperation(ctx context.Context, id string, response json.RawMessage) error {
	if id == h.id {
		return errors.New("injected journal outage after external success")
	}
	return h.memoryHooks.CompleteOperation(ctx, id, response)
}

func TestOperationCommitFailurePreservesUnknownAndNeverReissuesWrite(t *testing.T) {
	hooks := failingCommitHooks{memoryHooks: newHooks(), id: "write"}
	runner := &Runner{Hooks: hooks}
	operation := Operation{ID: "write", Kind: "tool", Effect: "write"}
	calls := 0

	for range 2 {
		_, err := runner.operation(context.Background(), operation, func() (json.RawMessage, error) {
			calls++
			return json.RawMessage(`{"ok":true}`), nil
		})
		var failed *Failure
		if !errors.As(err, &failed) || !failed.Unknown || failed.Retryable || failed.OperationID != operation.ID {
			t.Fatalf("journal failure lost unknown outcome: %v", err)
		}
	}

	if calls != 1 || hooks.calls["tool"] != 1 {
		t.Fatalf("write was reissued: calls=%d budgets=%v", calls, hooks.calls)
	}
}

func TestDockerAttemptCommitFailureNeverRestartsCompletedWork(t *testing.T) {
	for _, kind := range []string{"agent", "code"} {
		t.Run(kind, func(t *testing.T) {
			runner := integrationRunner(t)
			defer runner.Close()
			var modelCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				modelCalls.Add(1)
				fmt.Fprint(
					w,
					`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"knotra_finish","arguments":{"answer":42}}}]},"done":true}`,
				)
			}))
			defer server.Close()
			req := testRequest(server.URL)
			req.Node.Type, req.Node.Sandbox, req.Node.LLM = kind, "box", nil
			req.Node.Agent = &contract.AgentNode{
				Model:    "model",
				Prompt:   contract.TextSource{Text: "Finish immediately."},
				MaxSteps: 1,
			}
			operationName, counter := "agent-attempt", "model"
			if kind == "code" {
				req.Node.Agent = nil
				req.Node.Code = &contract.CodeNode{Command: []string{
					"python",
					"-c",
					`import json,os;json.dump({"answer":42},open(os.environ["KNOTRA_OUTPUT_JSON"],"w"))`,
				}}
				operationName, counter = "code", "tool"
			}
			hooks := failingCommitHooks{memoryHooks: newHooks(), id: operationID(req, operationName, true)}
			runner.Hooks = hooks
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			for range 2 {
				_, err := runner.Execute(ctx, req)
				var failed *Failure
				if !errors.As(err, &failed) || !failed.Unknown || failed.Retryable || failed.OperationID != hooks.id {
					t.Fatalf("attempt commit failure lost unknown outcome: %v", err)
				}
			}

			if hooks.calls[counter] != 1 || (kind == "agent" && modelCalls.Load() != 1) {
				t.Fatalf("completed work restarted: budgets=%v modelCalls=%d", hooks.calls, modelCalls.Load())
			}
		})
	}
}
