package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store/db"
)

func (s *Store) Project(ctx context.Context, p execution.Projection) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if e = s.ProjectTx(ctx, tx, p); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// ProjectTx writes an idempotent observation inside an existing transaction.
// It acquires the run lock before touching instances, artifacts or requests.
func (s *Store) ProjectTx(ctx context.Context, tx pgx.Tx, p execution.Projection) error {
	var b []byte
	var seq int64
	storedRunProjection, e := db.New(tx).LockRunProjection(ctx, p.RunID)
	if e == nil {
		b = storedRunProjection.Document
		seq = storedRunProjection.Sequence
	}
	if e != nil {
		return e
	}
	var run protocol.Run
	if e = json.Unmarshal(b, &run); e != nil {
		return e
	}
	var duplicate bool
	duplicate, e = db.New(tx).EventSequenceExists(ctx, db.EventSequenceExistsParams{RunID: p.RunID, Sequence: new(p.Sequence)})
	if e != nil {
		return e
	}
	if duplicate {
		return nil
	}
	switch {
	case p.InstanceID != "" && p.Kind == "node":
		if p.Status == "succeeded" {
			for _, value := range p.Outputs {
				for _, artifact := range value.Artifacts {
					if _, e = db.New(tx).PublishArtifact(ctx, db.PublishArtifactParams{ID: artifact.ID, OriginRunID: p.RunID}); e != nil {
						return e
					}
				}
			}
		}
		inst := protocol.Instance{
			ID: p.InstanceID, NodeID: p.NodeID, Scope: p.Pipeline, Status: p.Status,
			ParentInstanceID: p.ParentInstanceID, IterationIndex: p.IterationIndex,
			GraphPath: p.GraphPath, NodeType: p.NodeType,
			StartedAt: p.StartedAt, FinishedAt: p.FinishedAt, UpdatedAt: &p.Time,
			Reason: shortEventText(p.Reason), DataTruncated: p.DataTruncated,
		}
		var inputTruncated, outputTruncated bool
		inst.Inputs, inputTruncated = observationContext(p.Inputs)
		inst.Outputs, outputTruncated = observationContext(p.Outputs)
		inst.DataTruncated = inst.DataTruncated || inputTruncated || outputTruncated
		if p.Attempt > 0 {
			inst.AttemptID = fmt.Sprintf("%s.a%d", p.InstanceID, p.Attempt)
		}
		if p.Failure != nil {
			d := diagnostic(p.Failure)
			inst.Error = &d
		}
		ib, e := raw(inst)
		if e != nil {
			return e
		}
		_, e = db.New(tx).UpsertInstanceProjection(ctx, db.UpsertInstanceProjectionParams{RunID: p.RunID, ID: p.InstanceID, Sequence: p.Sequence, Document: ib})
		if e != nil {
			return e
		}
	case p.Kind == "diagnostic" && p.Failure != nil:
		run.Diagnostics = append(run.Diagnostics, diagnostic(p.Failure))
	case p.Kind == "run" && p.InstanceID == "" && p.Sequence > seq && !protocol.Terminal(run.Status):
		run.Status = p.Status
		if p.Outputs != nil {
			run.Outputs = map[string]json.RawMessage{}
			run.Artifacts = []contract.Artifact{}

			for _, key := range sortedKeys(p.Outputs) {
				v := p.Outputs[key]
				if v.JSON != nil {
					run.Outputs[key] = v.JSON
				} else {
					run.Artifacts = append(run.Artifacts, v.Artifacts...)
				}
			}
		}
		if p.Failure != nil {
			run.Diagnostics = append(run.Diagnostics, diagnostic(p.Failure))
		}

	}
	switch {
	case protocol.Terminal(run.Status):
		if _, e = db.New(tx).CancelOpenRequests(ctx, p.RunID); e != nil {
			return e
		}
		run.AvailableActions = []string{}
	case run.Status == "waiting_resolution":
		run.AvailableActions = []string{"cancel", "resolve"}
	default:
		run.AvailableActions = []string{"cancel"}

	}
	if p.Time.After(run.UpdatedAt) {
		run.UpdatedAt = p.Time
	}
	b, e = raw(run)
	if e != nil {
		return e
	}
	// Only root projections order root state; independent instance events must not
	// suppress a root event that was delivered after a concurrent activity.
	if p.InstanceID == "" && p.Sequence > seq {
		seq = p.Sequence
	}
	_, e = db.New(tx).UpdateRunProjection(ctx, db.UpdateRunProjectionParams{ID: p.RunID, Document: b, Sequence: seq})
	if e != nil {
		return e
	}
	data := map[string]any{
		"status": p.Status, "reason": shortEventText(p.Reason),
		"nodeId": p.NodeID, "scope": p.Pipeline,
		"parentInstanceId": p.ParentInstanceID, "iterationIndex": p.IterationIndex,
		"graphPath": p.GraphPath, "nodeType": p.NodeType,
	}
	ev := protocol.Event{
		RunID:      p.RunID,
		At:         p.Time,
		Type:       p.Kind,
		Message:    p.Status,
		InstanceID: p.InstanceID,
		Data:       data,
	}
	if p.Kind == "node" {
		inputs, inputTruncated := observationContext(p.Inputs)
		outputs, outputTruncated := observationContext(p.Outputs)
		if inputs != nil {
			data["inputs"] = inputs
		}
		if outputs != nil {
			data["outputs"] = outputs
		}
		data["dataTruncated"] = p.DataTruncated || inputTruncated || outputTruncated
		if p.StartedAt != nil {
			data["startedAt"] = p.StartedAt
		}
		if p.FinishedAt != nil {
			data["finishedAt"] = p.FinishedAt
		}
	}
	if p.Failure != nil {
		ev.Message = shortEventText(p.Failure.Message)
	}
	if p.Attempt > 0 {
		ev.AttemptID = fmt.Sprintf("%s.a%d", p.InstanceID, p.Attempt)
	}
	b, e = raw(ev)
	if e != nil {
		return e
	}
	_, e = db.New(tx).InsertExecutionEvent(ctx, db.InsertExecutionEventParams{RunID: p.RunID, Sequence: new(p.Sequence), Document: b})
	if e != nil {
		return e
	}
	return nil
}

