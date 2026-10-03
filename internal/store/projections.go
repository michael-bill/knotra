package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/engine"
	"github.com/michael-bill/knotra/internal/protocol"
)

func (s *Store) Project(ctx context.Context, p engine.Projection) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var b []byte
	var seq int64
	e = tx.QueryRow(ctx, "SELECT document,sequence FROM knotra_runs WHERE id=$1 FOR UPDATE", p.RunID).Scan(&b, &seq)
	if e != nil {
		return e
	}
	var run protocol.Run
	if e = json.Unmarshal(b, &run); e != nil {
		return e
	}
	var duplicate bool
	e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM knotra_events WHERE run_id=$1 AND sequence=$2)", p.RunID, p.Sequence).Scan(&duplicate)
	if e != nil {
		return e
	}
	if duplicate {
		return nil
	}
	if p.InstanceID != "" && p.Kind == "node" {
		if p.Status == "succeeded" {
			for _, value := range p.Outputs {
				for _, artifact := range value.Artifacts {
					if _, e = tx.Exec(ctx, "UPDATE knotra_artifacts SET published=true WHERE id=$1 AND document->'origin'->>'runId'=$2", artifact.ID, p.RunID); e != nil {
						return e
					}
				}
			}
		}
		inst := protocol.Instance{ID: p.InstanceID, NodeID: p.NodeID, Scope: p.Pipeline, Status: p.Status}
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
		_, e = tx.Exec(ctx, "INSERT INTO knotra_instances(run_id,id,sequence,document) VALUES($1,$2,$3,$4) ON CONFLICT(run_id,id) DO UPDATE SET sequence=EXCLUDED.sequence,document=EXCLUDED.document WHERE knotra_instances.sequence<EXCLUDED.sequence", p.RunID, p.InstanceID, p.Sequence, ib)
		if e != nil {
			return e
		}

	} else if p.Kind == "run" && p.InstanceID == "" && p.Sequence > seq && !protocol.Terminal(run.Status) {
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
	if protocol.Terminal(run.Status) {
		if _, e = tx.Exec(ctx, "UPDATE knotra_requests SET status='cancelled' WHERE run_id=$1 AND status IN ('open','pending')", p.RunID); e != nil {
			return e
		}
		run.AvailableActions = []string{}
	} else if run.Status == "waiting_resolution" {
		run.AvailableActions = []string{"cancel", "resolve"}
	} else {
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
	_, e = tx.Exec(ctx, "UPDATE knotra_runs SET document=$2,sequence=$3 WHERE id=$1", p.RunID, b, seq)
	if e != nil {
		return e
	}
	ev := protocol.Event{RunID: p.RunID, At: p.Time, Type: p.Kind, Message: p.Status, InstanceID: p.InstanceID, Data: map[string]any{"status": p.Status, "reason": shortEventText(p.Reason)}}
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
	_, e = tx.Exec(ctx, "INSERT INTO knotra_events(run_id,sequence,document) VALUES($1,$2,$3)", p.RunID, p.Sequence, b)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func diagnostic(f *engine.Failure) contract.Diagnostic {
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
func (s *Store) SaveRequest(ctx context.Context, r engine.Request) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
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
	_, err = tx.Exec(ctx, `INSERT INTO knotra_requests(id,run_id,kind,status,document) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(id) DO UPDATE SET status=EXCLUDED.status,document=EXCLUDED.document
 WHERE knotra_requests.status IN ('open','pending') AND EXCLUDED.status NOT IN ('open','pending')`, r.ID, r.RunID, r.Kind, r.Status, b)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) Request(ctx context.Context, id string) (engine.Request, error) {
	var r engine.Request
	var b []byte
	var status string
	e := s.Pool.QueryRow(ctx, "SELECT document,status FROM knotra_requests WHERE id=$1", id).Scan(&b, &status)
	if e == nil {
		e = json.Unmarshal(b, &r)
		r.Status = status
	}
	return r, classify(e)
}
func (s *Store) Requests(ctx context.Context, cursor string) ([]protocol.HumanRequest, error) {
	rows, e := s.Pool.Query(ctx, "SELECT document,status,created_at FROM knotra_requests WHERE kind='human' AND ($1='' OR id<$1) ORDER BY id DESC LIMIT 101", cursor)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []protocol.HumanRequest{}
	size := 0
	for rows.Next() {
		var r engine.Request
		var b []byte
		var v protocol.HumanRequest
		if e = rows.Scan(&b, &v.Status, &v.CreatedAt); e != nil {
			return nil, e
		}
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
	return out, rows.Err()
}

func (s *Store) Reserve(ctx context.Context, runID, kind string, scopes []engine.BudgetScope) error {
	if kind != "model" && kind != "tool" {
		return fmt.Errorf("unknown budget kind %q", kind)
	}
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "budget:"+runID)
	if e != nil {
		return e
	}
	for _, scope := range scopes {
		limit := scope.Limits.MaxModelCalls
		if kind == "tool" {
			limit = scope.Limits.MaxToolCalls
		}
		var used int64
		e = tx.QueryRow(ctx, "SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$2 AND kind=$3", runID, scope.ID, kind).Scan(&used)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if limit <= 0 || used >= int64(limit) {
			return fmt.Errorf("BUDGET_EXCEEDED: %s in scope %s", kind, scope.ID)
		}
		_, e = tx.Exec(ctx, "INSERT INTO knotra_budgets(run_id,scope,kind,used) VALUES($1,$2,$3,1) ON CONFLICT(run_id,scope,kind) DO UPDATE SET used=knotra_budgets.used+1", runID, scope.ID, kind)
		if e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

type OperationState struct {
	Started   bool
	Completed bool
	Response  json.RawMessage
}

func (s *Store) BeginOperation(ctx context.Context, id, runID, kind, effect string) (OperationState, error) {
	tag, e := s.Pool.Exec(ctx, "INSERT INTO knotra_operations(id,run_id,kind,effect) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", id, runID, kind, effect)
	if e != nil {
		return OperationState{}, e
	}
	if tag.RowsAffected() == 1 {
		return OperationState{}, nil
	}
	var state OperationState
	state.Started = true
	e = s.Pool.QueryRow(ctx, "SELECT completed,response FROM knotra_operations WHERE id=$1 AND run_id=$2", id, runID).Scan(&state.Completed, &state.Response)
	return state, e
}
func (s *Store) CompleteOperation(ctx context.Context, id string, response json.RawMessage) error {
	tag, e := s.Pool.Exec(ctx, "UPDATE knotra_operations SET completed=true,response=$2 WHERE id=$1 AND NOT completed", id, []byte(response))
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		var b []byte
		e = s.Pool.QueryRow(ctx, "SELECT response FROM knotra_operations WHERE id=$1 AND completed", id).Scan(&b)
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
func (s *Store) Answer(ctx context.Context, q engine.AnswerRequest) (*engine.HumanSignal, error) {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if _, _, e = lockRun(ctx, tx, q.RunID); e != nil {
		return nil, e
	}
	var status string
	var responseID *string
	var response []byte
	var acceptedAt *time.Time
	e = tx.QueryRow(ctx, "SELECT status,response_id,response,accepted_at FROM knotra_requests WHERE id=$1 AND run_id=$2 FOR UPDATE", q.RequestID, q.RunID).Scan(&status, &responseID, &response, &acceptedAt)
	if e != nil {
		return nil, classify(e)
	}
	if status == "answered" && responseID != nil && acceptedAt != nil {
		var values contract.Values
		if e = json.Unmarshal(response, &values); e != nil {
			return nil, e
		}
		return &engine.HumanSignal{RequestID: q.RequestID, ResponseID: *responseID, Values: values, AcceptedAt: *acceptedAt}, tx.Commit(ctx)
	}
	if q.CloseIfAbsent != "" {
		if q.CloseIfAbsent != "cancelled" && q.CloseIfAbsent != "expired" {
			return nil, ErrConflict
		}
		_, e = tx.Exec(ctx, "UPDATE knotra_requests SET status=$2 WHERE id=$1 AND status IN ('open','pending')", q.RequestID, q.CloseIfAbsent)
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
