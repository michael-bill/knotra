package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/adapters"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func claimedHost(t *testing.T, endpoint string) (*Host, execution.ExecuteRequest) {
	t.Helper()
	dsn := os.Getenv("KNOTRA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL for executor PostgreSQL tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "executor_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		_ = admin.Close(ctx)
	})
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	node := contract.Node{Type: "llm", Outputs: map[string]contract.Port{"answer": {Schema: json.RawMessage(`{"type":"integer"}`)}},
		LLM: &contract.LLMNode{Model: "model", Prompt: contract.TextSource{Text: "Return answer 42."}}}
	limits := contract.Limits{Timeout: "1h", MaxConcurrentNodes: 2, MaxNodeInstances: 100, MaxModelCalls: 2, MaxToolCalls: 2}
	plan := contract.Plan{Version: "knotra/v1", CompilerVersion: contract.CompilerVersion, CELVersion: contract.CELVersion, Root: "main.yaml", Digest: "frozen-plan",
		Pipelines: map[string]*contract.Pipeline{"main.yaml": {Spec: contract.Spec{Graph: contract.Graph{Nodes: map[string]contract.Node{"model": node}},
			Models: map[string]contract.Model{"model": {Connection: "local"}}}}},
		Profile: contract.Profile{Spec: contract.ProfileSpec{Limits: limits,
			Models: map[string]contract.ModelConnection{"local": {Provider: "ollama", Model: "fixture", BaseURL: endpoint}}}}}
	definition := protocol.Definition{ID: uuid.NewString(), PackageDigest: uuid.NewString(), Name: "executor-test"}
	run := protocol.Run{ID: uuid.NewString(), DefinitionID: definition.ID, Status: "pending"}
	id := execution.AttemptID{RunID: run.ID, InstanceID: "model", Number: 1}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := store.PutDefinition(ctx, tx, definition); err != nil {
			return err
		}
		if _, err := store.PutRiverRun(ctx, tx, run, plan, contract.Values{}, func(context.Context, pgx.Tx, string, int64) error { return nil }); err != nil {
			return err
		}
		state, err := store.LockExecutionRun(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		scopes := []execution.BudgetScope{{ID: run.ID, Limits: limits}}
		b, err := json.Marshal(scopes)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO knotra_execution_graphs(run_id,id,address,pipeline,graph_path,inputs,scopes,deadline,state)
			VALUES($1,'root','root','main.yaml','root','{}',$2,$3,'running')`, run.ID, b, state.Deadline); err != nil {
			return err
		}
		q := execution.ExecuteRequest{RunID: run.ID, InstanceID: id.InstanceID, NodeID: "model", Pipeline: "main.yaml", Attempt: 1, Node: node, Inputs: contract.Values{}, Scopes: scopes, Deadline: state.Deadline}
		b, err = json.Marshal(q)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO knotra_execution_nodes(run_id,id,graph_id,node_id,address,state,execution_request,deadline,attempt_number)
			VALUES($1,$2,'root','model','root/model','ready',$3,$4,1)`, run.ID, id.InstanceID, b, state.Deadline); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO knotra_execution_attempts(run_id,instance_id,number,dispatch_generation,state) VALUES($1,$2,1,1,'ready')`, run.ID, id.InstanceID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	workerID := uuid.NewString()
	if err := s.RegisterWorker(ctx, workerID, "test-host"); err != nil {
		t.Fatal(err)
	}
	var claim *execution.Claim
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		claim, err = s.ClaimAttempt(ctx, tx, id, 1, workerID)
		return err
	})
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v error=%v", claim, err)
	}
	runner := &adapters.Runner{}
	t.Cleanup(func() { _ = runner.Close() })
	return &Host{Store: s, Artifacts: store.Artifacts{Root: t.TempDir(), Store: s}, Runner: runner}, claim.Request
}

const fixtureResponse = `{"message":{"role":"assistant","content":"{\"answer\":42}"},"done":true,"done_reason":"stop"}`

func TestHostReplaysCompletedProviderResponseWithoutNewBudget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
		}
		calls.Add(1)
		_, _ = fmt.Fprint(w, fixtureResponse)
	}))
	defer server.Close()
	h, q := claimedHost(t, server.URL)
	ctx := context.Background()
	for range 2 {
		result := h.Execute(ctx, q)
		if result.Failure != nil || string(result.Outputs["answer"].JSON) != "42" {
			t.Fatalf("provider result=%+v", result)
		}
	}
	var used, completed int
	if err := h.Store.Pool.QueryRow(ctx, `SELECT
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
		(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND completed)`, q.RunID).Scan(&used, &completed); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || used != 1 || completed != 1 {
		t.Fatalf("completed operation repeated: physical=%d budget=%d completed=%d", calls.Load(), used, completed)
	}
	q.Ownership = nil
	if result := h.Execute(ctx, q); result.Failure == nil || result.Failure.Code != "OWNERSHIP_REQUIRED" || calls.Load() != 1 {
		t.Fatalf("unowned host bypassed claim: %+v calls=%d", result, calls.Load())
	}
}

func TestHostRechecksOwnershipAfterProviderPreparation(t *testing.T) {
	var calls atomic.Int32
	var h *Host
	var q execution.ExecuteRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			if _, err := h.Store.Pool.Exec(r.Context(), "UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1", q.RunID); err != nil {
				t.Error(err)
			}
			_, _ = fmt.Fprint(w, `{"models":[{"name":"fixture","digest":"admitted"}]}`)
			return
		}
		calls.Add(1)
		_, _ = fmt.Fprint(w, fixtureResponse)
	}))
	defer server.Close()
	h, q = claimedHost(t, server.URL)
	ctx := context.Background()
	// A frozen digest triggers the real provider preparation request.
	if _, err := h.Store.Pool.Exec(ctx, `UPDATE knotra_runs SET plan=(plan::jsonb || '{"modelDigests":{"local/fixture":"admitted"}}')::json WHERE id=$1`, q.RunID); err != nil {
		t.Fatal(err)
	}
	result := h.Execute(ctx, q)
	if result.Failure == nil || !result.Failure.Unknown || calls.Load() != 0 {
		t.Fatalf("expired owner generated after preparation: %+v physical=%d", result, calls.Load())
	}
	var budgets, admitted int
	if err := h.Store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM knotra_budgets WHERE run_id=$1),
		(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND admitted_at IS NOT NULL)`, q.RunID).Scan(&budgets, &admitted); err != nil {
		t.Fatal(err)
	}
	if budgets != 0 || admitted != 0 {
		t.Fatalf("preparation spent budget or admitted an expired call: budgets=%d intents=%d", budgets, admitted)
	}
}

