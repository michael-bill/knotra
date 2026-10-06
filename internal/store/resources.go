package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store/db"
)

// RegisterOwnedResource commits the identity before any directory/container creation.
func (s *Store) RegisterOwnedResource(ctx context.Context, token execution.Ownership, id, kind, lifetime string) (execution.ResourceRecord, error) {
	resource := execution.ResourceRecord{ID: id, EngineID: s.EngineID, Kind: kind, Lifetime: lifetime, State: "active", DispatchGeneration: 1, Ownership: token}
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id || (kind != "sandbox" && kind != "mcp_http") || (lifetime != "attempt" && lifetime != "run") {
		return resource, errors.New("invalid resource identity, kind or lifetime")
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := s.LockOwnedAttempt(ctx, tx, token); err != nil {
			return err
		}
		q := db.New(tx)
		host, err := q.ReadResourceHost(ctx, db.ReadResourceHostParams{ID: token.WorkerID, EngineID: s.EngineID})
		if err != nil {
			return err
		}
		resource.HostID = host
		_, err = q.InsertOwnedResource(ctx, db.InsertOwnedResourceParams{ID: id, EngineID: s.EngineID, HostID: host, RunID: token.RunID, InstanceID: token.InstanceID, AttemptNumber: token.Number, WorkerID: token.WorkerID, OwnershipGeneration: token.Generation, Kind: kind, Lifetime: lifetime})
		return err
	})
	return resource, err
}

// A late initialization response can still supply cleanup evidence after its
// attempt ends. The complete immutable owner must match; this admits no tool call.
func (s *Store) RecordOwnedMCPSession(ctx context.Context, token execution.Ownership, id string, session execution.MCPSessionRecord) error {
	if err := token.Validate(); err != nil {
		return err
	}
	if err := session.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	tag, err := db.New(s.Pool).RecordOwnedMCPSession(ctx, db.RecordOwnedMCPSessionParams{ID: id, EngineID: s.EngineID, RunID: token.RunID, InstanceID: token.InstanceID, AttemptNumber: token.Number, WorkerID: token.WorkerID, OwnershipGeneration: token.Generation, McpSession: data})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrExecutionOwnership
	}
	return err
}

// A creator may confirm deletion after its attempt ends, but cannot close another
// incarnation's resource. This writes no node result and admits no external effect.
func (s *Store) CloseOwnedResource(ctx context.Context, token execution.Ownership, id string) error {
	if err := token.Validate(); err != nil {
		return err
	}
	tag, err := db.New(s.Pool).CloseOwnedResource(ctx, db.CloseOwnedResourceParams{ID: id, EngineID: s.EngineID, RunID: token.RunID, InstanceID: token.InstanceID, AttemptNumber: token.Number, WorkerID: token.WorkerID, OwnershipGeneration: token.Generation})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrExecutionOwnership
	}
	return err
}

// ClaimResourceCleanup records a cleanup claim and checks the original owner after
// locking its heartbeat. A live run-scoped session stays protected after its
// creating attempt finishes. Physical cleanup happens after this transaction.
func (s *Store) ClaimResourceCleanup(ctx context.Context, id, host, worker string, generation int64) (*execution.ResourceRecord, error) {
	var resource *execution.ResourceRecord
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		resource, err = s.ClaimResourceCleanupTx(ctx, tx, id, host, worker, generation, 0)
		return err
	})
	return resource, err
}

// A nonzero dispatch generation additionally fences stale River cleanup jobs.
func (s *Store) ClaimResourceCleanupTx(ctx context.Context, tx pgx.Tx, id, host, worker string, generation, dispatchGeneration int64) (*execution.ResourceRecord, error) {
	return s.claimResourceCleanupTx(ctx, tx, id, host, worker, generation, dispatchGeneration, nil)
}

// Physical evidence can reopen a closed resource: an unanswered Docker create
// may finish after cleanup. Immutable labels must match its entire durable owner.
func (s *Store) ClaimObservedResourceCleanupTx(ctx context.Context, tx pgx.Tx, observed execution.ResourceRecord) (*execution.ResourceRecord, error) {
	if observed.Kind != "sandbox" {
		return nil, errors.New("container evidence requires a sandbox resource")
	}
	if err := observed.Validate(); err != nil {
		return nil, err
	}
	if observed.EngineID != s.EngineID {
		return nil, nil
	}
	return s.claimResourceCleanupTx(ctx, tx, observed.ID, observed.HostID, observed.Ownership.WorkerID, observed.Ownership.Generation, 0, &observed)
}

func (s *Store) claimResourceCleanupTx(ctx context.Context, tx pgx.Tx, id, host, worker string, generation, dispatchGeneration int64, observed *execution.ResourceRecord) (*execution.ResourceRecord, error) {
	q := db.New(tx)
	runID, err := q.ReadResourceIdentity(ctx, db.ReadResourceIdentityParams{ID: id, EngineID: s.EngineID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run, err := LockExecutionRun(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if err := checkExecutionVersion(run); err != nil {
		return nil, err
	}
	row, err := q.LockResourceCleanup(ctx, db.LockResourceCleanupParams{ID: id, EngineID: s.EngineID})
	if err != nil {
		return nil, err
	}
	if row.HostID != host || row.WorkerID != worker || row.OwnershipGeneration != generation || (dispatchGeneration > 0 && row.CleanupGeneration != dispatchGeneration) {
		return nil, nil
	}
	if observed != nil && (observed.Kind != row.Kind || observed.Ownership.RunID != row.RunID || observed.Ownership.InstanceID != row.InstanceID || observed.Ownership.Number != row.AttemptNumber) {
		return nil, nil
	}
	if row.State == "closed" && observed == nil {
		return nil, nil
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return nil, err
	}
	dead := !row.HeartbeatAt.Add(execution.LeaseDuration).After(now)
	ended := row.Lifetime == "attempt" && (row.AttemptState != "claimed" || row.LeaseExpiresAt == nil || !row.LeaseExpiresAt.After(now))
	if row.ClosedAt == nil && !dead && !ended && !protocol.Terminal(run.Status) {
		return nil, nil
	}
	if _, err := q.ClaimResourceCleanup(ctx, id); err != nil {
		return nil, err
	}
	resource := &execution.ResourceRecord{ID: id, EngineID: s.EngineID, HostID: row.HostID, Kind: row.Kind, Lifetime: row.Lifetime, State: "cleaning", DispatchGeneration: row.CleanupGeneration, Ownership: execution.Ownership{AttemptID: execution.AttemptID{RunID: row.RunID, InstanceID: row.InstanceID, Number: row.AttemptNumber}, WorkerID: row.WorkerID, Generation: row.OwnershipGeneration}}
	if row.McpSession != nil {
		if err := json.Unmarshal(row.McpSession, &resource.MCP); err != nil {
			return nil, err
		}
	}
	if err := resource.Validate(); err != nil {
		return nil, err
	}
	return resource, nil
}