func diagnostic(f *execution.Failure) contract.Diagnostic {
	return contract.Diagnostic{Severity: "error", Code: f.Code, Phase: "runtime", Message: f.Message, Path: ""}
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))

	for x := range m {
		k = append(k, x)
	}

	sort.Strings(k)
	return k
}

func (s *Store) SaveRequest(ctx context.Context, r execution.Request) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.SaveRequestTx(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) SaveRequestTx(ctx context.Context, tx pgx.Tx, r execution.Request) error {
	run, cancelling, err := lockRun(ctx, tx, r.RunID)
	if err != nil {
		return err
	}
	if (cancelling || protocol.Terminal(run.Status)) && (r.Status == "open" || r.Status == "pending") {
		r.Status = "cancelled"
	}
	b, err := raw(r)
	if err != nil {
		return err
	}
	_, err = db.New(tx).UpsertRequest(ctx, db.UpsertRequestParams{ID: r.ID, RunID: r.RunID, Kind: r.Kind, Status: r.Status, Document: b})
	if err != nil {
		return err
	}
	return nil
}

func (s *Store) Request(ctx context.Context, id string) (execution.Request, error) {
	var r execution.Request

	storedRequest, e := db.New(s.Pool).ReadRequest(ctx, id)

	if e == nil {
		e = json.Unmarshal(storedRequest.Document, &r)
		r.Status = storedRequest.Status
	}
	return r, classify(e)
}

func (s *Store) Requests(ctx context.Context, cursor string) ([]protocol.HumanRequest, error) {
	rows, e := db.New(s.Pool).ListHumanRequests(ctx, db.ListHumanRequestsParams{Cursor: cursor, MaxBytes: MaxListPageBytes})
	if e != nil {
		return nil, e
	}

	out := []protocol.HumanRequest{}
	size := 0

	for _, record := range rows {
		var r execution.Request
		var b []byte
		var v protocol.HumanRequest
		b = record.Document
		v.Status = record.Status
		if v.Status == "accepted" {
			// River distinguishes consumed answers internally; the desktop
			// protocol exposes both reserved and consumed answers as answered.
			v.Status = "answered"
		}
		v.CreatedAt = record.CreatedAt
		if e = json.Unmarshal(b, &r); e != nil {
			return nil, e
		}
		v.ID = r.ID
		v.RunID = r.RunID
		v.InstanceID = r.InstanceID
		v.AttemptID = r.InstanceID + ".a1"
		v.Prompt = r.Prompt
		v.Deadline = r.Deadline
		v.Inputs = contract.ContextEnvelope(r.Inputs)
		v.ResponseSchema = contract.PortObjectSchema(r.Outputs)
		out = append(out, v)
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		size += len(encoded)
		if len(out) > 1 && size > MaxListPageBytes {
			break
		}
	}
	return out, nil
}

