package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

// Switching admission does not change the backend of a frozen run. Both workers,
// PostgreSQL, HTTP, command receipts, SSE replay and process termination are real.
func TestAdmittedBackendSurvivesAdmissionSwitch(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY and KNOTRA_TEST_DATABASE_URL for backend switching")
	}
	t.Setenv("KNOTRA_ROUTING_FIXTURE_KEY", "fixture-key")
	for _, firstBackend := range []string{execution.BackendTemporal, execution.BackendRiver} {
		t.Run(firstBackend, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"id":"fixture-model"}`)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"knotra_output","arguments":"{\"answer\":42}"}]}`)
			}))
			defer provider.Close()
			work := t.TempDir()
			profile := recoveryProfile()
			profile.Spec.Sandboxes = nil
			profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_ROUTING_FIXTURE_KEY"}}
			profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: provider.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "fixture"}}}}
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: firstBackend, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx),
				TemporalAddress: env("KNOTRA_TEST_TEMPORAL", "127.0.0.1:27233"), Namespace: "default", TaskQueue: "routing-" + uuid.NewString(),
				DataDir: filepath.Join(work, "engine"), Profiles: []string{profilePath}}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			process := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			// A provider reply reaches the host but its journal commit fails. The
			// resulting open resolution must survive switching admission too.
			if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION lose_routing_reply() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				IF NEW.completed THEN RAISE EXCEPTION 'fixture lost response journal'; END IF; RETURN NEW; END $$;
				CREATE TRIGGER lose_routing_reply BEFORE UPDATE ON knotra_operations FOR EACH ROW EXECUTE FUNCTION lose_routing_reply()`); err != nil {
				t.Fatal(err)
			}
			define := func(source string) string {
				var result struct{ Definition protocol.Definition }
				pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
				if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &result); err != nil {
					t.Fatal(err)
				}
				return result.Definition.ID
			}
			humanDefinition := define(routingHumanPipeline)
			modelDefinition := define(routingModelPipeline)
			var legacyRuns []string
			t.Cleanup(func() {
				if t.Failed() {
					cleanupWorkflows(t, options, legacyRuns)
				}
			})
			start := func(definition string) protocol.HumanRequest {
				var accepted struct{ Run protocol.Run }
				if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition, "profile": profile.Metadata.Name}, uuid.NewString(), &accepted); err != nil {
					t.Fatal(err)
				}
				backend, err := store.ReadRunBackend(ctx, admin.Pool, accepted.Run.ID)
				if err != nil || backend != options.Backend {
					t.Fatalf("admitted backend=%s want=%s error=%v", backend, options.Backend, err)
				}
				if backend == execution.BackendTemporal {
					legacyRuns = append(legacyRuns, accepted.Run.ID)
				}
				if definition == modelDefinition {
					for {
						status, err := store.ReadRunStatus(ctx, admin.Pool, accepted.Run.ID)
						if err != nil || protocol.Terminal(status) {
							t.Fatalf("resolution wait ended: status=%s error=%v", status, err)
						}
						if status == "waiting_resolution" {
							break
						}
						select {
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						case <-time.After(100 * time.Millisecond):
						}
					}
					var requestID string
					if err := admin.Pool.QueryRow(ctx, "SELECT id FROM knotra_requests WHERE run_id=$1 AND kind='resolution' AND status='open'", accepted.Run.ID).Scan(&requestID); err != nil {
						t.Fatal(err)
					}
					request, err := admin.Request(ctx, requestID)
					if err != nil {
						t.Fatal(err)
					}
					if request.Kind != "resolution" || request.Failure == nil || !request.Failure.Unknown {
						t.Fatalf("provider reply was not retained as uncertain: %+v", request)
					}
					return protocol.HumanRequest{ID: request.ID, RunID: request.RunID, InstanceID: request.InstanceID, Deadline: request.Deadline, Status: request.Status}
				}
				return awaitHuman(t, ctx, api, accepted.Run.ID)
			}
			first := []protocol.HumanRequest{start(humanDefinition), start(humanDefinition), start(modelDefinition)}
			prefix := readEventsUntil(t, ctx, api, first[0].RunID, "", func(event protocol.Event) bool {
				return event.InstanceID == first[0].InstanceID && event.Message == "waiting_human"
			})
			process.kill(t)
			options.Backend = execution.BackendRiver
			if firstBackend == execution.BackendRiver {
				options.Backend = execution.BackendTemporal
			}
			writeJSON(t, optionsPath, options)
			process = startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "mixed.log"), api)
			var info struct {
				Backend        string
				WorkerBackends []string
			}
			if err := api.Get(ctx, "/info", &info); err != nil || info.Backend != options.Backend || !reflect.DeepEqual(info.WorkerBackends, []string{execution.BackendRiver, execution.BackendTemporal}) {
				t.Fatalf("mixed workers=%+v error=%v", info, err)
			}
			for _, request := range first {
				recovered, err := admin.Request(ctx, request.ID)
				if err != nil || recovered.Status != "open" || !recovered.Deadline.Equal(request.Deadline) {
					t.Fatalf("recovered request=%+v error=%v", recovered, err)
				}
			}
			replayed := readEventsUntil(t, ctx, api, first[0].RunID, "", func(event protocol.Event) bool { return event.ID == prefix[len(prefix)-1].ID })
			if !reflect.DeepEqual(prefix, replayed) {
				t.Fatal("SSE prefix changed with admission backend")
			}
			second := []protocol.HumanRequest{start(humanDefinition), start(humanDefinition), start(modelDefinition)}
			for batchIndex, batch := range [][]protocol.HumanRequest{first, second} {
				wantBackend := firstBackend
				if batchIndex == 1 {
					wantBackend = options.Backend
				}
				for index, request := range batch {
					path, kind, expectedStatus := "/requests/"+request.ID+"/response", "human", "succeeded"
					var body any = map[string]any{"outputs": map[string]any{"approved": true}}
					switch index {
					case 1:
						path, kind, expectedStatus, body = "/runs/"+request.RunID+"/cancel", "cancel", "cancelled", map[string]any{}
					case 2:
						path, kind = "/runs/"+request.RunID+"/instances/"+request.InstanceID+"/resolve", "resolve"
						body = map[string]any{"outcome": "succeeded", "evidence": "fixture confirmed one physical generation", "outputs": map[string]any{"answer": 42}}
					}
					key := uuid.NewString()
					var receipt, replay json.RawMessage
					if err := api.Command(ctx, path, body, key, &receipt); err != nil {
						t.Fatal(err)
					}
					fresh := &client.Client{BaseURL: api.BaseURL, StateDir: t.TempDir()}
					if err := fresh.Command(ctx, path, body, key, &replay); err != nil || !bytes.Equal(receipt, replay) {
						t.Fatalf("receipt replay changed: %s / %s error=%v", receipt, replay, err)
					}
					final := awaitTerminal(t, ctx, api, request.RunID)
					if final.Status != expectedStatus || (index == 0 && string(final.Outputs["approved"]) != "true") || (index == 2 && string(final.Outputs["answer"]) != "42") {
						t.Fatalf("command %s result=%+v", kind, final)
					}
					backend, err := store.ReadRunBackend(ctx, admin.Pool, request.RunID)
					if err != nil || backend != wantBackend {
						t.Fatalf("frozen backend changed: %s want=%s error=%v", backend, wantBackend, err)
					}
					var commands int
					if err == nil {
						err = admin.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_outbox WHERE run_id=$1 AND kind=$2", request.RunID, kind).Scan(&commands)
					}
					wantCommands := 0
					if backend == execution.BackendTemporal {
						wantCommands = 1
					}
					if err != nil || commands != wantCommands {
						t.Fatalf("backend=%s kind=%s outbox=%d want=%d error=%v", backend, kind, commands, wantCommands, err)
					}
					if index == 2 {
						var debits, intents, attempts int
						if err := admin.Pool.QueryRow(ctx, `SELECT
							(SELECT COALESCE(sum(used),0) FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
							(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='model' AND NOT completed),
							(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND number=1)`, request.RunID).Scan(&debits, &intents, &attempts); err != nil {
							t.Fatal(err)
						}
						wantAttempts := 0
						if backend == execution.BackendRiver {
							wantAttempts = 1
						}
						if debits != 1 || intents != 1 || attempts != wantAttempts {
							t.Fatalf("resolution repeated work: backend=%s debits=%d unknownIntents=%d attempts=%d", backend, debits, intents, attempts)
						}
					}
					if index == 1 {
						err := api.Command(ctx, "/requests/"+request.ID+"/response", map[string]any{"outputs": map[string]any{"approved": true}}, uuid.NewString(), new(any))
						var rejected *client.HTTPError
						if !errors.As(err, &rejected) || rejected.Status != http.StatusConflict {
							t.Fatalf("answer resurrected cancelled run: %v", err)
						}
					}
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("backend switch repeated provider work: %d calls", calls.Load())
			}
			// Once legacy runs and outbox are drained, River-only startup must work
			// without contacting Temporal, even with retained legacy projections.
			for {
				var drained bool
				if err := admin.Pool.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM knotra_outbox WHERE sent_at IS NULL)").Scan(&drained); err != nil {
					t.Fatal(err)
				}
				if drained {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(100 * time.Millisecond):
				}
			}
			process.kill(t)
			options.Backend, options.TemporalAddress = execution.BackendRiver, "127.0.0.1:1"
			writeJSON(t, optionsPath, options)
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "drained.log"), api)
			if err := api.Get(ctx, "/info", &info); err != nil || !reflect.DeepEqual(info.WorkerBackends, []string{execution.BackendRiver}) {
				t.Fatalf("drained workers=%+v error=%v", info, err)
			}
		})
	}
}

const routingHumanPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: routing-human}
spec:
  nodes:
    review:
      type: human
      human: {prompt: {text: Approve}}
      outputs:
        approved: {schema: {type: boolean}}
  outputs:
    approved: {schema: {type: boolean}, bind: {from: nodes.review.outputs.approved}}
`

const routingModelPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: routing-model}
spec:
  models:
    writer: {connection: cloud, requires: [structuredOutput]}
  nodes:
    generate:
      type: llm
      llm: {model: writer, prompt: {text: Return answer 42.}}
      outputs:
        answer: {schema: {type: integer}}
  outputs:
    answer: {schema: {type: integer}, bind: {from: nodes.generate.outputs.answer}}
`
