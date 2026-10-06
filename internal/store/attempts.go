package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store/db"
)

var ErrExecutionOwnership = errors.New("execution ownership is no longer valid")

// RegisterWorker requires a fresh process incarnation. Reusing an expired ID
// must not resurrect claims belonging to a previous process.
func (s *Store) RegisterWorker(ctx context.Context, id, hostID string, roles ...execution.Role) error {
	role := execution.RoleAll
	if len(roles) > 1 {
		return errors.New("worker requires exactly one process role")
	}
	if len(roles) == 1 {
		role = roles[0]
	}
	if err := role.Validate(); err != nil {
		return err
	}
	if err := execution.ValidateHostID(hostID); err != nil {
		return err
	}
	if id == "" || hostID == "" {
		return errors.New("worker requires an incarnation and host identity")
	}
	capabilities, err := json.Marshal(map[string]execution.Role{"role": role})
	if err != nil {
		return err
	}
	tag, err := db.New(s.Pool).RegisterWorker(ctx, db.RegisterWorkerParams{
		ID:           id,
		EngineID:     s.EngineID,
		HostID:       hostID,
		Column4:      execution.SchedulerVersion,
		Column5:      execution.StateFormatVersion,
		Capabilities: capabilities,
	})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}

func (s *Store) HeartbeatWorker(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var heartbeat time.Time
		storedWorkerHeartbeat, err := db.New(tx).LockWorkerHeartbeat(ctx, db.LockWorkerHeartbeatParams{ID: id, EngineID: s.EngineID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrExecutionOwnership
			}
			return err
		}

		heartbeat = storedWorkerHeartbeat

		var now time.Time
		storedDatabaseTime, err := db.New(tx).DatabaseTime(ctx)
		if err != nil {
			return err
		}

		now = storedDatabaseTime

		if !heartbeat.Add(execution.LeaseDuration).After(now) {
			return ErrExecutionOwnership
		}
		_, err = db.New(tx).UpdateWorkerHeartbeat(ctx, db.UpdateWorkerHeartbeatParams{ID: id, HeartbeatAt: now})
		return err
	})
}

func (s *Store) DrainWorker(ctx context.Context, id string) error {
	tag, err := db.New(s.Pool).DrainWorker(ctx, db.DrainWorkerParams{ID: id, EngineID: s.EngineID})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return err
}

