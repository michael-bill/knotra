package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/michael-bill/knotra/internal/execution"
)

func TestOwnedOperationAdmissionAndBudgetAreAtomic(t *testing.T) {
	s, ids, workers, _ := attemptFixture(t)
	ctx := context.Background()
	claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[0])
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE knotra_execution_scopes SET limits=jsonb_set(limits,'{maxModelCalls}','1') WHERE run_id=$1 AND id<>$1`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve(ctx, claim.RunID, "model", claim.Request.Scopes); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("unowned budget write bypassed guard: %v", err)
	}
	if _, err := s.BeginOperation(ctx, "unowned", claim.RunID, "tool", "write"); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("unowned operation bypassed guard: %v", err)
	}
	var physicalCalls atomic.Int32
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			state, err := s.BeginOwnedOperation(ctx, claim.Ownership, "model", "model", "read")
			if err != nil {
				t.Error(err)
				return
			}
			if state.Started {
				return
			}
			if err := s.AdmitOperation(ctx, claim.Ownership, "model", "model"); err != nil {
				if !errors.Is(err, ErrConflict) {
					t.Error(err)
				}
				return
			}
			physicalCalls.Add(1)
		})
	}
	wg.Wait()
	if physicalCalls.Load() != 1 {
		t.Fatalf("one admission permitted %d physical calls", physicalCalls.Load())
	}
	response := json.RawMessage(`{"answer":42}`)
	for range 2 {
		if err := s.CompleteOwnedOperation(ctx, claim.Ownership, "model", response); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CompleteOwnedOperation(ctx, claim.Ownership, "model", json.RawMessage(`{"answer":43}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed durable response: %v", err)
	}
	if err := s.CompleteOperation(ctx, "model", response); err == nil {
		t.Fatal("legacy completion bypassed token guard")
	}
	state, err := s.BeginOwnedOperation(ctx, claim.Ownership, "model", "model", "read")
	if err != nil || !state.Completed || string(state.Response) != string(response) {
		t.Fatalf("completed response was lost: %+v %v", state, err)
	}
	if _, err := s.BeginOwnedOperation(ctx, claim.Ownership, "model", "tool", "write"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused operation identity for another effect: %v", err)
	}
	if _, err := s.BeginOwnedOperation(ctx, claim.Ownership, "over-budget", "model", "read"); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitOperation(ctx, claim.Ownership, "over-budget", "model"); err == nil || !strings.Contains(err.Error(), "BUDGET_EXCEEDED") {
		t.Fatalf("bypassed ancestor budget: %v", err)
	}
	var rootUsed, childUsed int
	var unadmitted bool
	if err := s.Pool.QueryRow(ctx, `SELECT
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$2 AND kind='model'),
		(SELECT admitted_at IS NULL FROM knotra_operations WHERE id='over-budget')`, claim.RunID, claim.Request.Scopes[1].ID).
		Scan(&rootUsed, &childUsed, &unadmitted); err != nil {
		t.Fatal(err)
	}
	if rootUsed != 1 || childUsed != 1 || !unadmitted {
		t.Fatalf("failed admission partially committed: root=%d child=%d unadmitted=%v", rootUsed, childUsed, unadmitted)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1 AND instance_id=$2", claim.RunID, claim.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginOwnedOperation(ctx, claim.Ownership, "late", "tool", "write"); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("expired owner prepared a new effect: %v", err)
	}
	if err := s.AdmitOperation(ctx, claim.Ownership, "over-budget", "model"); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("expired owner admitted an effect: %v", err)
	}
	if err := s.CompleteOwnedOperation(ctx, claim.Ownership, "model", response); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("expired owner published directly: %v", err)
	}
}

func TestOwnedOperationCancellationRetainsUnconfirmedIntent(t *testing.T) {
	s, ids, workers, _ := attemptFixture(t)
	ctx := context.Background()
	claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[0])
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if _, err := s.BeginOwnedOperation(ctx, claim.Ownership, "write", "tool", "write"); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitOperation(ctx, claim.Ownership, "write", "tool"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginOwnedOperation(ctx, claim.Ownership, "prepared", "tool", "write"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_runs SET cancel_requested=true WHERE id=$1", claim.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitOperation(ctx, claim.Ownership, "prepared", "tool"); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("cancelled owner admitted a write: %v", err)
	}
	if err := s.CompleteOwnedOperation(ctx, claim.Ownership, "write", json.RawMessage(`{"ok":true}`)); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("cancelled owner confirmed late evidence: %v", err)
	}
	files := Artifacts{Store: s, Root: t.TempDir()}
	if _, err := s.PutOwnedArtifact(ctx, claim.Ownership, files, "late.txt", "text/plain", []byte("late"), map[string]string{"runId": claim.RunID}); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("cancelled owner published artifact metadata: %v", err)
	}
	var admitted, completed bool
	var artifacts, used int
	if err := s.Pool.QueryRow(ctx, `SELECT admitted_at IS NOT NULL,completed,
		(SELECT count(*) FROM knotra_artifacts),(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool')
		FROM knotra_operations WHERE id='write'`, claim.RunID).Scan(&admitted, &completed, &artifacts, &used); err != nil {
		t.Fatal(err)
	}
	if !admitted || completed || artifacts != 0 || used != 1 {
		t.Fatalf("late response erased uncertainty: admitted=%v completed=%v artifacts=%d used=%d", admitted, completed, artifacts, used)
	}
	// A lifecycle intent is already admitted even without a physical-call budget.
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_runs SET cancel_requested=false WHERE id=$1", claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginOwnedOperation(ctx, claim.Ownership, "workspace", "", "unknown"); err != nil {
		t.Fatal(err)
	}
	state, err := s.BeginOwnedOperation(ctx, claim.Ownership, "workspace", "", "unknown")
	if err != nil || !state.Started || state.Completed {
		t.Fatalf("lost workspace could be reconstructed: %+v %v", state, err)
	}
	stale := execution.Ownership{AttemptID: claim.AttemptID, WorkerID: claim.WorkerID, Generation: claim.Generation + 1}
	if _, err := s.BeginOwnedOperation(ctx, stale, "workspace", "", "unknown"); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("stale generation used journal: %v", err)
	}
}
