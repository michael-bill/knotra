package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/store/db"
)

// BeginOwnedOperation checks for a prior response without spending another
// budget. Model/tool intents are prepared here and admitted by AdmitOperation
// immediately before the physical call. Lifecycle intents (kind="") are admitted
// immediately so a lost agent workspace or MCP session cannot be recreated.
func (s *Store) BeginOwnedOperation(ctx context.Context, token execution.Ownership, id, kind, effect string) (OperationState, error) {
	if id == "" || (kind != "" && kind != "model" && kind != "tool") || (effect != "read" && effect != "write" && effect != "unknown") {
		return OperationState{}, errors.New("invalid external operation intent")
	}
	var state OperationState
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		run, err := s.LockOwnedAttempt(ctx, tx, token)
		if err != nil {
			return err
		}

		storedOperationIntent, queryErr1 := db.New(tx).LockOperationIntent(ctx, id)
		err = queryErr1
		if err == nil {
			state.Completed = storedOperationIntent.Completed
			state.Response = storedOperationIntent.Response
		}
		if errors.Is(err, pgx.ErrNoRows) {
			var admittedAt *time.Time
			if kind == "" {
				admittedAt = &run.Now
			}
			_, err := db.New(tx).InsertOwnedOperation(ctx, db.InsertOwnedOperationParams{
				ID:                  id,
				RunID:               token.RunID,
				Kind:                kind,
				Effect:              effect,
				InstanceID:          new(token.InstanceID),
				AttemptNumber:       new(token.Number),
				WorkerID:            new(token.WorkerID),
				OwnershipGeneration: new(token.Generation),
				AdmittedAt:          admittedAt,
			})
			return err
		}
		if err != nil {
			return err
		}
		if storedOperationIntent.RunID != token.RunID || storedOperationIntent.Kind != kind || storedOperationIntent.Effect != effect {
			return ErrConflict
		}
		state.Started = storedOperationIntent.AdmittedAt != nil
		if state.Started || state.Completed {
			return nil
		}
		if storedOperationIntent.InstanceID == nil || *storedOperationIntent.InstanceID != token.InstanceID {
			return ErrConflict
		}
		// A prepared intent proves no physical call was admitted. A permitted
		// later business attempt can therefore take responsibility for it.
		_, err = db.New(tx).TransferPreparedOperation(ctx, db.TransferPreparedOperationParams{
			ID:                  id,
			AttemptNumber:       new(token.Number),
			WorkerID:            new(token.WorkerID),
			OwnershipGeneration: new(token.Generation),
		})
		return err
	})
	return state, err
}

// AdmitOperation commits the intent and every ancestor budget debit together.
// Calling it twice does not grant permission for a second physical call.
func (s *Store) AdmitOperation(ctx context.Context, token execution.Ownership, id, kind string) error {
	if kind != "model" && kind != "tool" {
		return fmt.Errorf("unknown budget kind %q", kind)
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := s.LockOwnedAttempt(ctx, tx, token); err != nil {
			return err
		}
		var admitted *time.Time
		admitted, err := db.New(tx).LockOperationAdmission(ctx, db.LockOperationAdmissionParams{
			ID:                  id,
			RunID:               token.RunID,
			InstanceID:          new(token.InstanceID),
			AttemptNumber:       new(token.Number),
			WorkerID:            new(token.WorkerID),
			OwnershipGeneration: new(token.Generation),
			Kind:                kind,
		})
		if errors.Is(err, pgx.ErrNoRows) || admitted != nil {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		rows, err := db.New(tx).LockOperationScopes(ctx, db.LockOperationScopesParams{
			RunID:               token.RunID,
			InstanceID:          token.InstanceID,
			AttemptNumber:       token.Number,
			OwnershipGeneration: token.Generation,
		})
		if err != nil {
			return err
		}
		var scopes []execution.BudgetScope
		var hasRoot bool
		for _, record := range rows {
			var scope execution.BudgetScope
			var b []byte
			scope.ID = record.ID
			b = record.Limits
			if err := json.Unmarshal(b, &scope.Limits); err != nil {
				return err
			}
			hasRoot = hasRoot || scope.ID == token.RunID
			scopes = append(scopes, scope)
		}

		if err != nil {
			return err
		}
		if !hasRoot {
			return ErrExecutionOwnership
		}
		// Lock acquisition may have crossed a lease or contract deadline.
		run, err := s.LockOwnedAttempt(ctx, tx, token)
		if err != nil {
			return err
		}
		if err := reserveCalls(ctx, tx, token.RunID, kind, scopes); err != nil {
			return err
		}
		_, err = db.New(tx).SetOperationAdmitted(ctx, db.SetOperationAdmittedParams{ID: id, AdmittedAt: new(run.Now)})
		return err
	})
}

func (s *Store) CompleteOwnedOperation(ctx context.Context, token execution.Ownership, id string, response json.RawMessage) error {
	if !json.Valid(response) {
		return errors.New("invalid external operation response")
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := s.LockOwnedAttempt(ctx, tx, token); err != nil {
			return err
		}

		storedOwnedOperationResponse, err := db.New(tx).LockOwnedOperationResponse(ctx, db.LockOwnedOperationResponseParams{
			ID:                  id,
			RunID:               token.RunID,
			InstanceID:          new(token.InstanceID),
			AttemptNumber:       new(token.Number),
			WorkerID:            new(token.WorkerID),
			OwnershipGeneration: new(token.Generation),
		})

		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if storedOwnedOperationResponse.Completed {
			if !bytes.Equal(storedOwnedOperationResponse.Response, response) {
				return ErrConflict
			}
			return nil
		}
		_, err = db.New(tx).SetOperationCompleted(ctx, db.SetOperationCompletedParams{ID: id, Response: []byte(response)})
		return err
	})
}

// PutOwnedArtifact writes immutable bytes outside the mutation lock, then guards
// their metadata insertion. An abandoned file does not become a public artifact.
func (s *Store) PutOwnedArtifact(ctx context.Context, token execution.Ownership, files Artifacts, name, media string, data []byte, origin map[string]string) (contract.Artifact, error) {
	artifact, err := files.Write(name, media, data, origin)
	if err != nil {
		return artifact, err
	}
	b, err := json.Marshal(artifact)
	if err != nil {
		return artifact, err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := s.LockOwnedAttempt(ctx, tx, token); err != nil {
			return err
		}
		_, err := db.New(tx).InsertArtifact(ctx, db.InsertArtifactParams{ID: artifact.ID, Document: b})
		return err
	})
	return artifact, err
}