// ClaimAttempt returns nil for an ineligible or duplicate delivery, including a
// capacity miss. It never changes the business attempt number. Claimed attempts
// cannot be claimed again, even after their lease expires.
func (s *Store) ClaimAttempt(ctx context.Context, tx pgx.Tx, id execution.AttemptID, dispatchGeneration int64, workerID string) (*execution.Claim, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if dispatchGeneration < 1 || workerID == "" {
		return nil, errors.New("claim requires dispatch generation and worker incarnation")
	}
	inner, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inner.Rollback(ctx) }()
	run, err := LockExecutionRun(ctx, inner, id.RunID)
	if err != nil {
		return nil, err
	}
	if err := checkExecutionVersion(run); err != nil {
		return nil, err
	}
	if executionStopped(run) {
		return nil, nil
	}
	if run.Paused {
		// This delivery is consumed without admission. Preserve redispatch when
		// the last operator decision clears the root pause.
		if _, err := db.New(inner).MarkPausedAttemptForDispatch(ctx, db.MarkPausedAttemptForDispatchParams{
			RunID:              id.RunID,
			InstanceID:         id.InstanceID,
			Number:             id.Number,
			DispatchGeneration: dispatchGeneration,
		}); err != nil {
			return nil, err
		}
		return nil, inner.Commit(ctx)
	}

	var startedAt *time.Time

	storedClaimNode, queryErr3 := db.New(inner).LockClaimNode(ctx, db.LockClaimNodeParams{RunID: id.RunID, ID: id.InstanceID})
	err = queryErr3
	if err == nil {
		startedAt = storedClaimNode.StartedAt
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedClaimNode.NodeState != "ready" || storedClaimNode.GraphState != "running" || storedClaimNode.AttemptNumber != id.Number {
		return nil, nil
	}

	storedClaimAttempt, queryErr4 := db.New(inner).LockClaimAttempt(ctx, db.LockClaimAttemptParams{RunID: id.RunID, InstanceID: id.InstanceID, Number: id.Number})
	err = queryErr4

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedClaimAttempt.State != "ready" || storedClaimAttempt.DispatchGeneration != dispatchGeneration {
		return nil, nil
	}
	var request execution.ExecuteRequest
	if err := json.Unmarshal(storedClaimNode.ExecutionRequest, &request); err != nil {
		return nil, err
	}
	if request.RunID != id.RunID || request.InstanceID != id.InstanceID || request.Attempt != id.Number || request.NodeID != storedClaimNode.NodeID || request.Pipeline != storedClaimNode.Pipeline || request.Ownership != nil || storedClaimNode.Deadline == nil || !request.Deadline.Equal(*storedClaimNode.Deadline) {
		return nil, fmt.Errorf("stored execution request does not match attempt %v", id)
	}
	var scopes []execution.BudgetScope
	if err := json.Unmarshal(storedClaimNode.Scopes, &scopes); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		ids = append(ids, scope.ID)
	}
	if len(ids) != len(request.Scopes) || !slices.Contains(ids, id.RunID) {
		return nil, errors.New("attempt must reserve all enclosing scopes, including the root")
	}
	for i, scope := range request.Scopes {
		if scope.ID != ids[i] || scope.ID == "" {
			return nil, errors.New("attempt scope identity mismatch")
		}
	}
	slices.Sort(ids)
	if len(slices.Compact(slices.Clone(ids))) != len(ids) {
		return nil, errors.New("duplicate attempt scope")
	}

	var compatible bool
	storedClaimWorker, queryErr5 := db.New(inner).ReadClaimWorker(ctx, db.ReadClaimWorkerParams{
		ID:                 workerID,
		EngineID:           s.EngineID,
		SchedulerVersion:   run.SchedulerVersion,
		StateFormatVersion: run.StateFormatVersion,
	})
	err = queryErr5
	if err == nil {
		compatible = storedClaimWorker.Compatible != nil && *storedClaimWorker.Compatible
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedClaimWorker.Draining || !compatible {
		return nil, nil
	}
	rows, err := db.New(inner).LockClaimScopes(ctx, db.LockClaimScopesParams{RunID: id.RunID, ScopeIDs: ids})
	if err != nil {
		return nil, err
	}
	var found int
	var full bool
	for _, record := range rows {
		var scopeID string
		var limitsBytes []byte
		var active int
		scopeID = record.ID
		limitsBytes = record.Limits
		active = record.ActiveAttempts
		var limits contract.Limits
		if err := json.Unmarshal(limitsBytes, &limits); err != nil {
			return nil, err
		}
		full = full || limits.MaxConcurrentNodes <= 0 || active >= limits.MaxConcurrentNodes
		for i := range request.Scopes {
			if request.Scopes[i].ID == scopeID {
				request.Scopes[i].Limits = limits
			}
		}
		found++
	}

	if err != nil {
		return nil, err
	}
	if found != len(ids) {
		return nil, errors.New("missing enclosing execution scope")
	}
	storedDatabaseTime2, err := db.New(inner).DatabaseTime(ctx)
	if err != nil {
		return nil, err
	}

	run.Now = storedDatabaseTime2

	if !storedClaimWorker.HeartbeatAt.Add(execution.LeaseDuration).After(run.Now) || !storedClaimNode.Deadline.After(run.Now) || executionStopped(run) {
		return nil, nil
	}
	if full {
		if _, err := db.New(inner).MarkAttemptForDispatch(ctx, db.MarkAttemptForDispatchParams{RunID: id.RunID, InstanceID: id.InstanceID, Number: id.Number}); err != nil {
			return nil, err
		}
		return nil, inner.Commit(ctx)
	}
	claim := &execution.Claim{Ownership: execution.Ownership{AttemptID: id, WorkerID: workerID, Generation: storedClaimAttempt.OwnershipGeneration + 1}, Request: request}
	claim.PlanID = storedClaimNode.PlanDigest
	claim.OutcomeKey, err = claim.Ownership.OutcomeKey()
	if err != nil {
		return nil, err
	}
	claim.LeaseExpiresAt = minTime(run.Now.Add(execution.LeaseDuration), minTime(*storedClaimNode.Deadline, run.Deadline))
	claim.Request.Ownership = &claim.Ownership
	for _, scopeID := range ids {
		if _, err := db.New(inner).ReserveAttemptSlot(ctx, db.ReserveAttemptSlotParams{
			RunID:               id.RunID,
			InstanceID:          id.InstanceID,
			AttemptNumber:       id.Number,
			ScopeID:             scopeID,
			OwnershipGeneration: claim.Generation,
		}); err != nil {
			return nil, err
		}
		if _, err := db.New(inner).IncreaseActiveAttempts(ctx, db.IncreaseActiveAttemptsParams{RunID: id.RunID, ID: scopeID}); err != nil {
			return nil, err
		}
	}
	if _, err := db.New(inner).SetAttemptClaimed(ctx, db.SetAttemptClaimedParams{
		RunID:               id.RunID,
		InstanceID:          id.InstanceID,
		Number:              id.Number,
		Owner:               new(workerID),
		OwnershipGeneration: claim.Generation,
		LeaseExpiresAt:      new(claim.LeaseExpiresAt),
		OutcomeKey:          new(claim.OutcomeKey),
	}); err != nil {
		return nil, err
	}
	if startedAt == nil {
		startedAt = &run.Now
	}
	if _, err := db.New(inner).SetNodeRunning(ctx, db.SetNodeRunningParams{RunID: id.RunID, ID: id.InstanceID, StartedAt: startedAt}); err != nil {
		return nil, err
	}
	if _, err := db.New(inner).IncrementRunRevision(ctx, id.RunID); err != nil {
		return nil, err
	}
	projection := execution.Projection{RunID: id.RunID, Kind: "node", InstanceID: id.InstanceID, NodeID: storedClaimNode.NodeID,
		Pipeline: storedClaimNode.Pipeline, GraphPath: storedClaimNode.GraphPath + "/nodes/" + storedClaimNode.NodeID, IterationIndex: storedClaimNode.IterationIndex, NodeType: request.Node.Type,
		Status: "running", Attempt: id.Number, Inputs: request.Inputs, Time: run.Now, StartedAt: startedAt}
	if storedClaimNode.ParentInstanceID != nil {
		projection.ParentInstanceID = *storedClaimNode.ParentInstanceID
	}
	if err := s.ProjectExecution(ctx, inner, projection); err != nil {
		return nil, err
	}
	if err := inner.Commit(ctx); err != nil {
		return nil, err
	}
	return claim, nil
}

