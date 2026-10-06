package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func TestRiverCancellationStopsAgentAndMCPAndReleasesResources(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" || os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL and KNOTRA_TEST_HELPER for River cancellation acceptance")
	}
	t.Setenv("KNOTRA_TEST_CANCELLATION_KEY", "fixture-key")
	for _, mode := range []string{"agent", "tool", "delete_retry"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			blocked, interrupted := make(chan struct{}, 1), make(chan struct{}, 1)
			var modelCalls, toolCalls, deletes atomic.Int32
			var deleted atomic.Bool
			var cleanupHeaders atomic.Value
			var runSession atomic.Value
			type value struct {
				Value int `json:"value"`
			}
			server := mcp.NewServer(&mcp.Implementation{Name: "cancellation-fixture", Version: "1"}, nil)
			mcp.AddTool(server, &mcp.Tool{Name: "stats", Description: "Measure a value"}, func(call context.Context, req *mcp.CallToolRequest, input value) (*mcp.CallToolResult, value, error) {
				toolCalls.Add(1)
				runSession.Store(req.Session.ID())
				if input.Value != 42 {
					t.Errorf("downstream tool executed after cancellation: %+v", input)
				}
				if mode != "agent" {
					blocked <- struct{}{}
					select {
					case <-call.Done():
						interrupted <- struct{}{}
					case <-ctx.Done():
						return nil, value{}, ctx.Err()
					}
					return nil, value{}, call.Err()
				}
				return nil, input, nil
			})
			transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{SessionTimeout: time.Minute})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, ok := runSession.Load().(string)
				cleanup := ok && r.Method == http.MethodDelete && r.Header.Get("Mcp-Session-Id") == id
				if cleanup {
					cleanupHeaders.Store(r.Header.Clone())
					if count := deletes.Add(1); mode == "delete_retry" && count == 1 {
						http.Error(w, "temporary cleanup failure", http.StatusServiceUnavailable)
						return
					}
				}
				transport.ServeHTTP(w, r)
				if cleanup {
					deleted.Store(true)
				}
			}))
			defer func() { cancel(); remote.Close() }()
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"id":"fixture-model"}`)
					return
				}
				if _, err := io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20)); err != nil {
					t.Error(err)
					return
				}
				turn := modelCalls.Add(1)
				if turn == 3 {
					blocked <- struct{}{}
					select {
					case <-r.Context().Done():
						interrupted <- struct{}{}
					case <-ctx.Done():
					}
					return
				}
				if turn > 3 {
					t.Error("cancelled agent made another model call")
				}
				name, args := "knotra_files_write", `{"path":"report.txt","content":"confirmed-prefix"}`
				if turn == 2 {
					name, args = "mcp_1", `{"value":42}`
				}
				encoded, _ := json.Marshal(args)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":%q,"arguments":%s}]}`, turn, turn, name, encoded)
			}))
			defer func() { cancel(); model.Close() }()
			profile := recoveryProfile()
			profile.Spec.Limits.Timeout = "3m"
			profile.Spec.Limits.MaxModelCalls, profile.Spec.Limits.MaxToolCalls = 4, 4
			profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_TEST_CANCELLATION_KEY"}}
			profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: model.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "fixture"}}}}
			profile.Spec.Sandboxes["python"] = contract.SandboxProfile{Image: "python:3.13-alpine", Network: contract.Network{Mode: "none"}, AllowedTools: []string{"files.write"}, Resources: contract.Resources{CPU: 1, MemoryMiB: 128, DiskMiB: 32, Pids: 32}}
			profile.Spec.MCP = map[string]contract.MCPConnection{"data": {Transport: "streamable_http", URL: remote.URL, AllowedTools: []string{"stats"}, AllowRunSession: true, ToolPolicies: map[string]contract.ToolPolicy{"stats": {Effect: "read"}}}}
			options, api, _ := startRiverService(t, ctx, profile)
			source := riverToolCancellationPipeline
			if mode == "agent" {
				source = riverAgentCancellationPipeline
			}
			var definition struct{ Definition protocol.Definition }
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var admitted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &admitted); err != nil {
				t.Fatal(err)
			}
			select {
			case <-blocked:
			case <-ctx.Done():
				t.Fatal("external call did not start", ctx.Err())
			}
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			var deadline time.Time
			if err := admin.Pool.QueryRow(ctx, "SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id=$2", admitted.Run.ID, "work").Scan(&deadline); err != nil {
				t.Fatal(err)
			}
			key := uuid.NewString()
			for range 2 {
				if err := api.Command(ctx, "/runs/"+admitted.Run.ID+"/cancel", map[string]any{}, key, new(any)); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-interrupted:
			case <-time.After(15 * time.Second):
				t.Fatal("cancel did not interrupt the physical external call")
			}
			final := awaitTerminal(t, ctx, api, admitted.Run.ID)
			if final.Status != "cancelled" || len(final.Artifacts) != 0 || len(final.Outputs) != 0 {
				t.Fatalf("cancelled run published a result: status=%s artifacts=%d outputs=%d", final.Status, len(final.Artifacts), len(final.Outputs))
			}
			wantDeletes := int32(1)
			if mode == "delete_retry" {
				wantDeletes = 2
			}
			cleanup, stopCleanup := context.WithTimeout(ctx, 25*time.Second)
			defer stopCleanup()
			for deletes.Load() < wantDeletes || !deleted.Load() {
				select {
				case <-time.After(25 * time.Millisecond):
				case <-cleanup.Done():
					t.Fatalf("run session cleanup attempted %d DELETE requests, want %d", deletes.Load(), wantDeletes)
				}
			}
			probe, err := http.NewRequestWithContext(cleanup, http.MethodGet, remote.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			headers, ok := cleanupHeaders.Load().(http.Header)
			if !ok {
				t.Fatal("cleanup did not send session headers")
			}
			probe.Header = headers.Clone()
			probe.Header.Set("Accept", "text/event-stream")
			response, err := remote.Client().Do(probe)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("cancelled run retained its remote MCP session: HTTP %d", response.StatusCode)
			}
			var attempts, models, tools, confirmed, slots, requests, resources, published, receipts int
			var finalDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),0),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0),
				(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='tool' AND completed),
				(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
				(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'),
				(SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND state<>'closed'),
				(SELECT count(*) FROM knotra_artifacts WHERE published AND document->'origin'->>'runId'=$1),
				(SELECT count(*) FROM knotra_commands WHERE id=$2),
				(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='work')`, final.ID, key).Scan(&attempts, &models, &tools, &confirmed, &slots, &requests, &resources, &published, &receipts, &finalDeadline); err != nil {
				t.Fatal(err)
			}
			wantModels, wantTools, wantConfirmed := 0, 1, 0
			if mode == "agent" {
				wantModels, wantTools, wantConfirmed = 3, 2, 2
			}
			if attempts != 1 || models != wantModels || modelCalls.Load() != int32(wantModels) || tools != wantTools || toolCalls.Load() != 1 || confirmed != wantConfirmed || slots != 0 || requests != 0 || resources != 0 || published != 0 || receipts != 1 || !deadline.Equal(finalDeadline) {
				t.Fatalf("attempts=%d models=%d/%d tools=%d/%d confirmed=%d slots=%d requests=%d resources=%d published=%d receipts=%d deadline=%v", attempts, models, modelCalls.Load(), tools, toolCalls.Load(), confirmed, slots, requests, resources, published, receipts, deadline.Equal(finalDeadline))
			}
			if mode == "agent" {
				var resourceID string
				if err := admin.Pool.QueryRow(ctx, "SELECT id FROM knotra_resources WHERE run_id=$1 AND kind='sandbox'", final.ID).Scan(&resourceID); err != nil {
					t.Fatal(err)
				}
				data, inspectErr := exec.CommandContext(ctx, "docker", "inspect", "sandbox-"+resourceID).CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(inspectErr, &exit) || exit.ExitCode() != 1 || !strings.Contains(strings.ToLower(string(data)), "no such object: sandbox-"+resourceID) {
					t.Fatalf("sandbox deletion is unproven: output=%s error=%v", data, inspectErr)
				}
				if err := filepath.WalkDir(filepath.Join(options.DataDir, "work"), func(path string, _ os.DirEntry, err error) error {
					if err == nil && strings.HasSuffix(path, "sandbox-"+resourceID) {
						return fmt.Errorf("cancelled agent retained its workspace")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			t.Log("cancellation stopped physical work, kept confirmed tool evidence, closed the run MCP session and released capacity without another attempt")
		})
	}
}

const riverToolCancellationPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: tool-cancellation}
spec:
  mcp: {data: {connection: data, session: run}}
  nodes:
    work:
      type: tool
      execution: {timeout: 2m, retry: {maxAttempts: 3, backoff: 1ms}}
      tool: {server: data, name: stats, arguments: {expr: '{"value":42}'}}
      outputs: {result: {schema: {type: object}}}
    after:
      type: tool
      needs: [work]
      tool: {server: data, name: stats, arguments: {expr: '{"value":99}'}}
      outputs: {result: {schema: {type: object}}}
  outputs:
    result: {schema: {type: object}, bind: {from: nodes.after.outputs.result}}
`

const riverAgentCancellationPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: agent-cancellation}
spec:
  models: {writer: {connection: cloud, requires: [toolCalling, structuredOutput]}}
  mcp: {data: {connection: data, session: run}}
  sandboxes: {box: {profile: python}}
  defaults:
    tools: {mcp: {data: [stats]}}
  nodes:
    work:
      type: agent
      sandbox: box
      execution: {timeout: 2m, retry: {maxAttempts: 3, backoff: 1ms}}
      tools: {sandbox: [files.write]}
      agent: {model: writer, maxSteps: 4, prompt: {text: Write then measure}}
      outputs:
        answer: {schema: {type: integer}}
        report: {artifact: {mediaTypes: [text/plain]}, collect: {path: report.txt, mediaType: text/plain}}
    after:
      type: tool
      needs: [work]
      tool: {server: data, name: stats, arguments: {expr: '{"value":99}'}}
      outputs: {result: {schema: {type: object}}}
  outputs:
    answer: {schema: {type: integer}, bind: {from: nodes.work.outputs.answer}}
    report: {artifact: {mediaTypes: [text/plain]}, bind: {from: nodes.work.outputs.report}}
`
