package queue

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/adapters"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/executor"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func TestRoleRejectsUnknownRoleAndMissingConsumers(t *testing.T) {
	for _, role := range []execution.Role{"invalid", execution.RoleAll, execution.RoleScheduler, execution.RoleExecutor} {
		t.Run(string(role), func(t *testing.T) {
			_, err := New(nil, Config{Role: role, EngineID: uuid.NewString(), HostID: "test",
				ExecutionWorkers: 1, ExecutionTimeout: time.Minute}, Handlers{})
			if err == nil {
				t.Fatal("accepted an invalid role or missing required handlers")
			}
		})
	}
}

func TestSplitRolesExecuteDAGThroughIndependentClients(t *testing.T) {
	base := newTestRuntime(t)
	ctx := context.Background()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/api/chat" {
			t.Errorf("unexpected provider request: %s %s", req.Method, req.URL.Path)
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": `{"answer":"complete role-split output"}`},
			"done":    true, "done_reason": "stop",
		})
	}))
	defer server.Close()
	newRole := func(role execution.Role) *Runtime {
		t.Helper()
		runner := &adapters.Runner{}
		t.Cleanup(func() { _ = runner.Close() })
		r := &Runtime{Store: base.Store, Outcomes: base.Outcomes, WorkerID: uuid.NewString(), HostID: string(role) + "-test",
			Host: &executor.Host{Store: base.Store, Runner: runner, Artifacts: base.Host.Artifacts}}
		handlers := r.Handlers()
		switch role {
		case execution.RoleAPI:
			handlers = Handlers{}
		case execution.RoleScheduler:
			handlers.Execute, handlers.Cleanup = nil, nil
		case execution.RoleExecutor:
			handlers.Advance, handlers.Finalize = nil, nil
		}
		var err error
		r.Client, err = New(base.Store.Pool, Config{Role: role, EngineID: base.Store.EngineID, HostID: r.HostID,
			Schema: base.Store.Pool.Config().ConnConfig.RuntimeParams["search_path"], Logger: base.Client.logger,
			ExecutionWorkers: 2, ExecutionTimeout: time.Minute}, handlers)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stop, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := r.Stop(stop); err != nil {
				t.Error(err)
			}
		})
		return r
	}
	api, scheduler, worker := newRole(execution.RoleAPI), newRole(execution.RoleScheduler), newRole(execution.RoleExecutor)
	if err := api.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id := admitRuntimeRun(t, api, runtimePlan(server.URL))
	await(t, 5*time.Second, func() (bool, error) {
		var queued int
		err := base.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE queue=$1 AND state='available'", worker.Client.execute).Scan(&queued)
		return queued > 0, err
	})
	if calls.Load() != 0 {
		t.Fatal("API or scheduler performed physical execution without an executor")
	}
	var attempt execution.AttemptID
	attempt.RunID = id
	var generation int64
	if err := base.Store.Pool.QueryRow(ctx, `SELECT instance_id,number,dispatch_generation
		FROM knotra_execution_attempts WHERE run_id=$1 AND state='ready' ORDER BY instance_id LIMIT 1`, id).
		Scan(&attempt.InstanceID, &attempt.Number, &generation); err != nil {
		t.Fatal(err)
	}
	for _, nonExecutor := range []*Runtime{api, scheduler} {
		if err := pgx.BeginFunc(ctx, base.Store.Pool, func(tx pgx.Tx) error {
			claim, err := base.Store.ClaimAttempt(ctx, tx, attempt, generation, nonExecutor.WorkerID)
			if err != nil {
				return err
			}
			if claim != nil {
				return errors.New("non-executor role claimed a physical attempt")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		status, err := store.ReadRunStatus(ctx, base.Store.Pool, id)
		return protocol.Terminal(status), err
	})
	run, err := base.Store.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != `"complete role-split output"` || len(run.Instances) != 3 {
		t.Fatalf("split-role result=%+v error=%v", run, err)
	}
	var used, owners int
	if err := base.Store.Pool.QueryRow(ctx, `SELECT
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
		(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND owner=$2 AND state='completed')`, id, worker.WorkerID).Scan(&used, &owners); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || used != 3 || owners != 3 {
		t.Fatalf("physical calls=%d budget=%d executor-owned outcomes=%d", calls.Load(), used, owners)
	}
}
