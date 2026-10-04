package starter_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/michael-bill/knotra/internal/contract"
)

// These are user-facing starting points, not notation fixtures: every one must
// compile with the bundled local profile and run without extra inputs or secrets.
func TestStartersCompileWithLocalProfileAndDefaults(t *testing.T) {
	data, err := os.ReadFile("../local/profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	profile, diags := contract.ParseProfile(data)
	if contract.HasErrors(diags) {
		t.Fatal(diags)
	}
	for _, name := range []string{"hello", "research-dossier", "tic-tac-toe", "publication"} {
		t.Run(name, func(t *testing.T) {
			pkg, err := contract.LoadPackage(filepath.Join(name, "pipeline.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			plan, diags := contract.Compile(pkg, &profile)
			if contract.HasErrors(diags) {
				t.Fatal(diags)
			}
			pipeline := plan.Pipelines[plan.Root]
			if _, err := contract.ValidatePorts(pipeline.Spec.Inputs, contract.Values{}, true); err != nil {
				t.Fatalf("starter requires non-default inputs: %v", err)
			}
			if len(pipeline.Spec.MCP) != 0 || len(pipeline.Spec.Secrets) != 0 {
				t.Fatal("local starters must not require MCP or secrets")
			}
			for _, model := range pipeline.Spec.Models {
				if model.Connection != "model_main" {
					t.Fatalf("unexpected model connection: %s", model.Connection)
				}
			}
			for _, sandbox := range pipeline.Spec.Sandboxes {
				expected := "python_box"
				if name == "tic-tac-toe" {
					expected = "node_box"
				}
				if sandbox.Profile != expected {
					t.Fatalf("unexpected sandbox profile: %s", sandbox.Profile)
				}
			}
			artifacts := 0
			for _, output := range pipeline.Spec.Outputs {
				if output.Artifact != nil {
					artifacts++
				}
			}
			if artifacts == 0 {
				t.Fatal("starter must publish a usable downloadable artifact")
			}
		})
	}
}
