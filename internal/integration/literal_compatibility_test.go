package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

const literalRoot = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: literal-compatibility}
spec:
  files: [child.yaml, literal.py]
  limits: {timeout: 2m, maxConcurrentNodes: 2, maxNodeInstances: 16, maxModelCalls: %d, maxToolCalls: %d}
  inputs:
    marker: {schema: {type: string}}
  nodes:
    nested:
      type: pipeline
      inputs:
        marker: {schema: {type: string}, bind: {from: inputs.marker}}
      pipeline:
        file: child.yaml
        permissions: {models: [cloud], mcp: {data: [record]}, sandboxes: [python], secrets: []}
    review:
      type: human
      inputs:
        answer: {schema: {type: integer}, bind: {from: nodes.nested.outputs.answer}}
      human: {prompt: {text: Approve the literal provider results}}
      outputs:
        approved: {schema: {const: true}}
  outputs:
    answer: {schema: {type: integer}, bind: {from: nodes.nested.outputs.answer}}
    marker: {schema: {type: string}, bind: {from: nodes.nested.outputs.marker}}
    report: {artifact: {mediaTypes: [text/plain]}, bind: {from: nodes.nested.outputs.report}}
    approved: {schema: {const: true}, bind: {from: nodes.review.outputs.approved}}
`

const literalChild = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: literal-child}
spec:
  files: [literal.py]
%s
  models: {writer: {connection: cloud, requires: [toolCalling, structuredOutput]}}
  mcp: {data: {connection: data, session: run}}
  sandboxes: {python: {profile: python}}
  inputs:
    marker: {schema: {type: string}}
  nodes:
    generate:
      type: llm
      inputs:
        marker: {schema: {type: string}, bind: {from: inputs.marker}}
      llm: {model: writer, prompt: {text: fixture-llm}}
      outputs:
        answer: {schema: {const: 42}}
    agent:
      type: agent
      sandbox: python
      tools: {mcp: {data: [record]}}
      inputs:
        marker: {schema: {type: string}, bind: {from: inputs.marker}}
        answer: {schema: {const: 42}, bind: {from: nodes.generate.outputs.answer}}
      agent: {model: writer, maxSteps: 2, prompt: {text: fixture-agent}}
      outputs:
        answer: {schema: {const: 42}}
    record:
      type: tool
      inputs:
        marker: {schema: {type: string}, bind: {from: inputs.marker}}
        answer: {schema: {const: 42}, bind: {from: nodes.agent.outputs.answer}}
      tool:
        server: data
        name: record
        arguments: {expr: '{"value": args.answer, "marker": args.marker, "label": "tool"}'}
      outputs:
        result: {schema: {type: object}}
    code:
      type: code
      sandbox: python
      inputs:
        marker: {schema: {type: string}, bind: {from: inputs.marker}}
        result: {schema: {type: object}, bind: {from: nodes.record.outputs.result}}
      code: {command: [python, /package/literal.py]}
      outputs:
        answer: {schema: {const: 42}}
        marker: {schema: {type: string}}
        report: {artifact: {mediaTypes: [text/plain]}, collect: {path: report.txt, mediaType: text/plain}}
  outputs:
    answer: {schema: {const: 42}, bind: {from: nodes.code.outputs.answer}}
    marker: {schema: {type: string}, bind: {from: nodes.code.outputs.marker}}
    report: {artifact: {mediaTypes: [text/plain]}, bind: {from: nodes.code.outputs.report}}
`

const literalPython = `import json, os
from pathlib import Path
values = json.loads(Path(os.environ['KNOTRA_INPUT_JSON']).read_text())['values']
assert values['result'] == {'value': 42, 'marker': values['marker'], 'label': 'tool'}
Path('report.txt').write_text(values['marker'])
Path(os.environ['KNOTRA_OUTPUT_JSON']).write_text(json.dumps({'answer': 42, 'marker': values['marker']}))
`

