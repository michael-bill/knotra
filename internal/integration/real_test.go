package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/cli"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	temporalclient "go.temporal.io/sdk/client"
)

// This acceptance test runs real services end to end. No model, Docker, MCP,
// workflow, database, HTTP or client implementation is replaced by a mock.
func TestRealConcurrentPipelines(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_REAL") != "1" {
		t.Skip("set KNOTRA_TEST_REAL=1 for real Ollama/Docker/Temporal/PostgreSQL acceptance")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	workRoot := env("KNOTRA_TEST_WORKDIR", filepath.Join(root, ".knotra/integration-work"))
	if err := os.MkdirAll(workRoot, 0700); err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp(workRoot, "acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("failure evidence retained at %s", work)
		} else {
			if err := os.RemoveAll(work); err != nil {
				t.Error(err)
			}
		}
	})
	dsn := databaseSchema(t, ctx)
	serviceDir := filepath.Join(work, "mcp")
	if err := os.MkdirAll(serviceDir, 0700); err != nil {
		t.Fatal(err)
	}
	mcpURL := startMCPService(t, ctx, serviceDir)
	t.Setenv("KNOTRA_INTEGRATION_LABEL", "generated-integration-secret")
	profilePath := filepath.Join(work, "profile.json")
	profile := integrationProfile(mcpURL, env("KNOTRA_TEST_OLLAMA", "http://127.0.0.1:11434"))
	profileJSON, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, profileJSON, 0600); err != nil {
		t.Fatal(err)
	}
	address := unusedAddress(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	serverCtx, stopServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	options := app.Options{Listen: address, DatabaseURL: dsn, TemporalAddress: env("KNOTRA_TEST_TEMPORAL", "127.0.0.1:7233"), Namespace: env("KNOTRA_TEST_NAMESPACE", "default"), TaskQueue: "knotra-real-" + uuid.NewString(), DataDir: filepath.Join(work, "engine"), DockerHost: os.Getenv("DOCKER_HOST"), HelperPath: snapshotHelper(t, root, work), FirewallImage: env("KNOTRA_TEST_FIREWALL_IMAGE", "knotra-firewall:dev"), Profiles: []string{profilePath}, Version: "integration"}
	go func() { defer close(serverDone); serverDone <- app.Serve(serverCtx, options, logger) }()
	t.Cleanup(func() {
		stopServer()
		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(30 * time.Second):
			t.Error("engine did not shut down")
		}
	})
	api := &client.Client{BaseURL: "http://" + address, StateDir: filepath.Join(work, "client")}
	waitReady(t, ctx, api, serverDone)
	pkg, err := contract.LoadPackage(filepath.Join(root, "examples/integration/pipeline.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, diags := contract.Compile(pkg, &profile)
	if contract.HasErrors(diags) {
		t.Fatalf("scenario does not compile: %+v", diags)
	}
	if got := staticNodes(plan); got != 15 {
		t.Fatalf("expected 15 declared nodes, got %d", got)
	}
	var definition struct {
		Definition protocol.Definition `json:"definition"`
	}
	if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
		t.Fatal(err)
	}
	markers := []string{"report-alpha-" + uuid.NewString(), "report-beta-" + uuid.NewString(), "report-gamma-" + uuid.NewString()}
	type execution struct {
		marker      string
		run         protocol.Run
		events      []protocol.Event
		watchDone   chan error
		watchCancel context.CancelFunc
	}
	runs := make([]*execution, len(markers))
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		var owned []string
		for _, run := range runs {
			if run != nil {
				owned = append(owned, run.run.ID)
			}
		}
		cleanupWorkflows(t, options, owned)
	})
	started := make(chan error, len(markers))
	var starters sync.WaitGroup
	for i, marker := range markers {
		starters.Go(func() {
			var response struct {
				Run protocol.Run `json:"run"`
			}
			var err error
			if i == 0 {
				var output bytes.Buffer
				encoded, _ := json.Marshal(marker)
				err = cli.Execute(ctx, []string{"--endpoint", api.BaseURL, "--state-dir", filepath.Join(work, "cli"), "--json", "run", filepath.Join(root, "examples/integration/pipeline.yaml"), "--profile", profile.Metadata.Name, "--input", "marker=" + string(encoded)}, cli.Options{Out: &output, Err: &output})
				if err == nil {
					err = json.Unmarshal(output.Bytes(), &response)
				}
			} else {
				err = api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name, "inputs": map[string]any{"marker": marker}, "artifacts": map[string]any{}}, uuid.NewString(), &response)
			}
			if err != nil {
				started <- err
				return
			}
			runs[i] = &execution{marker: marker, run: response.Run, watchDone: make(chan error, 1)}
			t.Logf("started independent run %s (%s)", response.Run.ID, marker)
		})
	}
	starters.Wait()
	close(started)
	for err := range started {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	for _, run := range runs {
		runID := run.run.ID
		watchCtx, watchCancel := context.WithCancel(ctx)
		run.watchCancel = watchCancel
		defer watchCancel()
		go func() {
			done := errors.New("terminal event received")
			err := api.Watch(watchCtx, runID, "", func(event protocol.Event) error {
				run.events = append(run.events, event)
				if terminalEvent(event) {
					return done
				}
				return nil
			})
			if errors.Is(err, done) {
				err = nil
			}
			run.watchDone <- err
		}()
	}
	approved := map[string]bool{}
	finished := map[string]bool{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for len(finished) < len(runs) {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
		var requests protocol.Page[protocol.HumanRequest]
		if err := api.Get(ctx, "/requests", &requests); err != nil {
			t.Fatal(err)
		}
		for _, request := range requests.Items {
			if request.Status != "open" || approved[request.ID] {
				continue
			}
			var ignored any
			err := api.Command(ctx, "/requests/"+request.ID+"/response", map[string]any{"outputs": map[string]any{"approved": "yes"}}, uuid.NewString(), &ignored)
			var httpErr *client.HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != 422 {
				t.Fatalf("invalid human response was not rejected: %v", err)
			}
			if err := api.Command(ctx, "/requests/"+request.ID+"/response", map[string]any{"outputs": map[string]any{"approved": true}}, uuid.NewString(), &ignored); err != nil {
				t.Fatal(err)
			}
			approved[request.ID] = true
			t.Logf("validated and approved human request for run %s", request.RunID)
		}
		for _, run := range runs {
			if finished[run.run.ID] {
				continue
			}
			var response struct {
				Run protocol.Run `json:"run"`
			}
			if err := api.Get(ctx, "/runs/"+run.run.ID, &response); err != nil {
				t.Fatal(err)
			}
			if protocol.Terminal(response.Run.Status) {
				run.run = response.Run
				finished[run.run.ID] = true
				t.Logf("run %s finished %s with %d instances", run.run.ID, run.run.Status, len(run.run.Instances))
				if run.run.Status != "succeeded" {
					t.Errorf("run failed: %+v", run.run.Diagnostics)
				}
			} else if response.Run.Status == "waiting_resolution" {
				t.Fatalf("acceptance run requires unexpected operator resolution: %+v", response.Run.Diagnostics)
			}
		}
	}
	for _, run := range runs {
		select {
		case err := <-run.watchDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("terminal SSE event missing")
		}
	}
	if t.Failed() {
		return
	}
	if len(approved) != 3 {
		t.Fatalf("expected three independent approvals, got %d", len(approved))
	}
	allEvents := []protocol.Event{}
	leafIDs := map[string]bool{}
	artifacts := map[string]bool{}
	for _, run := range runs {
		if len(run.run.Instances) != 18 {
			t.Errorf("unexpected dynamic instance count %d", len(run.run.Instances))
		}
		if string(run.run.Outputs["doubled"]) != "[4,8,12]" || string(run.run.Outputs["iterations"]) != "2" || string(run.run.Outputs["verified"]) != "true" {
			t.Errorf("incorrect deterministic outputs: %v", run.run.Outputs)
		}
		skipped := 0
		for _, instance := range run.run.Instances {
			if instance.NodeID == "fallback" && instance.Status == "skipped" {
				skipped++
			}
			if slices.Contains([]string{"prepare", "double", "increment", "write_report", "narrative", "statistics", "assemble", "archive", "verify"}, instance.NodeID) {
				leafIDs[instance.ID] = true
			}
		}
		if skipped != 1 {
			t.Error("unselected branch was not skipped")
		}
		if len(run.run.Artifacts) != 1 {
			t.Fatalf("expected one published report, got %d", len(run.run.Artifacts))
		}
		artifact := run.run.Artifacts[0]
		if artifacts[artifact.ID] {
			t.Error("artifact handle shared between independent runs")
		}
		artifacts[artifact.ID] = true
		content, err := api.Bytes(ctx, "/artifacts/"+artifact.ID+"/content")
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		if artifact.SHA256 != hex.EncodeToString(sum[:]) || artifact.Size != int64(len(content)) {
			t.Error("artifact checksum/size mismatch")
		}
		if !strings.Contains(string(content), "Run marker: "+run.marker) || !strings.Contains(string(content), "Doubled total: 24") {
			t.Fatalf("report is not this run's result: %s", content)
		}
		for _, other := range markers {
			if other != run.marker && strings.Contains(string(content), other) {
				t.Fatal("agent/file data leaked between runs")
			}
		}
		allEvents = append(allEvents, run.events...)
		assertAgentEvidence(t, ctx, dsn, run.run)
	}
	slices.SortStableFunc(allEvents, func(a, b protocol.Event) int { return a.At.Compare(b.At) })
	active := map[string]string{}
	maxLeaves, maxRuns := 0, 0
	for _, event := range allEvents {
		if !leafIDs[event.InstanceID] {
			continue
		}
		data, _ := event.Data.(map[string]any)
		status, _ := data["status"].(string)
		if status == "running" {
			active[event.InstanceID] = event.RunID
		} else if protocol.Terminal(status) {
			delete(active, event.InstanceID)
		}
		activeRuns := map[string]bool{}
		for _, id := range active {
			activeRuns[id] = true
		}
		maxLeaves = max(maxLeaves, len(active))
		maxRuns = max(maxRuns, len(activeRuns))
	}
	if maxLeaves < 2 || maxRuns < 2 {
		t.Fatalf("no actual cross-run parallel execution: leaves=%d runs=%d", maxLeaves, maxRuns)
	}
	checkMCPJournal(t, serviceDir, markers)
	for _, run := range runs {
		command := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=io.knotra.run="+run.run.ID)
		if options.DockerHost != "" {
			command.Env = append(os.Environ(), "DOCKER_HOST="+options.DockerHost)
		}
		output, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(output)) != "" {
			t.Errorf("containers leaked for run %s: %s", run.run.ID, output)
		}
	}
	t.Logf("real acceptance passed: 3 concurrent runs, 15 static/18 dynamic nodes each, peak %d leaves across %d runs, 3 verified independent reports", maxLeaves, maxRuns)
}

