package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/engine"
	"github.com/michael-bill/knotra/internal/protocol"
)

// ValidationError describes rejected user data, as opposed to a storage failure.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// lockRun establishes the lock order used by projections, cancellation and answers:
// run first, then request. It also serializes new requests against cancellation.
func lockRun(ctx context.Context, tx pgx.Tx, id string) (protocol.Run, bool, error) {
	var run protocol.Run
	var b []byte
	var cancelling bool
	err := tx.QueryRow(ctx, "SELECT document,cancel_requested FROM knotra_runs WHERE id=$1 FOR UPDATE", id).Scan(&b, &cancelling)
	if err == nil {
		err = json.Unmarshal(b, &run)
	}
	return run, cancelling, classify(err)
}
func Cancel(ctx context.Context, tx pgx.Tx, id string) error {
	run, cancelling, err := lockRun(ctx, tx, id)
	if err != nil {
		return err
	}
	if protocol.Terminal(run.Status) {
		return ErrConflict
	}
	if cancelling {
		return nil
	}
	if _, err = tx.Exec(ctx, "UPDATE knotra_runs SET cancel_requested=true WHERE id=$1", id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE knotra_requests SET status='cancelled' WHERE run_id=$1 AND status IN ('open','pending')", id); err != nil {
		return err
	}
	return Enqueue(ctx, tx, id, "cancel", engine.CancelSignal{Reason: "requested by operator"})
}

// Respond reserves the first valid answer and its durable delivery together.
func Respond(ctx context.Context, tx pgx.Tx, id, responseID string, values contract.Values) error {
	var runID string
	if err := tx.QueryRow(ctx, "SELECT run_id FROM knotra_requests WHERE id=$1", id).Scan(&runID); err != nil {
		return classify(err)
	}
	run, cancelling, err := lockRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if cancelling || protocol.Terminal(run.Status) {
		return ErrConflict
	}
	var b []byte
	var status string
	if err = tx.QueryRow(ctx, "SELECT document,status FROM knotra_requests WHERE id=$1 FOR UPDATE", id).Scan(&b, &status); err != nil {
		return classify(err)
	}
	var request engine.Request
	if err = json.Unmarshal(b, &request); err != nil {
		return err
	}
	accepted := time.Now().UTC()
	if status != "open" || request.Kind != "human" || !request.Deadline.After(accepted) {
		return ErrConflict
	}
	values, err = contract.ValidatePorts(request.Outputs, values, false)
	if err != nil {
		return &ValidationError{err.Error()}
	}
	response, err := raw(values)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE knotra_requests SET status='answered',response_id=$2,response=$3,accepted_at=$4 WHERE id=$1", id, responseID, response, accepted); err != nil {
		return err
	}
	return Enqueue(ctx, tx, runID, "human", engine.HumanSignal{RequestID: id, ResponseID: responseID, Values: values, AcceptedAt: accepted})
}

// Resolve accepts only the currently open, matching resolution. It never sends
// arbitrary instance/operation IDs into a workflow or silently ignores bad data.
func Resolve(ctx context.Context, tx pgx.Tx, runID, instanceID, responseID, decision, evidence string, outputs contract.Values) error {
	run, cancelling, err := lockRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if cancelling || protocol.Terminal(run.Status) {
		return ErrConflict
	}
	var b []byte
	err = tx.QueryRow(ctx, `SELECT document FROM knotra_requests WHERE run_id=$1 AND kind='resolution' AND status='open' AND document->>'instanceId'=$2 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, runID, instanceID).Scan(&b)
	if err != nil {
		return classify(err)
	}
	var req engine.Request
	if err = json.Unmarshal(b, &req); err != nil {
		return err
	}
	if req.Failure == nil || !req.Deadline.After(time.Now()) {
		return ErrConflict
	}
	if strings.TrimSpace(evidence) == "" {
		return &ValidationError{"resolution requires evidence"}
	}
	switch decision {
	case "completed":
		for key, value := range outputs {
			if req.Outputs[key].Artifact != nil && value.JSON != nil {
				resolved, e := ArtifactValue(ctx, tx, value.JSON)
				if e != nil {
					return e
				}
				outputs[key] = resolved
			}
		}
		outputs, err = contract.ValidatePorts(req.Outputs, outputs, false)
		if err != nil {
			return &ValidationError{err.Error()}
		}
	case "not_executed", "failed":
		if len(outputs) != 0 {
			return &ValidationError{"outputs are only accepted for succeeded outcomes"}
		}
	default:
		return &ValidationError{fmt.Sprintf("invalid resolution decision %q", decision)}
	}
	accepted := time.Now().UTC()
	signal := engine.ResolutionSignal{AcceptedAt: accepted, InstanceID: instanceID, OperationID: req.Failure.OperationID, ResponseID: responseID, Decision: decision, Evidence: evidence, Outputs: outputs}
	response, err := raw(signal)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE knotra_requests SET status='resolved',response_id=$2,response=$3,accepted_at=$4 WHERE id=$1", req.ID, responseID, response, accepted); err != nil {
		return err
	}
	return Enqueue(ctx, tx, runID, "resolve", signal)
}

// Resolution arbitrates an accepted operator decision against a deadline or
// cancellation using the same lock order and transaction as the HTTP command.
func (s *Store) Resolution(ctx context.Context, q engine.AnswerRequest) (*engine.ResolutionSignal, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, _, err = lockRun(ctx, tx, q.RunID); err != nil {
		return nil, err
	}
	var status string
	var b []byte
	var accepted *time.Time
	err = tx.QueryRow(ctx, "SELECT status,response,accepted_at FROM knotra_requests WHERE id=$1 AND run_id=$2 AND kind='resolution' FOR UPDATE", q.RequestID, q.RunID).Scan(&status, &b, &accepted)
	if err != nil {
		return nil, classify(err)
	}
	if status == "resolved" && accepted != nil {
		var signal engine.ResolutionSignal
		if err = json.Unmarshal(b, &signal); err != nil {
			return nil, err
		}
		signal.AcceptedAt = *accepted
		return &signal, tx.Commit(ctx)
	}
	if q.CloseIfAbsent != "" {
		if q.CloseIfAbsent != "cancelled" && q.CloseIfAbsent != "expired" {
			return nil, ErrConflict
		}
		if _, err = tx.Exec(ctx, "UPDATE knotra_requests SET status=$2 WHERE id=$1 AND status IN ('open','pending')", q.RequestID, q.CloseIfAbsent); err != nil {
			return nil, err
		}
	}
	return nil, tx.Commit(ctx)
}
