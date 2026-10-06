// Package executor hosts one admitted leaf attempt without a workflow SDK.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/michael-bill/knotra/internal/adapters"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
	"github.com/michael-bill/knotra/internal/telemetry"
)

type Host struct {
	Store     *store.Store
	Artifacts store.Artifacts
	Runner    *adapters.Runner
	Log       *slog.Logger
}

func (h *Host) Execute(ctx context.Context, q execution.ExecuteRequest) execution.ExecuteResult {
	ctx, span := otel.Tracer("knotra/engine").Start(ctx, "node."+q.Node.Type)
	defer span.End()
	span.SetAttributes(attribute.String("knotra.run.id", q.RunID), attribute.String("knotra.instance.id", q.InstanceID), attribute.Int("knotra.attempt", q.Attempt))
	var loadErr error
	q.Plan, loadErr = h.Store.Plan(ctx, q.RunID)
	if loadErr != nil {
		return execution.ExecuteResult{Failure: &execution.Failure{Code: "STORAGE_UNAVAILABLE", Message: "cannot load admitted plan", Retryable: true}}
	}
	backend, err := store.ReadRunBackend(ctx, h.Store.Pool, q.RunID)
	if err != nil {
		return execution.ExecuteResult{Failure: &execution.Failure{Code: "STORAGE_UNAVAILABLE", Message: "cannot load admitted backend", Retryable: true}}
	}
	if (backend == execution.BackendRiver) != (q.Ownership != nil) || (q.Ownership != nil && q.Ownership.AttemptID != (execution.AttemptID{RunID: q.RunID, InstanceID: q.InstanceID, Number: q.Attempt})) {
		return execution.ExecuteResult{Failure: &execution.Failure{Code: "OWNERSHIP_REQUIRED", Message: "execution token does not match the admitted backend and attempt"}}
	}
	if q.Ownership != nil {
		span.SetAttributes(telemetry.OwnershipAttributes(*q.Ownership)...)
		span.SetAttributes(attribute.String("knotra.engine.id", h.Store.EngineID), attribute.String("knotra.host.id", h.Runner.HostID))
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, q.Deadline)
		defer cancel()
	}
	// Run sessions retain these hooks for cleanup, so keep only journal/owner
	// identity and budgets, not the loaded plan or potentially large inputs.
	hooks := &executionHooks{store: h.Store, artifacts: h.Artifacts, request: execution.ExecuteRequest{
		RunID: q.RunID, InstanceID: q.InstanceID, Attempt: q.Attempt,
		Ownership: q.Ownership, Scopes: q.Scopes,
	}}
	if q.Ownership != nil {
		hooks.log = telemetry.OwnershipLogger(h.Log, *q.Ownership)
	}
	scopeID := q.RunID
	if len(q.Scopes) > 0 {
		scopeID = q.Scopes[len(q.Scopes)-1].ID
	}
	outputs, e := h.Runner.WithHooks(hooks).Execute(
		ctx,
		adapters.Request{
			RunID:         q.RunID,
			InstanceID:    q.InstanceID,
			Attempt:       q.Attempt,
			Pipeline:      q.Pipeline,
			ScopeID:       scopeID,
			Plan:          &q.Plan,
			Node:          q.Node,
			Inputs:        q.Inputs,
			ToolArguments: q.ToolArguments,
		},
	)
	if e == nil {
		return execution.ExecuteResult{Outputs: outputs}
	}
	// Adapters can classify transport errors without retaining their cancellation
	// cause. The owned context still proves that execution was interrupted.
	if q.Ownership != nil && (ctx.Err() != nil || errors.Is(e, store.ErrExecutionOwnership) || errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded)) {
		return execution.ExecuteResult{Failure: &execution.Failure{Code: "OUTCOME_UNKNOWN", Message: e.Error(), Unknown: true,
			OperationID: fmt.Sprintf("%s/attempt/%d", q.InstanceID, q.Attempt), CanRetryIfNotExecuted: q.Node.Type != "agent"}}
	}
	var f *adapters.Failure
	if errors.As(e, &f) {
		return execution.ExecuteResult{Failure: &execution.Failure{
			Code:                  f.Code,
			Message:               f.Message,
			Retryable:             f.Retryable,
			Unknown:               f.Unknown,
			OperationID:           f.OperationID,
			CanRetryIfNotExecuted: q.Node.Type != "agent",
		}}
	}
	code := "EXECUTION_FAILED"
	if strings.Contains(e.Error(), "BUDGET_EXCEEDED") {
		code = "BUDGET_EXCEEDED"
	}
	if errors.Is(e, context.Canceled) {
		code = "CANCELLED"
	}
	if errors.Is(e, context.DeadlineExceeded) {
		code = "TIMEOUT"
	}
	return execution.ExecuteResult{Failure: &execution.Failure{Code: code, Message: e.Error()}}
}