func integrationProfile(mcpURL, ollamaURL string) contract.Profile {
	return contract.Profile{APIVersion: "knotra/v1", Kind: "EngineProfile", Metadata: contract.Metadata{Name: "real-integration"}, Spec: contract.ProfileSpec{
		Secrets:   map[string]contract.SecretSource{"integration_label": {Env: "KNOTRA_INTEGRATION_LABEL"}},
		Models:    map[string]contract.ModelConnection{"local_model": {Provider: "ollama", Model: "qwen3.5:9b", BaseURL: ollamaURL, Parameters: map[string]any{"think": false, "temperature": 0, "num_predict": 512, "num_ctx": 8192}}},
		MCP:       map[string]contract.MCPConnection{"dataset_service": {Transport: "streamable_http", URL: mcpURL, AllowedTools: []string{"stats", "archive"}, AllowRunSession: true, ToolPolicies: map[string]contract.ToolPolicy{"stats": {Effect: "read"}, "archive": {Effect: "write", IdempotencyArgument: "/key"}}}},
		Sandboxes: map[string]contract.SandboxProfile{"python": {Image: "python:3.13-alpine", Resources: contract.Resources{CPU: 0.5, MemoryMiB: 256, DiskMiB: 64, Pids: 64}, Network: contract.Network{Mode: "none"}, AllowedTools: []string{"files.read", "files.write", "process.exec"}, AllowedSecrets: []string{"integration_label"}}},
		Limits:    contract.Limits{Timeout: "30m", MaxConcurrentNodes: 6, MaxNodeInstances: 128, MaxModelCalls: 40, MaxToolCalls: 100},
	}}
}

