package api

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Run(r.Context(), r.PathValue("id"))
	if e != nil {
		s.err(w, e)
		return
	}
	s.write(w, 200, map[string]any{"run": v})
}

func page[T any](items []T, id func(T) string) protocol.Page[T] {
	p := protocol.Page[T]{Items: items}
	size := 0

	for i, item := range items {
		b, _ := json.Marshal(item)
		if i > 0 && (i >= 100 || size+len(b) > store.MaxListPageBytes) {
			p.Items = items[:i]
			cursor := id(items[i-1])
			p.NextCursor = &cursor
			break
		}
		size += len(b)
	}

	return p
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Runs(r.Context(), r.URL.Query().Get("cursor"))
	if e != nil {
		s.err(w, e)
		return
	}
	s.write(w, 200, page(v, func(x protocol.Run) string { return x.ID }))
}

func (s *Server) requests(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Requests(r.Context(), r.URL.Query().Get("cursor"))
	if e != nil {
		s.err(w, e)
		return
	}
	s.write(w, 200, page(v, func(x protocol.HumanRequest) string { return x.ID }))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	var q struct{}
	b, e := readBody(w, r, &q)
	if e != nil {
		s.fail(w, 400, "INPUT_INVALID", e.Error(), nil)
		return
	}
	id := r.PathValue("id")
	s.command(w, r, b, func(tx pgx.Tx) (int, any, error) {
		e := store.Cancel(r.Context(), tx, id, s.Wake)
		return 202, map[string]any{"accepted": true, "runId": id}, e
	})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	var q struct{}
	b, e := readBody(w, r, &q)
	if e != nil {
		s.fail(w, 400, "INPUT_INVALID", e.Error(), nil)
		return
	}
	s.command(w, r, b, func(_ pgx.Tx) (int, any, error) {
		return 409, protocol.Error{
			Code:        "UNSAFE_ACTION",
			Message:     "no suspended checkpoint is available; use the advertised resolution action",
			Diagnostics: []contract.Diagnostic{},
		}, nil
	})
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Outputs map[string]json.RawMessage `json:"outputs"`
	}
	b, e := readBody(w, r, &q)
	if e != nil {
		s.fail(w, 400, "INPUT_INVALID", e.Error(), nil)
		return
	}
	id := r.PathValue("id")
	s.command(w, r, b, func(tx pgx.Tx) (int, any, error) {
		values := contract.Values{}

		for k, v := range q.Outputs {
			values[k] = contract.Value{JSON: v}
		}

		e := store.Respond(r.Context(), tx, id, r.Header.Get("Idempotency-Key"), values, s.Wake)
		if e != nil {
			return 0, nil, e
		}
		return 200, map[string]any{"accepted": true, "requestId": id}, nil
	})
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Outcome  string                     `json:"outcome"`
		Evidence string                     `json:"evidence"`
		Outputs  map[string]json.RawMessage `json:"outputs"`
	}
	b, e := readBody(w, r, &q)
	if e != nil {
		s.fail(w, 400, "INPUT_INVALID", e.Error(), nil)
		return
	}
	id, instance := r.PathValue("id"), r.PathValue("instance")
	s.command(w, r, b, func(tx pgx.Tx) (int, any, error) {
		decisions := map[string]string{"succeeded": "completed", "not_started": "not_executed", "failed": "failed"}
		decision, ok := decisions[q.Outcome]
		if !ok {
			return 422, protocol.Error{
				Code:        "INPUT_INVALID",
				Message:     "invalid resolution outcome",
				Diagnostics: []contract.Diagnostic{},
			}, nil
		}
		outputs := contract.Values{}

		for k, v := range q.Outputs {
			outputs[k] = contract.Value{JSON: v}
		}

		e = store.Resolve(r.Context(), tx, id, instance, r.Header.Get("Idempotency-Key"), decision, q.Evidence, outputs, s.Wake)
		return 202, map[string]any{"accepted": true, "runId": id}, e
	})
}
