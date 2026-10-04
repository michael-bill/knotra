package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
)

const cloudReviewPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: cloud-review}
spec:
  models:
    writer: {connection: cloud, requires: [structuredOutput]}
  nodes:
    generate:
      type: llm
      llm: {model: writer, prompt: {text: Return answer 42.}}
      outputs:
        answer: {schema: {type: integer}}
    review:
      type: human
      inputs:
        answer: {schema: {type: integer}, bind: {from: nodes.generate.outputs.answer}}
      human: {prompt: {text: Approve the saved model answer.}}
      outputs:
        approved: {schema: {const: true}}
  outputs:
    answer: {schema: {type: integer}, bind: {from: nodes.generate.outputs.answer}}
    approved: {schema: {const: true}, bind: {from: nodes.review.outputs.approved}}
`

// The provider is a literal HTTP fixture; Temporal, PostgreSQL, CLI and the
// process restart are real. No API credentials or billable calls are needed.
func TestCloudProtocolsWithRealEngineRecovery(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" {
		t.Skip("set KNOTRA_TEST_RECOVERY=1 for provider fixture + real engine recovery")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOTRA_CLOUD_FIXTURE_KEY", "fixture-key")
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var generations atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprint(w, `{"id":"fixture-model"}`)
					return
				}
				generations.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if provider == "openai" {
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"knotra_output\",\"arguments\":\"{\\\"answer\\\":42}\"}]}}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"knotra_output\",\"input\":{\"answer\":42}}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			}))
			defer remote.Close()
			workRoot := env("KNOTRA_TEST_WORKDIR", filepath.Join(root, ".knotra", "integration-work"))
			if err := os.MkdirAll(workRoot, 0700); err != nil {
				t.Fatal(err)
			}
			work, err := os.MkdirTemp(workRoot, "cloud-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if !t.Failed() {
					_ = os.RemoveAll(work)
				} else {
					t.Logf("evidence retained at %s", work)
				}
			})
			profile := contract.Profile{APIVersion: "knotra/v1", Kind: "EngineProfile", Metadata: contract.Metadata{Name: "cloud"}, Spec: contract.ProfileSpec{
				Secrets: map[string]contract.SecretSource{"key": {Env: "KNOTRA_CLOUD_FIXTURE_KEY"}},
				Models:  map[string]contract.ModelConnection{"cloud": {Provider: provider, Model: "fixture-model", BaseURL: remote.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "key"}}}},
				Limits:  contract.Limits{Timeout: "3m", MaxConcurrentNodes: 2, MaxNodeInstances: 10, MaxModelCalls: 5, MaxToolCalls: 5},
			}}
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: env("KNOTRA_TEST_TEMPORAL", "127.0.0.1:27233"), Namespace: "default", TaskQueue: "cloud-" + uuid.NewString(), DataDir: filepath.Join(work, "engine"), HelperPath: snapshotHelper(t, root, work), Profiles: []string{profilePath}, Version: "cloud-test"}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			first := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
			var definition struct {
				Definition protocol.Definition `json:"definition"`
			}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": contract.Package{Entrypoint: "pipeline.yaml", Source: cloudReviewPipeline, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(cloudReviewPipeline)}}}}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var accepted struct {
				Run protocol.Run `json:"run"`
			}
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": "cloud"}, uuid.NewString(), &accepted); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if t.Failed() {
					cleanupWorkflows(t, options, []string{accepted.Run.ID})
				}
			})
			before := awaitHuman(t, ctx, api, accepted.Run.ID)
			first.kill(t)
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			after := awaitHuman(t, ctx, api, accepted.Run.ID)
			if before.ID != after.ID {
				t.Fatal("human request was replaced on recovery")
			}
			if _, err := executeCLI(ctx, api, "requests", "respond", after.ID, "--output", "approved=true"); err != nil {
				t.Fatal(err)
			}
			final := awaitTerminal(t, ctx, api, accepted.Run.ID)
			if final.Status != "succeeded" || string(final.Outputs["answer"]) != "42" || generations.Load() != 1 {
				t.Fatalf("recovery status=%s calls=%d diagnostics=%v", final.Status, generations.Load(), final.Diagnostics)
			}
			data, err := json.Marshal(final)
			if err != nil || len(data) == 0 {
				t.Fatal("completed run is not readable")
			}
		})
	}
}