func TestRealScenarioCompiles(t *testing.T) {
	pkg, err := contract.LoadPackage("../../examples/integration/pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	profile := integrationProfile("http://127.0.0.1:9999", "http://127.0.0.1:11434")
	plan, diags := contract.Compile(pkg, &profile)
	if contract.HasErrors(diags) {
		t.Fatalf("scenario rejected: %+v", diags)
	}
	if staticNodes(plan) != 15 {
		t.Fatal("scenario must exercise 15 declarations")
	}
	box := plan.Profile.Spec.Sandboxes["python"]
	box.Image = "pinned-by-admission"
	plan.Profile.Spec.Sandboxes["python"] = box
	if profile.Spec.Sandboxes["python"].Image != "python:3.13-alpine" {
		t.Fatal("admission snapshot mutation leaked into shared profile")
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Admission pins the helper digest. Keep a per-test immutable copy so building
// the development binary while a long model request runs cannot alter its plan.
func snapshotHelper(t *testing.T, root, work string) string {
	t.Helper()
	data, err := os.ReadFile(env("KNOTRA_TEST_HELPER", filepath.Join(root, ".knotra/bin/sandbox-helper")))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(work, "sandbox-helper")
	if err := os.WriteFile(path, data, 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Failed tests must not leave unreachable workflows after their private database
// schema is removed. Never list or terminate workflows belonging to another test
// or user: the only IDs considered are admissions returned to this test.
func cleanupWorkflows(t *testing.T, options app.Options, owned []string) {
	t.Helper()
	if len(owned) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := temporalclient.DialContext(ctx, temporalclient.Options{HostPort: options.TemporalAddress, Namespace: options.Namespace})
	if err != nil {
		t.Errorf("connect for owned workflow cleanup: %v", err)
		return
	}
	defer connection.Close()
	for _, id := range owned {
		state, err := connection.DescribeWorkflowExecution(ctx, id, "")
		var missing *serviceerror.NotFound
		if errors.As(err, &missing) {
			continue
		}
		if err != nil {
			t.Errorf("inspect owned workflow %s: %v", id, err)
			continue
		}
		if state.WorkflowExecutionInfo.Status == enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
			if err := connection.TerminateWorkflow(ctx, id, "", "failed integration test cleanup"); err != nil && !errors.As(err, &missing) {
				t.Errorf("terminate owned workflow %s: %v", id, err)
			}
		}
	}
}

func unusedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func databaseSchema(t *testing.T, ctx context.Context) string {
	t.Helper()
	dsn := os.Getenv("KNOTRA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("KNOTRA_TEST_DATABASE_URL is required")
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "knotra_acceptance_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
		connection.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := connection.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		_ = connection.Close(cleanup)
	})
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return dsn + " search_path=" + name
	}
	query := parsed.Query()
	query.Set("search_path", name)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func waitReady(t *testing.T, ctx context.Context, c *client.Client, done <-chan error) {
	t.Helper()
	timeout := time.NewTimer(time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		var info map[string]any
		if c.Get(ctx, "/info", &info) == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("engine startup failed: %v", err)
		case <-timeout.C:
			t.Fatal("engine startup timeout")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func staticNodes(plan *contract.Plan) int {
	var graph func(contract.Graph) int
	graph = func(g contract.Graph) int {
		n := len(g.Nodes)
		for _, node := range g.Nodes {
			if node.Foreach != nil {
				n += graph(node.Foreach.Body)
			}
			if node.Loop != nil {
				n += graph(node.Loop.Body)
			}
		}
		return n
	}
	total := 0
	for _, pipeline := range plan.Pipelines {
		total += graph(pipeline.Spec.Graph)
	}
	return total
}

type statsInput struct {
	Numbers []int64 `json:"numbers"`
	Marker  string  `json:"marker"`
}
type statsOutput struct {
	Count int     `json:"count"`
	Sum   int64   `json:"sum"`
	Mean  float64 `json:"mean"`
}
type archiveInput struct {
	Caption string `json:"caption"`
	Total   int64  `json:"total"`
	Marker  string `json:"marker"`
	Key     string `json:"key"`
}
type archiveOutput struct {
	Receipt string `json:"receipt"`
	Total   int64  `json:"total"`
	Writes  int    `json:"writes"`
}
type auditRecord struct {
	Tool, Marker, Session string
	At                    time.Time
}

// A functional MCP data service runs in a separate OS process. It computes real
// statistics and durably archives results, including idempotency receipts.
func TestMCPServiceProcess(t *testing.T) {
	directory := os.Getenv("KNOTRA_INTEGRATION_MCP_PROCESS")
	if directory == "" {
		t.Skip("MCP subprocess entrypoint")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "knotra-dataset-service", Version: "1.0.0"}, nil)
	var mu sync.Mutex
	appendAudit := func(record auditRecord) error {
		file, err := os.OpenFile(filepath.Join(directory, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := json.NewEncoder(file).Encode(record); err != nil {
			return err
		}
		return file.Sync()
	}
	mcp.AddTool(server, &mcp.Tool{Name: "stats", Description: "Compute count, sum and mean for numbers belonging to one run marker."}, func(ctx context.Context, req *mcp.CallToolRequest, input statsInput) (*mcp.CallToolResult, statsOutput, error) {
		result := statsOutput{Count: len(input.Numbers)}
		for _, number := range input.Numbers {
			result.Sum += number
		}
		if result.Count > 0 {
			result.Mean = float64(result.Sum) / float64(result.Count)
		}
		mu.Lock()
		defer mu.Unlock()
		err := appendAudit(auditRecord{"stats", input.Marker, req.Session.ID(), time.Now().UTC()})
		return nil, result, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "archive", Description: "Durably save a report summary once per idempotency key."}, func(ctx context.Context, req *mcp.CallToolRequest, input archiveInput) (*mcp.CallToolResult, archiveOutput, error) {
		mu.Lock()
		defer mu.Unlock()
		hash := sha256.Sum256([]byte(input.Key))
		path := filepath.Join(directory, "record-"+hex.EncodeToString(hash[:])+".json")
		record := struct {
			Input  archiveInput
			Output archiveOutput
		}{Input: input, Output: archiveOutput{Receipt: input.Key, Total: input.Total, Writes: 1}}
		raw, err := os.ReadFile(path)
		if err == nil {
			var existing struct {
				Input  archiveInput
				Output archiveOutput
			}
			if err := json.Unmarshal(raw, &existing); err != nil {
				return nil, archiveOutput{}, err
			}
			if existing.Input != input {
				return nil, archiveOutput{}, fmt.Errorf("idempotency key reused with other input")
			}
			return nil, existing.Output, nil
		}
		if !os.IsNotExist(err) {
			return nil, archiveOutput{}, err
		}
		raw, err = json.Marshal(record)
		if err != nil {
			return nil, archiveOutput{}, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, archiveOutput{}, err
		}
		if _, err = file.Write(raw); err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, archiveOutput{}, err
		}
		err = appendAudit(auditRecord{"archive", input.Marker, req.Session.ID(), time.Now().UTC()})
		return nil, record.Output, err
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "KNOTRA_MCP_URL=http://"+listener.Addr().String())
	if err := http.Serve(listener, handler); err != nil {
		t.Fatal(err)
	}
}

func startMCPService(t *testing.T, ctx context.Context, directory string) string {
	t.Helper()
	childCtx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestMCPServiceProcess$", "-test.v", "-test.timeout=30m")
	command.Env = append(os.Environ(), "KNOTRA_INTEGRATION_MCP_PROCESS="+directory)
	command.Stderr = os.Stderr
	output, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("MCP process did not exit")
		}
	})
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "KNOTRA_MCP_URL=") {
				ready <- strings.TrimPrefix(line, "KNOTRA_MCP_URL=")
			}
		}
	}()
	select {
	case endpoint := <-ready:
		return endpoint
	case <-time.After(30 * time.Second):
		t.Fatal("MCP subprocess startup timeout")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return ""
}

func checkMCPJournal(t *testing.T, directory string, markers []string) {
	t.Helper()
	file, err := os.Open(filepath.Join(directory, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	stats := map[string]int{}
	writes := map[string]int{}
	sessions := map[string]map[string]bool{}
	for {
		var record auditRecord
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(markers, record.Marker) {
			t.Fatalf("MCP received wrong/cross-run marker %q", record.Marker)
		}
		if record.Tool == "stats" {
			stats[record.Marker]++
		} else {
			writes[record.Marker]++
		}
		if sessions[record.Marker] == nil {
			sessions[record.Marker] = map[string]bool{}
		}
		sessions[record.Marker][record.Session] = true
	}
	unique := map[string]bool{}
	for _, marker := range markers {
		if stats[marker] < 2 || writes[marker] != 1 {
			t.Errorf("MCP work missing/duplicated for %s: stats=%d archive=%d", marker, stats[marker], writes[marker])
		}
		if len(sessions[marker]) != 1 {
			t.Errorf("run session unexpectedly changed: %v", sessions[marker])
		}
		for session := range sessions[marker] {
			if session == "" || unique[session] {
				t.Error("MCP run session shared across runs or missing")
			}
			unique[session] = true
		}
	}
	files, err := filepath.Glob(filepath.Join(directory, "record-*.json"))
	if err != nil || len(files) != len(markers) {
		t.Fatalf("expected three durable archive records, got %d (%v)", len(files), err)
	}
}
