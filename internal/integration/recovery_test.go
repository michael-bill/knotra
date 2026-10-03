package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/cli"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
)

func TestRealProcessRecovery(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_REAL") != "1" && os.Getenv("KNOTRA_TEST_RECOVERY") != "1" {
		t.Skip("set KNOTRA_TEST_RECOVERY=1 for Docker/Temporal/PostgreSQL process recovery")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	workRoot := env("KNOTRA_TEST_WORKDIR", filepath.Join(root, ".knotra/integration-work"))
	if err := os.MkdirAll(workRoot, 0700); err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp(workRoot, "recovery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("recovery evidence retained at %s", work)
		} else if err := os.RemoveAll(work); err != nil {
			t.Error(err)
		}
	})
	profile := recoveryProfile()
	profilePath := filepath.Join(work, "profile.json")
	writeJSON(t, profilePath, profile)
	options := app.Options{Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: env("KNOTRA_TEST_TEMPORAL", "127.0.0.1:7233"), Namespace: env("KNOTRA_TEST_NAMESPACE", "default"), TaskQueue: "knotra-recovery-" + uuid.NewString(), DataDir: filepath.Join(work, "engine"), DockerHost: os.Getenv("DOCKER_HOST"), HelperPath: snapshotHelper(t, root, work), FirewallImage: env("KNOTRA_TEST_FIREWALL_IMAGE", "knotra-firewall:dev"), Profiles: []string{profilePath}, Version: "recovery-test"}
	optionsPath := filepath.Join(work, "server.json")
	writeJSON(t, optionsPath, options)
	api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
	var owned []string
	t.Cleanup(func() {
		if t.Failed() {
			cleanupWorkflows(t, options, owned)
		}
	})
	first := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
	identity := engineIdentity(t, ctx, api)
	marker := "restart-" + uuid.NewString()
	run := startCLIRun(t, ctx, api, filepath.Join(root, "examples/integration/recovery.yaml"), marker)
	owned = append(owned, run.ID)
	request := awaitHuman(t, ctx, api, run.ID)
	var envelope struct {
		Artifacts map[string]contract.Artifact `json:"artifacts"`
	}
	raw, err := json.Marshal(request.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("invalid human inputs: %v", err)
	}
	artifact := envelope.Artifacts["document"]
	before, err := api.Bytes(ctx, "/artifacts/"+artifact.ID+"/content")
	if err != nil {
		t.Fatal(err)
	}
	var document struct{ Marker, Nonce string }
	if err := json.Unmarshal(before, &document); err != nil || document.Marker != marker || document.Nonce == "" {
		t.Fatalf("invalid checkpoint file: %s (%v)", before, err)
	}
	prefix := readEventsUntil(t, ctx, api, run.ID, "", func(e protocol.Event) bool { return e.InstanceID == request.InstanceID && e.Message == "waiting_human" })
	cursor := prefix[len(prefix)-1].ID
	// Kill the process without graceful shutdown. Neither its Go heap nor worker
	// cache survives; recovery must use durable Temporal and PostgreSQL state.
	first.kill(t)
	t.Logf("killed engine at human request %s after artifact %s was published", request.ID, artifact.ID)
	startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
	if engineIdentity(t, ctx, api) != identity {
		t.Fatal("engine identity changed across restart")
	}
	recovered := awaitHuman(t, ctx, api, run.ID)
	if recovered.ID != request.ID || !recovered.Deadline.Equal(request.Deadline) {
		t.Fatal("human request identity or deadline changed during recovery")
	}
	after, err := api.Bytes(ctx, "/artifacts/"+artifact.ID+"/content")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("published file did not survive restart: %v", err)
	}
	replayed := readEventsUntil(t, ctx, api, run.ID, "", func(e protocol.Event) bool { return e.ID == cursor })
	if !reflect.DeepEqual(prefix, replayed) {
		t.Fatal("durable SSE prefix changed after process restart")
	}
	if _, err := executeCLI(ctx, api, "requests", "respond", request.ID, "--output", "approved=true"); err != nil {
		t.Fatal(err)
	}
	run = awaitTerminal(t, ctx, api, run.ID)
	if run.Status != "succeeded" || string(run.Outputs["verified"]) != "true" {
		t.Fatalf("recovered execution failed: %s %+v", run.Status, run.Diagnostics)
	}
	var nonce string
	if err := json.Unmarshal(run.Outputs["nonce"], &nonce); err != nil || nonce != document.Nonce {
		t.Fatal("completed code was repeated or its recorded output changed")
	}
	if len(run.Artifacts) != 1 || run.Artifacts[0].ID != artifact.ID {
		t.Fatal("recovery replaced the previously published artifact")
	}
	suffix := readEventsUntil(t, ctx, api, run.ID, cursor, terminalEvent)
	seen := map[string]bool{}
	for _, event := range prefix {
		seen[event.ID] = true
	}
	for _, event := range suffix {
		if seen[event.ID] {
			t.Fatalf("SSE cursor replay duplicated event %s", event.ID)
		}
		seen[event.ID] = true
	}
	assertSinglePreparation(t, run, append(prefix, suffix...))

	// A second real run is cancelled while waiting. The cancellation is durable
	// before its HTTP response, so a later answer must not resurrect execution.
	cancelled := startCLIRun(t, ctx, api, filepath.Join(root, "examples/integration/recovery.yaml"), "cancel-"+uuid.NewString())
	owned = append(owned, cancelled.ID)
	open := awaitHuman(t, ctx, api, cancelled.ID)
	if _, err := executeCLI(ctx, api, "runs", "cancel", cancelled.ID); err != nil {
		t.Fatal(err)
	}
	var ignored any
	err = api.Command(ctx, "/requests/"+open.ID+"/response", map[string]any{"outputs": map[string]any{"approved": true}}, uuid.NewString(), &ignored)
	var httpError *client.HTTPError
	if !errors.As(err, &httpError) || httpError.Status != 409 {
		t.Fatalf("late answer after cancellation was not rejected with 409: %v", err)
	}
	cancelled = awaitTerminal(t, ctx, api, cancelled.ID)
	if cancelled.Status != "cancelled" {
		t.Fatalf("cancelled run became %s: %+v", cancelled.Status, cancelled.Diagnostics)
	}
	for _, instance := range cancelled.Instances {
		if instance.NodeID == "verify" && instance.AttemptID != "" {
			t.Fatal("downstream code was dispatched after cancellation")
		}
	}
	check := readEventsUntil(t, ctx, api, cancelled.ID, "", terminalEvent)
	assertSinglePreparation(t, cancelled, check)
	t.Log("process recovery passed: stable identity, unchanged artifact, no repeated code, exact SSE replay, CLI response/cancellation, late answer rejected")
}