// Both real services consume the same package and providers. The observations
// come from HTTP effects, Docker output and persisted budgets, not leaf callbacks.
func TestLiteralBackendCompatibilityAndNestedBudgets(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY=1 and KNOTRA_TEST_HELPER for literal backend compatibility")
	}
	t.Setenv("KNOTRA_LITERAL_KEY", "fixture-key")
	type observation struct {
		Status    string
		Models    int
		Writes    string
		Budgets   map[string]int
		Leaves    map[string]string
		Confirmed int
	}
	for _, test := range []struct {
		name                                           string
		rootModels, rootTools, childModels, childTools int
		models, tools, writes                          int
	}{
		{"exact", 3, 3, 3, 3, 3, 3, 2},
		{"root_model_limit", 2, 3, 0, 0, 2, 1, 1},
		{"child_model_limit", 3, 3, 2, 3, 2, 1, 1},
		{"root_tool_limit", 3, 1, 0, 0, 3, 1, 1},
		{"child_tool_limit", 3, 3, 3, 1, 3, 1, 1},
		{"root_code_limit", 3, 2, 0, 0, 3, 2, 2},
		{"child_code_limit", 3, 3, 3, 2, 3, 2, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var baseline observation
			for _, backend := range []string{execution.BackendTemporal, execution.BackendRiver} {
				t.Run(backend, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
					defer cancel()
					work := t.TempDir()
					marker := strings.Repeat("literal-input-", 12500)
					var models, agentTurns atomic.Int32
					model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodGet {
							_, _ = fmt.Fprint(w, `{"id":"fixture-model"}`)
							return
						}
						body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
						if err != nil || r.Header.Get("Authorization") != "Bearer fixture-key" || !strings.Contains(string(body), marker) {
							t.Errorf("provider input/auth: bytes=%d error=%v", len(body), err)
						}
						models.Add(1)
						name, args := "knotra_output", `{"answer":42}`
						if strings.Contains(string(body), "fixture-agent") {
							name = "knotra_finish"
							if agentTurns.Add(1) == 1 {
								name = "mcp_0"
								encoded, e := json.Marshal(map[string]any{"value": 42, "marker": marker, "label": "agent"})
								if e != nil {
									t.Error(e)
								}
								args = string(encoded)
							}
						} else if !strings.Contains(string(body), "fixture-llm") {
							t.Error("unrecognized model prompt")
						}
						encoded, _ := json.Marshal(args)
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":%q,"arguments":%s}]}`, name, encoded)
					}))
					defer model.Close()
					writesPath := filepath.Join(work, "external-writes")
					type record struct {
						Value  int    `json:"value"`
						Marker string `json:"marker"`
						Label  string `json:"label"`
					}
					server := mcp.NewServer(&mcp.Implementation{Name: "literal", Version: "1"}, nil)
					mcp.AddTool(server, &mcp.Tool{Name: "record", Description: "Persist an external write"}, func(_ context.Context, _ *mcp.CallToolRequest, input record) (*mcp.CallToolResult, record, error) {
						if input.Value != 42 || input.Marker != marker || (input.Label != "agent" && input.Label != "tool") {
							return nil, input, fmt.Errorf("invalid external write")
						}
						file, err := os.OpenFile(writesPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
						if err != nil {
							return nil, input, err
						}
						defer func() { _ = file.Close() }()
						if _, err := fmt.Fprintln(file, input.Label); err != nil {
							return nil, input, err
						}
						return nil, input, file.Sync()
					})
					remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
					defer remote.Close()
					profile := recoveryProfile()
					profile.Spec.Limits.MaxModelCalls, profile.Spec.Limits.MaxToolCalls = 3, 3
					profile.Spec.Secrets = map[string]contract.SecretSource{"key": {Env: "KNOTRA_LITERAL_KEY"}}
					profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: model.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "key"}}}}
					profile.Spec.MCP = map[string]contract.MCPConnection{"data": {Transport: "streamable_http", URL: remote.URL, AllowedTools: []string{"record"}, AllowRunSession: true, ToolPolicies: map[string]contract.ToolPolicy{"record": {Effect: "write"}}}}
					profilePath := filepath.Join(work, "profile.json")
					writeJSON(t, profilePath, profile)
					options := app.Options{Backend: backend, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: env("KNOTRA_TEST_TEMPORAL", "127.0.0.1:27235"), Namespace: "default", TaskQueue: "literal-" + uuid.NewString(), DataDir: filepath.Join(work, "engine"), DockerHost: os.Getenv("DOCKER_HOST"), HelperPath: os.Getenv("KNOTRA_TEST_HELPER"), Profiles: []string{profilePath}, Version: "literal-test"}
					if backend == execution.BackendRiver {
						options.TemporalAddress = "127.0.0.1:1"
					}
					optionsPath := filepath.Join(work, "server.json")
					writeJSON(t, optionsPath, options)
					api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
					process := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "engine.log"), api)
					source := fmt.Sprintf(literalRoot, test.rootModels, test.rootTools)
					childLimits := ""
					if test.childModels != 0 {
						childLimits = fmt.Sprintf("  limits: {timeout: 2m, maxConcurrentNodes: 2, maxNodeInstances: 16, maxModelCalls: %d, maxToolCalls: %d}", test.childModels, test.childTools)
					}
					pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}, {Path: "child.yaml", Content: []byte(fmt.Sprintf(literalChild, childLimits))}, {Path: "literal.py", Content: []byte(literalPython)}}}
					if _, diagnostics := contract.Compile(pkg, &profile); len(diagnostics) != 0 {
						t.Fatalf("literal package diagnostics: %+v", diagnostics)
					}
					var definition struct{ Definition protocol.Definition }
					if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
						t.Fatal(err)
					}
					var accepted struct{ Run protocol.Run }
					if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name, "inputs": map[string]any{"marker": marker}}, uuid.NewString(), &accepted); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if t.Failed() && backend == execution.BackendTemporal {
							cleanupWorkflows(t, options, []string{accepted.Run.ID})
						}
					})
					if test.name == "exact" {
						request := awaitHuman(t, ctx, api, accepted.Run.ID)
						if _, err := executeCLI(ctx, api, "requests", "respond", request.ID, "--output", "approved=true"); err != nil {
							t.Fatal(err)
						}
					}
					final := awaitTerminal(t, ctx, api, accepted.Run.ID)
					got := observation{Status: final.Status, Models: int(models.Load()), Budgets: map[string]int{}, Leaves: map[string]string{}}
					var childScope string
					for _, node := range final.Instances {
						if node.NodeID == "nested" {
							childScope = node.ID
						}
						if node.NodeType == "llm" || node.NodeType == "agent" || node.NodeType == "tool" || node.NodeType == "code" || node.NodeType == "human" {
							got.Leaves[node.NodeID] = node.Status
						}
					}
					writes, err := os.ReadFile(writesPath)
					if err != nil {
						t.Fatal(err)
					}
					got.Writes = string(writes)
					admin, err := store.Open(ctx, options.DatabaseURL)
					if err != nil {
						t.Fatal(err)
					}
					defer admin.Close()
					for name, scope := range map[string]string{"root": final.ID, "child": childScope} {
						for kind, want := range map[string]int{"model": test.models, "tool": test.tools} {
							var used int
							if err := admin.Pool.QueryRow(ctx, `SELECT COALESCE(sum(used),0) FROM knotra_budgets WHERE run_id=$1 AND scope=$2 AND kind=$3`, final.ID, scope, kind).Scan(&used); err != nil || used != want {
								t.Fatalf("%s %s budget=%d want=%d error=%v", name, kind, used, want, err)
							}
							got.Budgets[name+"/"+kind] = used
						}
					}
					if err := admin.Pool.QueryRow(ctx, `SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind IN ('model','tool') AND completed`, final.ID).Scan(&got.Confirmed); err != nil || got.Confirmed != test.models+test.tools {
						t.Fatalf("confirmed physical operations=%d want=%d error=%v", got.Confirmed, test.models+test.tools, err)
					}
					wantWrites := "agent\n"
					if test.writes == 2 {
						wantWrites += "tool\n"
					}
					if got.Models != test.models || got.Writes != wantWrites || childScope == "" {
						t.Fatalf("physical effects=%+v", got)
					}
					failedLeaf := ""
					switch {
					case test.models == 2:
						failedLeaf = "agent"
					case test.tools == 1:
						failedLeaf = "record"
					case test.tools == 2:
						failedLeaf = "code"
					}
					wantLeaves := map[string]string{}
					status := "succeeded"
					for _, node := range []string{"generate", "agent", "record", "code", "review"} {
						if node == failedLeaf {
							wantLeaves[node] = "failed"
							status = "cancelled"
						} else {
							wantLeaves[node] = status
						}
					}
					if !reflect.DeepEqual(got.Leaves, wantLeaves) {
						t.Fatalf("terminal leaves=%v want=%v", got.Leaves, wantLeaves)
					}
					if test.name == "exact" {
						var outputMarker string
						if err := json.Unmarshal(final.Outputs["marker"], &outputMarker); err != nil || outputMarker != marker || final.Status != "succeeded" || string(final.Outputs["answer"]) != "42" || string(final.Outputs["approved"]) != "true" || len(final.Artifacts) != 1 {
							t.Fatalf("literal exports: status=%s marker bytes=%d artifacts=%d error=%v", final.Status, len(outputMarker), len(final.Artifacts), err)
						}
						content, err := api.Bytes(ctx, "/artifacts/"+final.Artifacts[0].ID+"/content")
						if err != nil || string(content) != marker {
							t.Fatalf("real Docker artifact: bytes=%d error=%v", len(content), err)
						}
					} else if final.Status != "failed" || !strings.Contains(fmt.Sprint(final.Diagnostics), "BUDGET_EXCEEDED") || len(final.Outputs) != 0 || len(final.Artifacts) != 0 {
						t.Fatalf("budget exhaustion result: status=%s diagnostics=%v exports=%d artifacts=%d", final.Status, final.Diagnostics, len(final.Outputs), len(final.Artifacts))
					}
					if test.name != "exact" {
						exhaustedScope := final.ID
						if strings.HasPrefix(test.name, "child_") {
							exhaustedScope = childScope
						}
						if !strings.Contains(fmt.Sprint(final.Diagnostics), "in scope "+exhaustedScope) {
							t.Fatalf("wrong exhausted budget scope: %v", final.Diagnostics)
						}
					}
					process.kill(t)
					if backend == execution.BackendTemporal {
						baseline = got
					} else if !reflect.DeepEqual(got, baseline) {
						t.Fatalf("backend mismatch: Temporal=%+v River=%+v", baseline, got)
					}
					t.Logf("physical models=%d MCP writes=%d confirmed=%d root/child budgets=%v terminal leaves=%v", got.Models, test.writes, got.Confirmed, got.Budgets, got.Leaves)
				})
			}
		})
	}
}
