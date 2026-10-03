package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/michael-bill/knotra/internal/contract"
	"net/http"
	"os"
	"slices"
)

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	items := []map[string]any{}
	for _, id := range keys(s.Profiles) {
		p := s.Profiles[id]
		b, _ := json.Marshal(p)
		h := sha256.Sum256(b)
		items = append(items, map[string]any{"id": id, "title": p.Metadata.Title, "revision": hex.EncodeToString(h[:])})
	}
	s.write(w, 200, map[string]any{"items": items})
}
func (s *Server) resources(w http.ResponseWriter, r *http.Request) {
	items := []map[string]any{}
	for _, profileID := range keys(s.Profiles) {
		p := s.Profiles[profileID]
		for _, kind := range []string{"model", "mcp", "sandbox", "secret"} {
			var ids []string
			switch kind {
			case "model":
				ids = keys(p.Spec.Models)
			case "mcp":
				ids = keys(p.Spec.MCP)
			case "sandbox":
				ids = keys(p.Spec.Sandboxes)
			case "secret":
				ids = keys(p.Spec.Secrets)
			}
			for _, id := range ids {
				available := true
				capabilities := []string{}
				credentialsReady := func(values map[string]contract.Credential) bool {
					for _, v := range values {
						if v.SecretRef != "" {
							source, ok := p.Spec.Secrets[v.SecretRef]
							if !ok || os.Getenv(source.Env) == "" {
								return false
							}
						}
					}
					return true
				}
				switch kind {
				case "model":
					m := p.Spec.Models[id]
					available = m.Provider == "ollama" && credentialsReady(m.Auth)
				case "mcp":
					m := p.Spec.MCP[id]
					available = credentialsReady(m.Headers) && credentialsReady(m.Env)
					capabilities = append(capabilities, m.AllowedTools...)
				case "sandbox":
					capabilities = append(capabilities, p.Spec.Sandboxes[id].AllowedTools...)
				case "secret":
					available = os.Getenv(p.Spec.Secrets[id].Env) != ""
				}
				status := "available"
				if !available {
					status = "unavailable"
				}
				items = append(items, map[string]any{"id": id, "kind": kind, "title": profileID + " / " + id, "capabilities": capabilities, "status": status})
			}
		}
	}
	s.write(w, 200, map[string]any{"items": items})
}
func keys[V any](m map[string]V) []string {
	a := make([]string, 0, len(m))
	for k := range m {
		a = append(a, k)
	}
	slices.Sort(a)
	return a
}