func TestRecoveryScenarioCompiles(t *testing.T) {
	pkg, err := contract.LoadPackage("../../examples/integration/recovery.yaml")
	if err != nil {
		t.Fatal(err)
	}
	profile := recoveryProfile()
	if _, diags := contract.Compile(pkg, &profile); contract.HasErrors(diags) {
		t.Fatalf("recovery scenario rejected: %+v", diags)
	}
}

func recoveryProfile() contract.Profile {
	return contract.Profile{APIVersion: "knotra/v1", Kind: "EngineProfile", Metadata: contract.Metadata{Name: "recovery"}, Spec: contract.ProfileSpec{
		Sandboxes: map[string]contract.SandboxProfile{"python": {Image: "python:3.13-alpine", Network: contract.Network{Mode: "none"}, AllowedTools: []string{"process.exec"}, Resources: contract.Resources{CPU: 0.5, MemoryMiB: 128, DiskMiB: 32, Pids: 32}}},
		Limits:    contract.Limits{Timeout: "10m", MaxConcurrentNodes: 2, MaxNodeInstances: 16, MaxModelCalls: 2, MaxToolCalls: 2},
	}}
}

// TestEngineProcess is a subprocess entrypoint, not a substitute engine. It
// invokes the same application composition as `knotra serve`.
func TestEngineProcess(t *testing.T) {
	path := os.Getenv("KNOTRA_INTEGRATION_ENGINE_OPTIONS")
	if path == "" {
		t.Skip("engine subprocess entrypoint")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var options app.Options
	if err := json.Unmarshal(data, &options); err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := app.Serve(ctx, options, slog.New(slog.NewTextHandler(os.Stderr, nil))); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type engineProcess struct {
	command  *exec.Cmd
	finished chan struct{}
}

func startEngineProcess(t *testing.T, ctx context.Context, options, logPath string, api *client.Client) *engineProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestEngineProcess$", "-test.timeout=10m", "-test.v")
	command.Env = append(os.Environ(), "KNOTRA_INTEGRATION_ENGINE_OPTIONS="+options)
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	process := &engineProcess{command: command, finished: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		err := command.Wait()
		log.Close()
		done <- err
		close(done)
		close(process.finished)
	}()
	t.Cleanup(func() { process.kill(t) })
	waitReady(t, ctx, api, done)
	return process
}

func (p *engineProcess) kill(t *testing.T) {
	t.Helper()
	select {
	case <-p.finished:
		return
	default:
	}
	if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error(err)
	}
	select {
	case <-p.finished:
	case <-time.After(15 * time.Second):
		t.Error("engine process did not exit")
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func engineIdentity(t *testing.T, ctx context.Context, api *client.Client) string {
	t.Helper()
	var info struct{ EngineID string }
	if err := api.Get(ctx, "/info", &info); err != nil || info.EngineID == "" {
		t.Fatalf("invalid engine identity: %v", err)
	}
	return info.EngineID
}

func executeCLI(ctx context.Context, api *client.Client, args ...string) ([]byte, error) {
	var output bytes.Buffer
	full := append([]string{"--endpoint", api.BaseURL, "--state-dir", api.StateDir, "--json"}, args...)
	err := cli.Execute(ctx, full, cli.Options{Out: &output, Err: &output})
	return output.Bytes(), err
}

func startCLIRun(t *testing.T, ctx context.Context, api *client.Client, path, marker string) protocol.Run {
	t.Helper()
	value, _ := json.Marshal(marker)
	out, err := executeCLI(ctx, api, "run", path, "--profile", "recovery", "--input", "marker="+string(value))
	if err != nil {
		t.Fatalf("CLI run failed: %v (%s)", err, out)
	}
	var response struct{ Run protocol.Run }
	if err := json.Unmarshal(out, &response); err != nil || response.Run.ID == "" {
		t.Fatalf("invalid CLI run result: %s (%v)", out, err)
	}
	return response.Run
}

func awaitHuman(t *testing.T, ctx context.Context, api *client.Client, runID string) protocol.HumanRequest {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var page protocol.Page[protocol.HumanRequest]
		if err := api.Get(ctx, "/requests", &page); err != nil {
			t.Fatal(err)
		}
		for _, request := range page.Items {
			if request.RunID == runID && request.Status == "open" {
				return request
			}
		}
		var response struct{ Run protocol.Run }
		if err := api.Get(ctx, "/runs/"+runID, &response); err != nil {
			t.Fatal(err)
		}
		if protocol.Terminal(response.Run.Status) {
			t.Fatalf("run terminated before opening a human request: %s %+v", response.Run.Status, response.Run.Diagnostics)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func awaitTerminal(t *testing.T, ctx context.Context, api *client.Client, runID string) protocol.Run {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var response struct{ Run protocol.Run }
		if err := api.Get(ctx, "/runs/"+runID, &response); err != nil {
			t.Fatal(err)
		}
		if protocol.Terminal(response.Run.Status) {
			return response.Run
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func terminalEvent(event protocol.Event) bool {
	data, _ := event.Data.(map[string]any)
	status, _ := data["status"].(string)
	return event.InstanceID == "" && protocol.Terminal(status)
}

func readEventsUntil(t *testing.T, ctx context.Context, api *client.Client, runID, cursor string, stop func(protocol.Event) bool) []protocol.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var events []protocol.Event
	done := errors.New("requested event reached")
	err := api.Watch(ctx, runID, cursor, func(event protocol.Event) error {
		events = append(events, event)
		if stop(event) {
			return done
		}
		return nil
	})
	if !errors.Is(err, done) {
		t.Fatalf("SSE replay failed: %v", err)
	}
	return events
}

func assertSinglePreparation(t *testing.T, run protocol.Run, events []protocol.Event) {
	t.Helper()
	var id string
	for _, instance := range run.Instances {
		if instance.NodeID == "prepare" {
			id = instance.ID
		}
	}
	running, succeeded := 0, 0
	for _, event := range events {
		if event.InstanceID == id {
			if event.Message == "running" {
				running++
			}
			if event.Message == "succeeded" {
				succeeded++
			}
		}
	}
	if id == "" || running != 1 || succeeded != 1 {
		t.Errorf("prepare execution duplicated or absent: running=%d succeeded=%d", running, succeeded)
	}
}
