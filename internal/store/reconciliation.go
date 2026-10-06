package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/store/db"
)

// StageLostAttempt fences an expired claim without a verified final outcome.
// The caller checked that the expected envelope is missing or invalid and must commit the scheduler decision,
// slot release and wakeup in this same transaction. A later envelope stays evidence;
// it cannot replace the resulting operator decision or cancelled node.
func (s *Store) StageLostAttempt(ctx context.Context, tx pgx.Tx, token execution.Ownership) (bool, error) {
	if err := token.Validate(); err != nil {
		return false, err
	}
	run, err := LockExecutionRun(ctx, tx, token.RunID)
	if err != nil {
		return false, err
	}
	if err := checkExecutionVersion(run); err != nil {
		return false, err
	}
	q := db.New(tx)
	attempt, err := q.LockLostAttempt(ctx, db.LockLostAttemptParams{RunID: token.RunID, InstanceID: token.InstanceID, Number: token.Number})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if attempt.State != "claimed" || attempt.Owner == nil || *attempt.Owner != token.WorkerID || attempt.OwnershipGeneration != token.Generation || len(attempt.Outcome) > 0 {
		return false, nil
	}
	key, err := token.OutcomeKey()
	if err != nil {
		return false, err
	}
	if attempt.OutcomeKey == nil || *attempt.OutcomeKey != key || attempt.LeaseExpiresAt == nil {
		return false, ErrConflict
	}
	run.Now, err = q.DatabaseTime(ctx)
	if err != nil {
		return false, err
	}
	if attempt.LeaseExpiresAt.After(run.Now) {
		return false, nil
	}
	var request execution.ExecuteRequest
	if err := json.Unmarshal(attempt.ExecutionRequest, &request); err != nil {
		return false, err
	}
	if request.RunID != token.RunID || request.InstanceID != token.InstanceID || request.Attempt != token.Number {
		return false, ErrConflict
	}
	lost := &execution.Failure{Code: "OUTCOME_UNKNOWN", Message: "attempt lease expired without a verified durable final outcome", Unknown: true,
		OperationID: fmt.Sprintf("%s/attempt/%d", token.InstanceID, token.Number), CanRetryIfNotExecuted: request.Node.Type != "agent"}
	operation, err := q.FirstUnconfirmedOperation(ctx, db.FirstUnconfirmedOperationParams{RunID: token.RunID, InstanceID: new(token.InstanceID), AttemptNumber: new(token.Number)})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err == nil {
		lost.OperationID = operation
	}
	state := "completed"
	if executionStopped(run) || !request.Deadline.After(run.Now) || attempt.NodeState != "running" || attempt.AttemptNumber != token.Number {
		state = "cancelled"
	}
	result, err := json.Marshal(execution.ExecuteResult{Failure: lost})
	if err != nil {
		return false, err
	}
	cause, err := json.Marshal(lost)
	if err != nil {
		return false, err
	}
	tag, err := q.SetLostAttempt(ctx, db.SetLostAttemptParams{RunID: token.RunID, InstanceID: token.InstanceID, Number: token.Number, State: state, Result: result, Failure: cause})
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, ErrConflict
	}
	if state == "cancelled" {
		if err := s.ProjectExecution(ctx, tx, execution.Projection{RunID: token.RunID, InstanceID: token.InstanceID, Kind: "diagnostic", Status: "attempt_lost", Failure: lost, Attempt: token.Number, Time: run.Now}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// BindWorkerDeliveryClient associates fetched River jobs with the heartbeat of
// this process. Rebinding an incarnation to another client is forbidden.
func (s *Store) BindWorkerDeliveryClient(ctx context.Context, id, clientID string) error {
	if id == "" || clientID == "" {
		return errors.New("worker delivery binding requires identities")
	}
	tag, err := db.New(s.Pool).BindWorkerDeliveryClient(ctx, db.BindWorkerDeliveryClientParams{ID: id, EngineID: s.EngineID, DeliveryClientID: new(clientID)})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}