func executionStopped(run execution.RunState) bool {
	return run.Cancelled || run.StopCause != nil || protocol.Terminal(run.Status) || !run.Deadline.After(run.Now)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// LockOwnedAttempt gates new external effects and result publication. Admission
// pause allows already admitted work to finish; cancellation and expiry do not.
// Workers are locked after attempts and before scopes in the mutation lock order.
func (s *Store) LockOwnedAttempt(ctx context.Context, tx pgx.Tx, token execution.Ownership) (execution.RunState, error) {
	if err := token.Validate(); err != nil {
		return execution.RunState{}, err
	}
	run, err := LockExecutionRun(ctx, tx, token.RunID)
	if err != nil {
		return run, err
	}
	if err := checkExecutionVersion(run); err != nil {
		return run, err
	}

	storedOwnedNode, queryErr7 := db.New(tx).LockOwnedNode(ctx, db.LockOwnedNodeParams{RunID: token.RunID, ID: token.InstanceID})
	err = queryErr7

	if errors.Is(err, pgx.ErrNoRows) {
		return run, ErrExecutionOwnership
	}
	if err != nil {
		return run, err
	}
	if storedOwnedNode.State != "running" || storedOwnedNode.AttemptNumber != token.Number || storedOwnedNode.Deadline == nil {
		return run, ErrExecutionOwnership
	}
	lease, err := db.New(tx).LockAttemptLease(ctx, db.LockAttemptLeaseParams{
		RunID:               token.RunID,
		InstanceID:          token.InstanceID,
		Number:              token.Number,
		Owner:               new(token.WorkerID),
		OwnershipGeneration: token.Generation,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return run, ErrExecutionOwnership
	}
	if err != nil {
		return run, err
	}
	if lease == nil {
		return run, ErrExecutionOwnership
	}
	heartbeat, err := db.New(tx).ReadOwnedWorkerHeartbeat(ctx, db.ReadOwnedWorkerHeartbeatParams{
		ID:                 token.WorkerID,
		EngineID:           s.EngineID,
		SchedulerVersion:   run.SchedulerVersion,
		StateFormatVersion: run.StateFormatVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return run, ErrExecutionOwnership
	}
	if err != nil {
		return run, err
	}
	storedDatabaseTime3, err := db.New(tx).DatabaseTime(ctx)
	if err != nil {
		return run, err
	}

	run.Now = storedDatabaseTime3

	if executionStopped(run) || !storedOwnedNode.Deadline.After(run.Now) || !lease.After(run.Now) || !heartbeat.Add(execution.LeaseDuration).After(run.Now) {
		return run, ErrExecutionOwnership
	}
	return run, nil
}

func (s *Store) RenewAttempt(ctx context.Context, tx pgx.Tx, token execution.Ownership) (time.Time, error) {
	run, err := s.LockOwnedAttempt(ctx, tx, token)
	if err != nil {
		return time.Time{}, err
	}
	expires, err := db.New(tx).RenewAttempt(ctx, db.RenewAttemptParams{
		RunID:          token.RunID,
		ID:             token.InstanceID,
		Number:         token.Number,
		LeaseExpiresAt: new(run.Now.Add(execution.LeaseDuration)),
		RootDeadline:   new(run.Deadline),
	})
	if err != nil {
		return time.Time{}, err
	}
	if expires == nil {
		return time.Time{}, ErrExecutionOwnership
	}
	return *expires, nil
}

// ReleaseAttemptSlots is called after completion or reconciliation changes the
// attempt state. A stale generation cannot release a replacement owner's slots.
// The release and wakeup roll back together, even if the caller commits on error.
func ReleaseAttemptSlots(ctx context.Context, tx pgx.Tx, token execution.Ownership, enqueue EnqueueWake) (bool, error) {
	if err := token.Validate(); err != nil {
		return false, err
	}
	if enqueue == nil {
		return false, errors.New("slot release requires transactional wakeup")
	}
	inner, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = inner.Rollback(ctx) }()
	run, err := LockExecutionRun(ctx, inner, token.RunID)
	if err != nil {
		return false, err
	}
	if err := checkExecutionVersion(run); err != nil {
		return false, err
	}
	var state string
	state, err = db.New(inner).LockCompletedAttempt(ctx, db.LockCompletedAttemptParams{
		RunID:               token.RunID,
		InstanceID:          token.InstanceID,
		Number:              token.Number,
		Owner:               new(token.WorkerID),
		OwnershipGeneration: token.Generation,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state == "claimed" || state == "ready" {
		return false, ErrConflict
	}
	rows, err := db.New(inner).LockReservedScopeIDs(ctx, db.LockReservedScopeIDsParams{
		RunID:               token.RunID,
		InstanceID:          token.InstanceID,
		AttemptNumber:       token.Number,
		OwnershipGeneration: token.Generation,
	})
	if err != nil {
		return false, err
	}
	var ids []string
	for _, record := range rows {
		id := record
		ids = append(ids, id)
	}

	if err != nil || len(ids) == 0 {
		return false, err
	}
	for _, id := range ids {
		if _, err := db.New(inner).DecreaseActiveAttempts(ctx, db.DecreaseActiveAttemptsParams{RunID: token.RunID, ID: id}); err != nil {
			return false, err
		}
	}
	if _, err := db.New(inner).ReleaseReservedSlots(ctx, db.ReleaseReservedSlotsParams{
		RunID:               token.RunID,
		InstanceID:          token.InstanceID,
		AttemptNumber:       token.Number,
		OwnershipGeneration: token.Generation,
	}); err != nil {
		return false, err
	}
	if _, err := WakeExecution(ctx, inner, token.RunID, enqueue); err != nil {
		return false, err
	}
	return true, inner.Commit(ctx)
}
