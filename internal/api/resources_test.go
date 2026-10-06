package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michael-bill/knotra/internal/contract"
)

func TestResourceCatalogRecognizesCloudProvidersAndRequiresCredentials(t *testing.T) {
	t.Setenv("KNOTRA_CATALOG_KEY", "private-catalog-key")
	profile := contract.Profile{Spec: contract.ProfileSpec{
		Secrets: map[string]contract.SecretSource{"key": {Env: "KNOTRA_CATALOG_KEY"}, "missing": {Env: "KNOTRA_CATALOG_MISSING"}},
		Models: map[string]contract.ModelConnection{
			"local":           {Provider: "ollama"},
			"openai":          {Provider: "openai", Auth: map[string]contract.Credential{"key": {SecretRef: "key"}}},
			"anthropic":       {Provider: "anthropic", Auth: map[string]contract.Credential{"key": {SecretRef: "key"}}},
			"missing":         {Provider: "openai", Auth: map[string]contract.Credential{"key": {SecretRef: "missing"}}},
			"unauthenticated": {Provider: "anthropic"},
			"unknown":         {Provider: "unknown"},
		},
	}}
	t.Setenv("KNOTRA_CATALOG_MISSING", "")
	response := httptest.NewRecorder()
	(&Server{Profiles: map[string]contract.Profile{"cloud": profile}}).resources(response, httptest.NewRequestWithContext(t.Context(), "GET", "/v1/resources", nil))
	var body struct {
		Items []struct{ ID, Kind, Status string }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{"local": "available", "openai": "available", "anthropic": "available", "missing": "unavailable", "unauthenticated": "unavailable", "unknown": "unavailable"}
	seen := map[string]bool{}
	for _, item := range body.Items {
		if item.Kind == "model" {
			seen[item.ID] = true
		}
		if item.Kind == "model" && item.Status != expected[item.ID] {
			t.Errorf("%s: %s", item.ID, item.Status)
		}
	}
	if len(seen) != len(expected) {
		t.Fatal("catalog omitted configured models")
	}
	if strings.Contains(response.Body.String(), "private-catalog-key") {
		t.Fatal("catalog exposed credentials")
	}
}
