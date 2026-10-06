package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRiverAgentAndMCPReuseSessionAndReleaseAtCompletion(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" || os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL and KNOTRA_TEST_HELPER for real River agent/MCP acceptance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var turns, deletes atomic.Int32
	var sessionMu sync.Mutex
	var sessions []string
	type value struct {
		Value int `json:"value"`
	}
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "stats", Description: "Echo a measured value"}, func(_ context.Context, req *mcp.CallToolRequest, input value) (*mcp.CallToolResult, value, error) {
		sessionMu.Lock()
		sessions = append(sessions, req.Session.ID())
		sessionMu.Unlock()
		return nil, input, nil
	})
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	mcpRemote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		transport.ServeHTTP(w, r)
	}))
	defer mcpRemote.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"id":"fixture-model"}`)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
		}
		turn := turns.Add(1)
		name, args := "knotra_finish", `{"answer":42}`
		switch turn {
		case 1:
			name, args = "knotra_files_write", `{"path":"report.txt","content":"hello"}`
		case 2:
			name, args = "mcp_1", `{"value":42}`
		case 3:
			args = `{"answer":"wrong"}`
		case 4:
			if !strings.Contains(string(body), "Validation error:") {
				t.Error("invalid agent output was not returned to the model for correction")
			}
		default:
			t.Error("completed agent made an additional model call")
		}
		encoded, _ := json.Marshal(args)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":%q,"arguments":%s}]}`, turn, turn, name, encoded)
	}))
	defer model.Close()
	t.Setenv("KNOTRA_TEST_RIVER_AGENT_KEY", "fixture-key")
	profile := recoveryProfile()
	profile.Spec.Limits.MaxModelCalls = 4
	profile.Spec.Limits.MaxToolCalls = 4
	profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_TEST_RIVER_AGENT_KEY"}}
	profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: model.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "fixture"}}}}
	profile.Spec.Sandboxes["python"] = contract.SandboxProfile{Image: "python:3.13-alpine", Network: contract.Network{Mode: "none"}, AllowedTools: []string{"files.write"}, Resources: contract.Resources{CPU: 1, MemoryMiB: 128, DiskMiB: 32, Pids: 32}}
	profile.Spec.MCP = map[string]contract.MCPConnection{"data": {Transport: "streamable_http", URL: mcpRemote.URL, AllowedTools: []string{"stats"}, AllowRunSession: true, ToolPolicies: map[string]contract.ToolPolicy{"stats": {Effect: "read"}}}}
	options, api, _ := startRiverService(t, ctx, profile)
	const source = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: agent-mcp}
spec:
  models: {writer: {connection: cloud, requires: [toolCalling, structuredOutput]}}
  mcp: {data: {connection: data, session: run}}
  sandboxes: {box: {profile: python}}
  defaults:
    tools: {mcp: {data: [stats]}}
  nodes:
    write:
      type: agent
      sandbox: box
      tools: {sandbox: [files.write]}
      agent: {model: writer, maxSteps: 4, prompt: {text: Write the report and measure the value}}
      outputs:
        answer: {schema: {type: integer}}
        report: {artifact: {mediaTypes: [text/plain]}, collect: {path: report.txt, mediaType: text/plain}}
    first:
      type: tool
      needs: [write]
      tool: {server: data, name: stats, arguments: {expr: '{"value": 42}'}}
      outputs: {result: {schema: {type: object, properties: {value: {type: integer}}, required: [value]}}}
    second:
      type: tool
      needs: [first]
      tool: {server: data, name: stats, arguments: {expr: '{"value": 42}'}}
      outputs: {result: {schema: {type: object, properties: {value: {type: integer}}, required: [value]}}}
  outputs:
    answer: {schema: {type: integer}, bind: {from: nodes.write.outputs.answer}}
    report: {artifact: {mediaTypes: [text/plain]}, bind: {from: nodes.write.outputs.report}}
`
	var definition struct{ Definition protocol.Definition }
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
	if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
		t.Fatal(err)
	}
	var accepted struct{ Run protocol.Run }
	if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &accepted); err != nil {
		t.Fatal(err)
	}
	final := awaitTerminal(t, ctx, api, accepted.Run.ID)
	if final.Status != "succeeded" || string(final.Outputs["answer"]) != "42" || len(final.Artifacts) != 1 || turns.Load() != 4 {
		t.Fatalf("agent result=%+v turns=%d", final, turns.Load())
	}
	data, err := api.Bytes(ctx, "/artifacts/"+final.Artifacts[0].ID+"/content")
	if err != nil || string(data) != "hello" {
		t.Fatalf("agent artifact=%s error=%v", data, err)
	}
	for deletes.Load() < 2 {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("terminal MCP session was not closed", ctx.Err())
		}
	}
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if len(sessions) != 3 || sessions[0] == "" || sessions[0] != sessions[1] || sessions[1] != sessions[2] {
		t.Fatalf("run session was silently replaced between attempts: %v", sessions)
	}
	admin, err := store.Open(ctx, options.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var models, tools, attempts int
	if err := admin.Pool.QueryRow(ctx, `SELECT
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),
		(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1)`, final.ID).Scan(&models, &tools, &attempts); err != nil || models != 4 || tools != 4 || attempts != 3 {
		t.Fatalf("model calls=%d tool calls=%d attempts=%d error=%v", models, tools, attempts, err)
	}
}
