package api

import (
	"encoding/base64"
	"mime"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Name      string  `json:"name"`
		MediaType string  `json:"mediaType"`
		Content   *string `json:"content"`
	}
	b, e := readBody(w, r, &q)
	if e != nil {
		s.fail(w, 400, "INPUT_INVALID", e.Error(), nil)
		return
	}
	var content []byte
	if q.Content != nil {
		content, e = base64.StdEncoding.DecodeString(*q.Content)
		if e != nil {
			s.fail(w, 400, "INPUT_INVALID", "content must be a base64 string", nil)
			return
		}
	}
	s.command(w, r, b, func(tx pgx.Tx) (int, any, error) {
		mt, params, mediaErr := mime.ParseMediaType(q.MediaType)
		if q.Name == "" || len(q.Name) > 1024 ||
			mediaErr != nil || len(params) > 0 || !strings.Contains(mt, "/") || strings.Contains(mt, "*") ||
			q.Content == nil || len(content) > store.MaxArtifactBytes {
			return 422, protocol.Error{
				Code:        "INPUT_INVALID",
				Message:     "name, mediaType and content <=64 MiB required",
				Diagnostics: []contract.Diagnostic{},
			}, nil
		}
		art, e := s.Artifacts.Write(q.Name, q.MediaType, content, map[string]string{})
		if e != nil {
			return 0, nil, e
		}
		e = store.RegisterArtifact(r.Context(), tx, art)
		return 201, map[string]any{"artifact": art}, e
	})
}

func (s *Server) artifacts(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Artifacts(r.Context(), r.URL.Query().Get("cursor"))
	if e != nil {
		s.err(w, e)
		return
	}
	s.write(w, 200, page(v, func(x contract.Artifact) string { return x.ID }))
}

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Artifact(r.Context(), r.PathValue("id"))
	if e != nil {
		s.err(w, e)
		return
	}
	s.write(w, 200, map[string]any{"artifact": v})
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Artifact(r.Context(), r.PathValue("id"))
	if e != nil {
		s.err(w, e)
		return
	}
	b, e := s.Artifacts.Get(r.Context(), v.ID)
	if e != nil {
		s.err(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("ETag", `"`+v.SHA256+`"`)
	_, _ = w.Write(b)
}
