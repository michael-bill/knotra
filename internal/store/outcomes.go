package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store/db"
)

// StageOutcome is the live owner's part of a publication transaction. The
// caller applies scheduler changes, projections, slot releases, jobs and River
// completion in tx before committing. It cannot publish with an expired lease.
func (s *Store) StageOutcome(ctx context.Context, tx pgx.Tx, outcome execution.Outcome) (bool, error) {
	if _, err := s.LockOwnedAttempt(ctx, tx, outcome.Ownership); err != nil {
		return false, err
	}
	return stageOutcome(ctx, tx, outcome)
}

// RecoverOutcome imports a verified immutable envelope, independently of the
// old owner's lease. It never executes work or replaces an operator decision.
// Required artifact bytes must have been verified outside this transaction.
func RecoverOutcome(ctx context.Context, tx pgx.Tx, outcome execution.Outcome) (bool, error) {
	return stageOutcome(ctx, tx, outcome)
}

func stageOutcome(ctx context.Context, tx pgx.Tx, outcome execution.Outcome) (bool, error) {
	if err := outcome.Validate(); err != nil {
		return false, err
	}
	token := outcome.Ownership
	run, err := LockExecutionRun(ctx, tx, token.RunID)
	if err != nil {
		return false, err
	}
	if err := checkExecutionVersion(run); err != nil {
		return false, err
	}
	attempt, err := db.New(tx).LockOutcomeAttempt(ctx, db.LockOutcomeAttemptParams{RunID: token.RunID, ID: token.InstanceID, Number: token.Number})
	if err != nil {
		return false, classify(err)
	}
	if attempt.Owner == nil || attempt.OutcomeKey == nil {
		return false, ErrExecutionOwnership
	}
	key := *attempt.OutcomeKey
	// The attempt lock may have waited beyond a deadline; evaluate eligibility
	// with database time after all ownership records have been locked.
	storedDatabaseTime, err := db.New(tx).DatabaseTime(ctx)
	if err != nil {
		return false, err
	}

	run.Now = storedDatabaseTime

	expectedKey, err := token.OutcomeKey()
	if err != nil {
		return false, err
	}
	if key != expectedKey || *attempt.Owner != token.WorkerID || attempt.OwnershipGeneration != token.Generation || attempt.PlanDigest != outcome.PlanID {
		return false, ErrConflict
	}
	b, err := json.Marshal(outcome)
	if err != nil {
		return false, err
	}
	if len(attempt.Outcome) > 0 {
		var same bool
		storedOutcomeMatches, err := db.New(tx).OutcomeMatches(ctx, db.OutcomeMatchesParams{RunID: token.RunID, InstanceID: token.InstanceID, Number: token.Number, Outcome: b})
		if err != nil {
			return false, err
		}

		same = storedOutcomeMatches

		if !same {
			return false, execution.ErrOutcomeConflict
		}
	} else if _, err := db.New(tx).SaveOutcomeEvidence(ctx, db.SaveOutcomeEvidenceParams{
		RunID:       token.RunID,
		InstanceID:  token.InstanceID,
		Number:      token.Number,
		Outcome:     b,
		EvidenceKey: new(key),
	}); err != nil {
		return false, err
	}
	// Even ineligible evidence remains inspectable. Cancellation and an already
	// terminal/superseded node cannot admit its outputs into the graph.
	if attempt.AttemptNumber != token.Number || attempt.NodeState != "running" || run.Cancelled || protocol.Terminal(run.Status) || run.StopCause != nil || attempt.Deadline == nil || !attempt.Deadline.After(run.Now) || !run.Deadline.After(run.Now) {
		if attempt.AttemptState == "claimed" {
			_, err := db.New(tx).CancelAttempt(ctx, db.CancelAttemptParams{RunID: token.RunID, InstanceID: token.InstanceID, Number: token.Number})
			return false, err
		}
		return false, nil
	}
	if attempt.AttemptState == "completed" {
		return true, nil
	}
	if attempt.AttemptState != "claimed" {
		return false, nil
	}
	if outcome.Failure == nil {
		var unconfirmed bool
		storedHasUnconfirmedOperations, err := db.New(tx).HasUnconfirmedOperations(ctx, db.HasUnconfirmedOperationsParams{
			RunID:         token.RunID,
			InstanceID:    new(token.InstanceID),
			AttemptNumber: new(token.Number),
		})
		if err != nil {
			return false, err
		}

		unconfirmed = storedHasUnconfirmedOperations

		if unconfirmed {
			return false, errors.New("successful envelope contains an unconfirmed external operation")
		}
	}
	result, err := json.Marshal(execution.ExecuteResult{Outputs: outcome.Outputs, Failure: outcome.Failure})
	if err != nil {
		return false, err
	}
	_, err = db.New(tx).SetAttemptCompleted(ctx, db.SetAttemptCompletedParams{
		RunID:      token.RunID,
		InstanceID: token.InstanceID,
		Number:     token.Number,
		Result:     result,
	})
	return err == nil, err
}
