// Package api exposes the engine protocol. Mutations are durable commands;
// disconnected HTTP clients never own workflow lifetime.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

const maxRequestBytes = 256 << 20

var operationPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Server struct {
	Store      *store.Store
	Artifacts  store.Artifacts
	Profiles   map[string]contract.Profile
	Token      string
	Version    string
	CORSOrigin string
	Admit      func(context.Context, *contract.Plan) error
	Log        *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.info)
	mux.HandleFunc("GET /v1/profiles", s.profiles)
	mux.HandleFunc("GET /v1/resources", s.resources)
	mux.HandleFunc("POST /v1/packages/validate", s.validate)
	mux.HandleFunc("POST /v1/definitions", s.define)
	mux.HandleFunc("GET /v1/definitions", s.definitions)
	mux.HandleFunc("GET /v1/definitions/{id}", s.definition)
	mux.HandleFunc("POST /v1/runs", s.start)
	mux.HandleFunc("GET /v1/runs", s.runs)
	mux.HandleFunc("GET /v1/runs/{id}", s.run)
	mux.HandleFunc("POST /v1/runs/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /v1/runs/{id}/resume", s.resume)
	mux.HandleFunc("POST /v1/runs/{id}/instances/{instance}/resolve", s.resolve)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.events)
	mux.HandleFunc("GET /v1/requests", s.requests)
	mux.HandleFunc("POST /v1/requests/{id}/response", s.respond)
	mux.HandleFunc("POST /v1/artifacts", s.upload)
	mux.HandleFunc("GET /v1/artifacts", s.artifacts)
	mux.HandleFunc("GET /v1/artifacts/{id}", s.artifact)
	mux.HandleFunc("GET /v1/artifacts/{id}/content", s.content)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if origin := r.Header.Get("Origin"); origin != "" {
			if s.CORSOrigin == "" || origin != s.CORSOrigin {
				s.fail(w, 403, "PERMISSION_DENIED", "origin is not allowed", nil)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, Last-Event-ID")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.Token != "" {
			token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !bearer || subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) != 1 {
				s.fail(w, 401, "UNAUTHENTICATED", "valid bearer token required", nil)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) principal() string {
	if s.Token == "" {
		return "local"
	}
	h := sha256.Sum256([]byte(s.Token))
	return "token-" + hex.EncodeToString(h[:16])
}
func (s *Server) write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil && s.Log != nil {
		s.Log.Debug("response write failed", "error", err)
	}
}
func (s *Server) fail(w http.ResponseWriter, status int, code, message string, diags []contract.Diagnostic) {
	if diags == nil {
		diags = []contract.Diagnostic{}
	}
	s.write(w, status, protocol.Error{Code: code, Message: message, Diagnostics: diags})
}
func (s *Server) err(w http.ResponseWriter, e error) {
	if errors.Is(e, store.ErrNotFound) {
		s.fail(w, 404, "NOT_FOUND", "object not found", nil)
	} else if errors.Is(e, store.ErrConflict) {
		s.fail(w, 409, "OPERATION_CONFLICT", "operation conflicts with existing state", nil)
	} else {
		s.fail(w, 503, "UNAVAILABLE", "engine storage or service is unavailable", nil)
		if s.Log != nil {
			s.Log.Error("engine request failed", "error", e)
		}
	}
}
func readBody(w http.ResponseWriter, r *http.Request, v any) ([]byte, error) {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if e != nil {
		return nil, e
	}
	decoded, e := contract.DecodeJSONLimit(b, maxRequestBytes)
	if e != nil {
		return nil, e
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if e = dec.Decode(v); e != nil {
		return nil, e
	}
	// Deterministic field ordering while retaining number lexemes for the command digest.
	var normalized any
	dec = json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if e = dec.Decode(&normalized); e != nil {
		return nil, e
	}
	return json.Marshal(normalized)
}
func (s *Server) command(w http.ResponseWriter, r *http.Request, payload []byte, fn func(pgx.Tx) (int, any, error)) {
	key := r.Header.Get("Idempotency-Key")
	if !operationPattern.MatchString(key) {
		s.fail(w, 400, "IDEMPOTENCY_KEY_REQUIRED", "provide Idempotency-Key (1–128 letters, digits, _ or -)", nil)
		return
	}
	status, b, e := s.Store.Command(r.Context(), s.principal(), key, r.Method+" "+r.URL.EscapedPath(), payload, func(tx pgx.Tx) (int, any, error) {
		nested, err := tx.Begin(r.Context())
		if err != nil {
			return 0, nil, err
		}
		status, value, err := fn(nested)
		if err == nil {
			err = nested.Commit(r.Context())
			return status, value, err
		}
		if rollback := nested.Rollback(r.Context()); rollback != nil {
			return 0, nil, rollback
		}
		code, message := "", ""
		var validation *store.ValidationError
		switch {
		case errors.Is(err, store.ErrConflict):
			status, code, message = 409, "OPERATION_CONFLICT", "operation conflicts with existing state"
		case errors.Is(err, store.ErrNotFound):
			status, code, message = 404, "NOT_FOUND", "object not found"
		case errors.As(err, &validation):
			status, code, message = 422, "INPUT_INVALID", validation.Message
		default:
			return 0, nil, err
		}
		return status, protocol.Error{Code: code, Message: message, Diagnostics: []contract.Diagnostic{}}, nil
	})
	if e != nil {
		s.err(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	s.write(w, 200, map[string]any{"protocol": protocol.Version, "engineId": s.Store.EngineID, "principalId": s.principal(), "version": s.Version, "capabilities": []string{"validate", "definitions", "runs", "events", "human", "artifacts", "resolution"}})
}