func (s *Store) Reserve(ctx context.Context, runID, kind string, scopes []execution.BudgetScope) error {
	if kind != "model" && kind != "tool" {
		return fmt.Errorf("unknown budget kind %q", kind)
	}
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	backend, e := ReadRunBackend(ctx, tx, runID)
	if e != nil {
		return e
	}
	if backend != execution.BackendTemporal {
		return ErrExecutionOwnership
	}
	_, e = db.New(tx).LockTransactionKey(ctx, "budget:"+runID)
	if e != nil {
		return e
	}

	if err := reserveCalls(ctx, tx, runID, kind, scopes); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func reserveCalls(ctx context.Context, tx pgx.Tx, runID, kind string, scopes []execution.BudgetScope) error {
	for _, scope := range scopes {
		limit := scope.Limits.MaxModelCalls
		if kind == "tool" {
			limit = scope.Limits.MaxToolCalls
		}
		var used int64
		used, e := db.New(tx).ReadBudgetUsed(ctx, db.ReadBudgetUsedParams{RunID: runID, Scope: scope.ID, Kind: kind})
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if limit <= 0 || used >= int64(limit) {
			return fmt.Errorf("BUDGET_EXCEEDED: %s in scope %s", kind, scope.ID)
		}
		_, e = db.New(tx).IncrementBudget(ctx, db.IncrementBudgetParams{RunID: runID, Scope: scope.ID, Kind: kind})
		if e != nil {
			return e
		}
	}
	return nil
}

type OperationState struct {
	Started   bool
	Completed bool
	Response  json.RawMessage
}

func (s *Store) BeginOperation(ctx context.Context, id, runID, kind, effect string) (OperationState, error) {
	tag, e := db.New(s.Pool).InsertLegacyOperation(ctx, db.InsertLegacyOperationParams{ID: id, RunID: runID, Kind: kind, Effect: effect})
	if e != nil {
		return OperationState{}, e
	}
	if tag.RowsAffected() == 1 {
		return OperationState{}, nil
	}
	var state OperationState
	state.Started = true
	storedLegacyOperation, queryErr5 := db.New(s.Pool).ReadLegacyOperation(ctx, db.ReadLegacyOperationParams{ID: id, RunID: runID})
	e = queryErr5
	if e == nil {
		state.Completed = storedLegacyOperation.Completed
		state.Response = storedLegacyOperation.Response
	}
	if errors.Is(e, pgx.ErrNoRows) {
		return OperationState{}, ErrExecutionOwnership
	}
	return state, e
}

func (s *Store) CompleteOperation(ctx context.Context, id string, response json.RawMessage) error {
	tag, e := db.New(s.Pool).CompleteLegacyOperation(ctx, db.CompleteLegacyOperationParams{ID: id, Response: []byte(response)})
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		var b []byte
		b, e = db.New(s.Pool).ReadLegacyOperationResponse(ctx, id)
		if e != nil {
			return e
		}
		if string(b) != string(response) {
			return ErrConflict
		}
	}
	return nil
}

// Answer shares the same row lock as Respond. A timer cannot discard an answer
// already committed before its deadline, even if outbox delivery was delayed.
func (s *Store) Answer(ctx context.Context, q execution.AnswerRequest) (*execution.HumanSignal, error) {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, _, e = lockRun(ctx, tx, q.RunID); e != nil {
		return nil, e
	}

	storedHumanAnswer, queryErr7 := db.New(tx).LockHumanAnswer(ctx, db.LockHumanAnswerParams{ID: q.RequestID, RunID: q.RunID})
	e = queryErr7

	if e != nil {
		return nil, classify(e)
	}
	if storedHumanAnswer.Status == "answered" && storedHumanAnswer.ResponseID != nil && storedHumanAnswer.AcceptedAt != nil {
		var values contract.Values
		if e = json.Unmarshal(storedHumanAnswer.Response, &values); e != nil {
			return nil, e
		}
		return &execution.HumanSignal{
			RequestID:  q.RequestID,
			ResponseID: *storedHumanAnswer.ResponseID,
			Values:     values,
			AcceptedAt: *storedHumanAnswer.AcceptedAt,
		}, tx.Commit(ctx)
	}
	if q.CloseIfAbsent != "" {
		if q.CloseIfAbsent != "cancelled" && q.CloseIfAbsent != "expired" {
			return nil, ErrConflict
		}
		_, e = db.New(tx).CloseRequest(ctx, db.CloseRequestParams{ID: q.RequestID, Status: q.CloseIfAbsent})
		if e != nil {
			return nil, e
		}
	}
	return nil, tx.Commit(ctx)
}

func shortEventText(s string) string {
	runes := []rune(s)
	if len(runes) > 4096 {
		return string(runes[:4096]) + "…"
	}
	return s
}
