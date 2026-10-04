package adapters

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

// Paid calls require a separate explicit opt-in, even when credentials happen
// to be present in a contributor's environment. No default model is guessed.
func TestCloudRealStructuredAndAgent(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_CLOUD") != "1" {
		t.Skip("set KNOTRA_TEST_CLOUD=1 to enable paid provider checks")
	}
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			prefix := strings.ToUpper(provider)
			key, model := os.Getenv(prefix+"_API_KEY"), os.Getenv("KNOTRA_TEST_"+prefix+"_MODEL")
			if key == "" || model == "" {
				t.Skip("set " + prefix + "_API_KEY and KNOTRA_TEST_" + prefix + "_MODEL")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			req := cloudRequest(provider, os.Getenv("KNOTRA_TEST_"+prefix+"_BASE_URL"))
			connection := req.Plan.Profile.Spec.Models["local"]
			connection.Model = model
			connection.Auth["key"] = contract.Credential{SecretRef: "provider_key"}
			req.Plan.Profile.Spec.Models["local"] = connection
			req.Plan.Profile.Spec.Secrets = map[string]contract.SecretSource{"provider_key": {Env: prefix + "_API_KEY"}}
			req.Plan.Pipelines[req.Pipeline].Spec.Sandboxes = nil
			runner := &Runner{Hooks: newHooks()}
			if err := runner.Prepare(ctx, req.Plan); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				values, err := runner.Execute(ctx, req)
				if err != nil || string(values["answer"].JSON) != "42" {
					t.Fatalf("structured output: %v %v", values, err)
				}
			}
			t.Run("agent", func(t *testing.T) {
				agentRunner := integrationRunner(t)
				req.InstanceID = "live-agent"
				req.Plan.Pipelines[req.Pipeline].Spec.Sandboxes = testRequest("").Plan.Pipelines[req.Pipeline].Spec.Sandboxes
				req.Node.Type, req.Node.Sandbox, req.Node.LLM = "agent", "box", nil
				req.Node.Agent = &contract.AgentNode{Model: "model", Prompt: contract.TextSource{Text: "Immediately call knotra_finish with the JSON argument {\"answer\":42}."}, MaxSteps: 3}
				if err := agentRunner.Prepare(ctx, req.Plan); err != nil {
					t.Fatal(err)
				}
				if _, err := agentRunner.Execute(ctx, req); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestOllamaRealStructuredWithoutDocker(t *testing.T) {
	endpoint := os.Getenv("KNOTRA_TEST_OLLAMA")
	if endpoint == "" {
		t.Skip("set KNOTRA_TEST_OLLAMA for local model checks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req := testRequest(endpoint)
	req.Plan.Pipelines[req.Pipeline].Spec.Sandboxes = nil
	connection := req.Plan.Profile.Spec.Models["local"]
	connection.Parameters = map[string]any{"think": false, "temperature": 0, "num_predict": 256}
	req.Plan.Profile.Spec.Models["local"] = connection
	hooks := newHooks()
	runner := &Runner{Hooks: hooks}
	if err := runner.Prepare(ctx, req.Plan); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		values, err := runner.Execute(ctx, req)
		if err != nil || string(values["answer"].JSON) != "42" {
			t.Fatalf("actual model: %v %v", values, err)
		}
	}
	if hooks.calls["model"] != 1 {
		t.Fatal("replay consumed another model call")
	}
}
