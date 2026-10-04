// Package adapters implements external work. It neither schedules graphs nor owns
// durable state; the engine supplies transactional budgets and operation journals.
package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/michael-bill/knotra/internal/contract"
)

// Version identifies the execution ABI frozen by admission. Change it when a
// saved plan can no longer be executed with the same adapter semantics.
const Version = "knotra-adapters/v1"

type Request struct {
	RunID, InstanceID, Pipeline, ScopeID string
	Attempt                              int
	Plan                                 *contract.Plan
	Node                                 contract.Node
	Inputs                               contract.Values
	ToolArguments                        json.RawMessage
	service                              bool
	observation                          *executionObservation
}

type Operation struct{ ID, Kind, Effect, IdempotencyKey string }

type OperationState struct {
	Started, Completed bool
	Response           json.RawMessage
}

// Hooks must atomically record intent before work and persist responses before
// acknowledging success. A journal entry must never be shared across runs.
type Hooks interface {
	Reserve(context.Context, string) error
	PutArtifact(context.Context, string, string, []byte) (contract.Artifact, error)
	GetArtifact(context.Context, string) ([]byte, error)
	BeginOperation(context.Context, Operation) (OperationState, error)
	CompleteOperation(context.Context, string, json.RawMessage) error
}

// Failure carries execution decisions without coupling adapters to a workflow SDK.
type Failure struct {
	Code, Message, OperationID string
	Retryable, Unknown         bool
}

func (e *Failure) Error() string { return e.Code + ": " + e.Message }

func failure(code string, err error) error { return &Failure{Code: code, Message: err.Error()} }

type Runner struct {
	Hooks                                          Hooks
	HTTPClient                                     *http.Client
	DockerHost, HelperPath, WorkDir, FirewallImage string
	EngineID                                       string
	LookupEnv                                      func(string) (string, bool)
	mu                                             sync.Mutex
	shared                                         *sessionCache
}

type sessionCache struct {
	mu       sync.Mutex
	sessions map[string]*mcpSession
}

func (r *Runner) cache() *sessionCache {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shared == nil {
		r.shared = &sessionCache{sessions: map[string]*mcpSession{}}
	}
	return r.shared
}

// WithHooks creates an execution-scoped runner while retaining shared run MCP
// sessions. Configure a runner before its first use; never mutate it concurrently.
func (r *Runner) WithHooks(h Hooks) *Runner {
	return &Runner{
		Hooks:         h,
		HTTPClient:    r.HTTPClient,
		DockerHost:    r.DockerHost,
		HelperPath:    r.HelperPath,
		WorkDir:       r.WorkDir,
		FirewallImage: r.FirewallImage,
		EngineID:      r.EngineID,
		LookupEnv:     r.LookupEnv,
		shared:        r.cache(),
	}
}

func (r *Runner) httpClient() *http.Client {
	client := &http.Client{}
	if r.HTTPClient != nil {
		*client = *r.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func (r *Runner) Execute(ctx context.Context, req Request) (contract.Values, error) {
	if r.Hooks == nil || req.Plan == nil {
		return nil, fmt.Errorf("adapters: hooks and plan are required")
	}
	// Direct in-process callers can construct plans without admission metadata.
	// Persisted engine plans always have a version and must never drift silently.
	if version := req.Plan.Runtime.AdapterVersion; version != "" && version != Version {
		return nil, failure(
			"ADAPTER_VERSION_UNSUPPORTED",
			fmt.Errorf("admitted plan requires adapter version %q; worker has %q", version, Version),
		)
	}
	req.observation = r.newObservation(ctx, req)
	defer req.observation.finish(ctx)

	switch req.Node.Type {
	case "llm":
		return r.llm(ctx, req)
	case "agent":
		return r.agent(ctx, req)
	case "code":
		return r.code(ctx, req)
	case "tool":
		return r.tool(ctx, req)
	default:
		return nil, fmt.Errorf("adapters: unsupported leaf type %q", req.Node.Type)
	}
}

func (r *Runner) secret(p contract.Profile, name string) (string, error) {
	s, ok := p.Spec.Secrets[name]
	if !ok {
		return "", fmt.Errorf("unknown secret reference %q", name)
	}
	lookup := r.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, ok := lookup(s.Env)
	if !ok || value == "" {
		return "", fmt.Errorf("secret %q is unavailable", name)
	}
	return value, nil
}

func (r *Runner) credential(p contract.Profile, c contract.Credential) (string, error) {
	if c.Value != nil {
		return *c.Value, nil
	}
	return r.secret(p, c.SecretRef)
}

func operationID(req Request, name string, perAttempt bool) string {
	id := req.RunID + "/" + req.InstanceID + "/" + name
	if perAttempt {
		id += fmt.Sprintf("/attempt/%d", req.Attempt)
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func (r *Runner) operation(ctx context.Context, op Operation, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	state, err := r.Hooks.BeginOperation(ctx, op)
	if err != nil {
		return nil, err
	}
	if state.Completed {
		return state.Response, nil
	}
	if state.Started {
		safeRetry := op.Effect == "read" || op.IdempotencyKey != ""
		return nil, &Failure{
			Code:        "OUTCOME_UNKNOWN",
			Message:     "operation was started without a durable response",
			OperationID: op.ID,
			Unknown:     !safeRetry,
			Retryable:   safeRetry,
		}
	}
	if op.Kind != "" {
		if err = r.Hooks.Reserve(ctx, op.Kind); err != nil {
			return nil, err
		}
	}
	result, err := fn()
	if err != nil {
		var typed *Failure
		if errors.As(err, &typed) {
			copy := *typed
			if copy.OperationID == "" {
				copy.OperationID = op.ID
			}
			return nil, &copy
		}
		// Transport failure after sending cannot prove absence of external effects.
		safeRetry := op.Effect == "read" || op.IdempotencyKey != ""
		return nil, &Failure{
			Code:        "EXTERNAL_CALL_FAILED",
			Message:     err.Error(),
			OperationID: op.ID,
			Unknown:     !safeRetry,
			Retryable:   safeRetry,
		}
	}
	if err = r.Hooks.CompleteOperation(ctx, op.ID, result); err != nil {
		return nil, &Failure{
			Code:        "OUTCOME_UNKNOWN",
			Message:     "cannot persist external response",
			OperationID: op.ID,
			Unknown:     true,
		}
	}
	return result, nil
}

func pipeline(req Request) *contract.Pipeline { return req.Plan.Pipelines[req.Pipeline] }

func textSource(req Request, t contract.TextSource) (string, error) {
	if t.File == "" {
		return t.Text, nil
	}

	for _, f := range req.Plan.Package.Files {
		if f.Path == t.File {
			return string(f.Content), nil
		}
	}

	return "", fmt.Errorf("package text %q is missing", t.File)
}

func (r *Runner) Close() error {
	cache := r.cache()
	cache.mu.Lock()
	defer cache.mu.Unlock()

	for k, s := range cache.sessions {
		_ = s.close()
		delete(cache.sessions, k)
	}

	return nil
}

// ReleaseRun closes shared MCP sessions when a run reaches a terminal state.
func (r *Runner) ReleaseRun(runID string) {
	cache := r.cache()
	cache.mu.Lock()
	defer cache.mu.Unlock()

	for key, s := range cache.sessions {
		if strings.HasPrefix(key, runID+"/") {
			_ = s.close()
			delete(cache.sessions, key)
		}
	}
}
