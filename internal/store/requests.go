package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store/db"
)

// ValidationError describes rejected user data, as opposed to a storage failure.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// lockRun establishes the lock order used by projections, cancellation and answers:
// run first, then request. It also serializes new requests against cancellation.
func lockRun(ctx context.Context, tx pgx.Tx, id string) (protocol.Run, bool, error) {
	var run protocol.Run

	storedRunCommand, err := db.New(tx).LockRunCommand(ctx, id)

	if err == nil {
		err = json.Unmarshal(storedRunCommand.Document, &run)
	}
	return run, storedRunCommand.CancelRequested, classify(err)
}

func Cancel(ctx context.Context, tx pgx.Tx, id string, wake ...EnqueueWake) error {
	inner, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = inner.Rollback(ctx) }()
	tx = inner
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
	if _, err = db.New(tx).SetRunCancelled(ctx, id); err != nil {
		return err
	}
	if _, err = db.New(tx).CancelOpenRequests(ctx, id); err != nil {
		return err
	}
	if err := enqueueControl(ctx, tx, id, "cancel", execution.CancelSignal{Reason: "requested by operator"}, wake); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Respond reserves the first valid answer and its durable delivery together.
func Respond(ctx context.Context, tx pgx.Tx, id, responseID string, values contract.Values, wake ...EnqueueWake) error {
	if strings.TrimSpace(responseID) == "" {
		return &ValidationError{"response requires an identity"}
	}
	inner, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = inner.Rollback(ctx) }()
	tx = inner
	var runID string
	storedRequestRunID, err := db.New(tx).ReadRequestRunID(ctx, id)
	if err != nil {
		return classify(err)
	}

	runID = storedRequestRunID

	run, cancelling, err := lockRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if cancelling || protocol.Terminal(run.Status) {
		return ErrConflict
	}

	storedRequest, queryErr3 := db.New(tx).LockRequest(ctx, id)
	err = queryErr3
	if err != nil {
		return classify(err)
	}

	var request execution.Request
	if err = json.Unmarshal(storedRequest.Document, &request); err != nil {
		return err
	}
	accepted, err := controlTime(ctx, tx, runID)
	if err != nil {
		return err
	}
	if storedRequest.Status != "open" || request.Kind != "human" || !request.Deadline.After(accepted) {
		return ErrConflict
	}
	values, err = validateResponse(ctx, tx, request.Outputs, values)
	if err != nil {
		return err
	}
	response, err := raw(values)
	if err != nil {
		return err
	}
	accepted, err = controlTime(ctx, tx, runID)
	if err != nil || !request.Deadline.After(accepted) {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	if _, err = db.New(tx).AcceptHumanAnswer(ctx, db.AcceptHumanAnswerParams{ID: id, ResponseID: new(responseID), Response: response, AcceptedAt: new(accepted)}); err != nil {
		return err
	}
	if err := enqueueControl(
		ctx,
		tx,
		runID,
		"human",
		execution.HumanSignal{RequestID: id, ResponseID: responseID, Values: values, AcceptedAt: accepted},
		wake,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Resolve accepts only the currently open, matching resolution. It never sends
// arbitrary instance/operation IDs into a workflow or silently ignores bad data.
func Resolve(
	ctx context.Context,
	tx pgx.Tx,
	runID, instanceID, responseID, decision, evidence string,
	outputs contract.Values,
	wake ...EnqueueWake,
) error {
	if strings.TrimSpace(responseID) == "" {
		return &ValidationError{"resolution requires an identity"}
	}
	inner, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = inner.Rollback(ctx) }()
	tx = inner
	run, cancelling, err := lockRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if cancelling || protocol.Terminal(run.Status) {
		return ErrConflict
	}
	stored, err := db.New(tx).LockResolutionRequest(ctx, db.LockResolutionRequestParams{RunID: runID, InstanceID: instanceID})
	if err != nil {
		return classify(err)
	}
	if stored.Status != "open" {
		return ErrConflict
	}
	var req execution.Request
	if err = json.Unmarshal(stored.Document, &req); err != nil {
		return err
	}
	accepted, err := controlTime(ctx, tx, runID)
	if err != nil {
		return err
	}
	if req.Failure == nil || !req.Deadline.After(accepted) {
		return ErrConflict
	}
	if strings.TrimSpace(evidence) == "" {
		return &ValidationError{"resolution requires evidence"}
	}

	switch decision {
	case "completed":
		outputs, err = validateResponse(ctx, tx, req.Outputs, outputs)
		if err != nil {
			return err
		}
	case "not_executed", "failed":
		if len(outputs) != 0 {
			return &ValidationError{"outputs are only accepted for succeeded outcomes"}
		}
	default:
		return &ValidationError{fmt.Sprintf("invalid resolution decision %q", decision)}
	}

	accepted, err = controlTime(ctx, tx, runID)
	if err != nil || !req.Deadline.After(accepted) {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	signal := execution.ResolutionSignal{
		AcceptedAt:  accepted,
		InstanceID:  instanceID,
		OperationID: req.Failure.OperationID,
		ResponseID:  responseID,
		Decision:    decision,
		Evidence:    evidence,
		Outputs:     outputs,
	}
	response, err := raw(signal)
	if err != nil {
		return err
	}
	if _, err = db.New(tx).AcceptResolution(ctx, db.AcceptResolutionParams{ID: req.ID, ResponseID: new(responseID), Response: response, AcceptedAt: new(accepted)}); err != nil {
		return err
	}
	if err := enqueueControl(ctx, tx, runID, "resolve", signal, wake); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateResponse(ctx context.Context, tx pgx.Tx, ports map[string]contract.Port, values contract.Values) (contract.Values, error) {
	for key, value := range values {
		if ports[key].Artifact != nil && value.JSON != nil {
			resolved, err := ArtifactValue(ctx, tx, value.JSON)
			if err != nil {
				return nil, err
			}
			values[key] = resolved
		}
	}
	values, err := contract.ValidatePorts(ports, values, false)
	if err != nil {
		return nil, &ValidationError{err.Error()}
	}
	return values, nil
}

func controlTime(ctx context.Context, tx pgx.Tx, runID string) (time.Time, error) {
	backend, err := ReadRunBackend(ctx, tx, runID)
	if err != nil {
		return time.Time{}, err
	}
	if backend == execution.BackendTemporal {
		return time.Now().UTC(), nil
	}
	run, err := LockExecutionRun(ctx, tx, runID)
	if err != nil {
		return time.Time{}, err
	}
	if err := checkExecutionVersion(run); err != nil {
		return time.Time{}, err
	}
	if !run.Deadline.After(run.Now) {
		return time.Time{}, ErrConflict
	}
	return run.Now, nil
}

func enqueueControl(ctx context.Context, tx pgx.Tx, runID, kind string, signal any, wake []EnqueueWake) error {
	backend, err := ReadRunBackend(ctx, tx, runID)
	if err != nil {
		return err
	}
	if backend == execution.BackendTemporal {
		return Enqueue(ctx, tx, runID, kind, signal)
	}
	if len(wake) != 1 || wake[0] == nil {
		return fmt.Errorf("river command requires transactional scheduler delivery")
	}
	_, err = WakeExecution(ctx, tx, runID, wake[0])
	return err
}

// Resolution arbitrates an accepted operator decision against a deadline or
// cancellation using the same lock order and transaction as the HTTP command.
func (s *Store) Resolution(ctx context.Context, q execution.AnswerRequest) (*execution.ResolutionSignal, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, _, err = lockRun(ctx, tx, q.RunID); err != nil {
		return nil, err
	}

	storedResolutionAnswer, queryErr5 := db.New(tx).LockResolutionAnswer(ctx, db.LockResolutionAnswerParams{ID: q.RequestID, RunID: q.RunID})
	err = queryErr5

	if err != nil {
		return nil, classify(err)
	}
	if storedResolutionAnswer.Status == "resolved" && storedResolutionAnswer.AcceptedAt != nil {
		var signal execution.ResolutionSignal
		if err = json.Unmarshal(storedResolutionAnswer.Response, &signal); err != nil {
			return nil, err
		}
		signal.AcceptedAt = *storedResolutionAnswer.AcceptedAt
		return &signal, tx.Commit(ctx)
	}
	if q.CloseIfAbsent != "" {
		if q.CloseIfAbsent != "cancelled" && q.CloseIfAbsent != "expired" {
			return nil, ErrConflict
		}
		if _, err = db.New(tx).CloseRequest(ctx, db.CloseRequestParams{ID: q.RequestID, Status: q.CloseIfAbsent}); err != nil {
			return nil, err
		}
	}
	return nil, tx.Commit(ctx)
}