type executionHooks struct {
	store     *store.Store
	artifacts store.Artifacts
	request   execution.ExecuteRequest
	pending   *adapters.Operation
	log       *slog.Logger
}

func (h *executionHooks) Observe(ctx context.Context, event adapters.ExecutionEvent) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return h.store.Observe(ctx, protocol.Event{
		RunID: h.request.RunID, InstanceID: h.request.InstanceID,
		AttemptID:   fmt.Sprintf("%s.a%d", h.request.InstanceID, h.request.Attempt),
		OperationID: event.OperationID, Type: event.Type, Message: event.Type,
		Data: event.Data,
	})
}

func (h *executionHooks) Reserve(ctx context.Context, kind string) error {
	if h.request.Ownership != nil {
		if h.pending == nil || h.pending.Kind != kind {
			return errors.New("physical call requires a matching prepared operation")
		}
		err := h.store.AdmitOperation(ctx, *h.request.Ownership, h.pending.ID, kind)
		telemetry.Event(ctx, h.log, "operation.admission", err, attribute.String("knotra.operation.id", h.pending.ID))
		return err
	}
	return h.store.Reserve(ctx, h.request.RunID, kind, h.request.Scopes)
}

func (h *executionHooks) PutArtifact(ctx context.Context, name, mime string, b []byte) (contract.Artifact, error) {
	origin := map[string]string{"runId": h.request.RunID, "instanceId": h.request.InstanceID, "attemptId": fmt.Sprintf("%s.a%d", h.request.InstanceID, h.request.Attempt)}
	if h.request.Ownership != nil {
		return h.store.PutOwnedArtifact(ctx, *h.request.Ownership, h.artifacts, name, mime, b, origin)
	}
	return h.artifacts.Put(ctx, name, mime, b, origin)
}

func (h *executionHooks) GetArtifact(ctx context.Context, id string) ([]byte, error) {
	return h.artifacts.Get(ctx, id)
}

func (h *executionHooks) BeginOperation(ctx context.Context, op adapters.Operation) (adapters.OperationState, error) {
	var s store.OperationState
	var e error
	if h.request.Ownership != nil {
		s, e = h.store.BeginOwnedOperation(ctx, *h.request.Ownership, op.ID, op.Kind, op.Effect)
		if e == nil && !s.Started && !s.Completed && op.Kind != "" {
			h.pending = &op
		}
	} else {
		s, e = h.store.BeginOperation(ctx, op.ID, h.request.RunID, op.Kind, op.Effect)
	}
	telemetry.Event(ctx, h.log, "operation.intent", e, attribute.String("knotra.operation.id", op.ID))
	return adapters.OperationState{Started: s.Started, Completed: s.Completed, Response: s.Response}, e
}

func (h *executionHooks) CompleteOperation(ctx context.Context, id string, b json.RawMessage) error {
	var err error
	if h.request.Ownership != nil {
		err = h.store.CompleteOwnedOperation(ctx, *h.request.Ownership, id, b)
	} else {
		err = h.store.CompleteOperation(ctx, id, b)
	}
	telemetry.Event(ctx, h.log, "operation.confirmation", err, attribute.String("knotra.operation.id", id))
	return err
}

func (h *executionHooks) RegisterResource(ctx context.Context, id, kind, lifetime string) (execution.ResourceRecord, error) {
	if h.request.Ownership != nil {
		return h.store.RegisterOwnedResource(ctx, *h.request.Ownership, id, kind, lifetime)
	}
	return execution.ResourceRecord{ID: id, EngineID: h.store.EngineID, Kind: kind, Lifetime: lifetime}, nil
}

func (h *executionHooks) CloseResource(ctx context.Context, id string) error {
	if h.request.Ownership != nil {
		return h.store.CloseOwnedResource(ctx, *h.request.Ownership, id)
	}
	return nil
}

func (h *executionHooks) RecordMCPSession(ctx context.Context, id string, session execution.MCPSessionRecord) error {
	if h.request.Ownership != nil {
		return h.store.RecordOwnedMCPSession(ctx, *h.request.Ownership, id, session)
	}
	return nil
}