func TestHostLostOwnershipCannotConfirmExternalResponse(t *testing.T) {
	var calls atomic.Int32
	var h *Host
	var q execution.ExecuteRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// External work has happened, but the original owner's lease has expired
		// before its response can be journaled. The envelope must remain unknown.
		if _, err := h.Store.Pool.Exec(r.Context(), "UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1", q.RunID); err != nil {
			t.Error(err)
		}
		_, _ = fmt.Fprint(w, fixtureResponse)
	}))
	defer server.Close()
	h, q = claimedHost(t, server.URL)
	ctx := context.Background()
	result := h.Execute(ctx, q)
	if result.Failure == nil || !result.Failure.Unknown || len(result.Outputs) != 0 || calls.Load() != 1 {
		t.Fatalf("unjournaled response became success: %+v calls=%d", result, calls.Load())
	}
	files, err := execution.OpenOutcomeFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	key, err := files.Put(execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: *q.Ownership,
		PlanID: "frozen-plan", CompletedAt: time.Now(), Failure: result.Failure})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := files.Get(key)
	if err != nil || saved.Failure == nil || !saved.Failure.Unknown || len(saved.Outputs) != 0 {
		t.Fatalf("durable handoff erased uncertainty: %+v %v", saved, err)
	}
	if again := h.Execute(ctx, q); again.Failure == nil || !again.Failure.Unknown || calls.Load() != 1 {
		t.Fatalf("lost owner repeated a physical call: %+v calls=%d", again, calls.Load())
	}
	var used, unconfirmed int
	if err := h.Store.Pool.QueryRow(ctx, `SELECT
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
		(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND admitted_at IS NOT NULL AND NOT completed)`, q.RunID).Scan(&used, &unconfirmed); err != nil {
		t.Fatal(err)
	}
	if used != 1 || unconfirmed != 1 {
		t.Fatalf("external evidence lost: budget=%d unconfirmed=%d", used, unconfirmed)
	}
}
