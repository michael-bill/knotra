package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

type ValidateRequest struct {
	Package   contract.Package           `json:"package"`
	Profile   string                     `json:"profile"`
	Inputs    map[string]json.RawMessage `json:"inputs"`
	Artifacts map[string]json.RawMessage `json:"artifacts"`
}

func (s *Server) compile(
	ctx context.Context,
	query store.Querier,
	pkg contract.Package,
	profile string,
	input map[string]json.RawMessage,
	artifacts map[string]json.RawMessage,
) (*contract.Plan, contract.Values, []contract.Diagnostic) {
	p, ok := s.Profiles[profile]
	if !ok {
		return nil, nil, []contract.Diagnostic{{
			Severity: "error",
			Code:     "REFERENCE_INVALID",
			Phase:    "admission",
			Message:  "unknown engine profile",
			Path:     "/profile",
		}}
	}
	plan, diags := contract.Compile(pkg, &p)
	if contract.HasErrors(diags) {
		return plan, nil, diags
	}
	values := contract.Values{}

	for k, v := range input {
		values[k] = contract.Value{JSON: v}
	}

	for name, b := range artifacts {
		if _, dup := values[name]; dup {
			diags = append(diags, diag("INPUT_INVALID", "input is supplied as both JSON and artifact", "/inputs/"+name))
			continue
		}
		v, e := store.ArtifactValue(ctx, query, b)
		if e != nil {
			diags = append(diags, diag("INPUT_INVALID", "artifact binding must contain registered IDs", "/artifacts/"+name))
			continue
		}

		values[name] = v
	}

	if !contract.HasErrors(diags) {
		var e error
		values, e = contract.ValidatePorts(plan.Pipelines[plan.Root].Spec.Inputs, values, true)
		if e != nil {
			diags = append(diags, diag("INPUT_INVALID", e.Error(), "/inputs"))
		}
	}
	if !contract.HasErrors(diags) && s.Admit != nil {
		if e := s.Admit(ctx, plan); e != nil {
			diags = append(diags, diag("CAPABILITY_UNSUPPORTED", e.Error(), "/profile"))
		}
	}
	return plan, values, diags
}

func diag(code, message, path string) contract.Diagnostic {
	return contract.Diagnostic{Severity: "error", Code: code, Phase: "admission", Message: message, Path: path}
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var q ValidateRequest
	if _, e := readBody(w, r, &q); e != nil {
		s.fail(w, 400, "INPUT_INVALID", e.Error(), nil)
		return
	}
	_, _, d := s.compile(r.Context(), s.Store.Pool, q.Package, q.Profile, q.Inputs, q.Artifacts)
	if d == nil {
		d = []contract.Diagnostic{}
	}
	s.write(w, 200, map[string]any{"valid": !contract.HasErrors(d), "diagnostics": d})
}

func (s *Server) define(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Package contract.Package `json:"package"`
	}
	b, e := readBody(w, r, &q)
	if e != nil {
		s.fail(w, 400, "INPUT_INVALID", e.Error(), nil)
		return
	}
	s.command(w, r, b, func(tx pgx.Tx) (int, any, error) {
		plan, d := contract.Compile(q.Package, nil)
		if contract.HasErrors(d) {
			return 422, protocol.Error{Code: "VALIDATION_FAILED", Message: "package failed validation", Diagnostics: d}, nil
		}
		p := plan.Pipelines[plan.Root]
		title := p.Metadata.Title
		if title == "" {
			title = p.Metadata.Name
		}
		def := protocol.Definition{
			ID:            uuid.NewString(),
			Name:          p.Metadata.Name,
			Title:         title,
			PackageDigest: plan.Digest,
			CreatedAt:     time.Now().UTC(),
			Package:       q.Package,
		}
		def, e := store.PutDefinition(r.Context(), tx, def)
		return 201, map[string]any{"definition": def}, e
	})
}

func (s *Server) definitions(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Definitions(r.Context(), r.URL.Query().Get("cursor"))
	if e != nil {
		s.err(w, e)
		return
	}
	s.write(w, 200, page(v, func(d protocol.Definition) string { return d.ID }))
}

func (s *Server) definition(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Definition(r.Context(), r.PathValue("id"))
	if e != nil {
		s.err(w, e)
		return
	}
	s.write(w, 200, map[string]any{"definition": v})
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var q struct {
		DefinitionID string                     `json:"definitionId"`
		Profile      string                     `json:"profile"`
		Inputs       map[string]json.RawMessage `json:"inputs"`
		Artifacts    map[string]json.RawMessage `json:"artifacts"`
	}
	b, e := readBody(w, r, &q)
	if e != nil {
		s.fail(w, 400, "INPUT_INVALID", e.Error(), nil)
		return
	}
	s.command(w, r, b, func(tx pgx.Tx) (int, any, error) {
		def, e := store.ReadDefinition(r.Context(), tx, q.DefinitionID)
		if e != nil {
			return 0, nil, e
		}
		plan, values, d := s.compile(r.Context(), tx, def.Package, q.Profile, q.Inputs, q.Artifacts)
		if contract.HasErrors(d) {
			return 422, protocol.Error{Code: "VALIDATION_FAILED", Message: "run admission failed", Diagnostics: d}, nil
		}
		run := protocol.Run{
			ID:               uuid.NewString(),
			DefinitionID:     def.ID,
			Title:            def.Title,
			Status:           "pending",
			CreatedAt:        time.Now().UTC(),
			UpdatedAt:        time.Now().UTC(),
			Profile:          q.Profile,
			Package:          def.Package,
			Inputs:           map[string]json.RawMessage{},
			InputArtifacts:   map[string]any{},
			Outputs:          map[string]json.RawMessage{},
			Artifacts:        []contract.Artifact{},
			Instances:        []protocol.Instance{},
			Diagnostics:      []contract.Diagnostic{},
			AvailableActions: []string{"cancel"},
		}

		for k, v := range values {
			if v.JSON != nil {
				run.Inputs[k] = v.JSON
			} else if v.Collection {
				ids := []string{}

				for _, a := range v.Artifacts {
					ids = append(ids, a.ID)
				}

				run.InputArtifacts[k] = ids
			} else if len(v.Artifacts) == 1 {
				run.InputArtifacts[k] = v.Artifacts[0].ID
			}
		}

		e = store.PutRun(r.Context(), tx, run, *plan, values)
		return 201, map[string]any{"run": run}, e
	})
}
