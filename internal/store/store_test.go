package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/engine"
	"github.com/michael-bill/knotra/internal/protocol"
)

// Integration tests create isolated schemas and never reset a developer's DB.
// Run with KNOTRA_TEST_DATABASE_URL set to a disposable/local PostgreSQL DSN.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("KNOTRA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "knotra_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	scoped := dsn
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		scoped = u.String()
	} else {
		scoped += " search_path=" + schema
	}
	db, err := Open(ctx, scoped)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		if err := admin.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	return db
}

func setupRun(t *testing.T, s *Store) (string, contract.Plan) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	id := uuid.NewString()
	def := protocol.Definition{ID: uuid.NewString(), PackageDigest: "sha256:" + strings.Repeat("a", 64), Name: "test", CreatedAt: time.Now().UTC()}
	if _, err := PutDefinition(ctx, tx, def); err != nil {
		t.Fatal(err)
	}
	plan := contract.Plan{Version: "knotra/v1", CompilerVersion: contract.CompilerVersion, CELVersion: contract.CELVersion, Root: "pipeline.yaml", Pipelines: map[string]*contract.Pipeline{"pipeline.yaml": {Spec: contract.Spec{Graph: contract.Graph{Inputs: map[string]contract.Port{}, Outputs: map[string]contract.Port{}, Nodes: map[string]contract.Node{}}}}}}
	run := protocol.Run{ID: id, DefinitionID: def.ID, Status: "pending", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := PutRun(ctx, tx, run, plan, contract.Values{}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id, plan
}

func TestCommandConcurrentIdempotency(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, "CREATE TABLE test_effects (id integer PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var wg sync.WaitGroup
	failures := make(chan error, 24)
	for range 24 {
		wg.Go(func() {
			status, response, err := s.Command(ctx, "principal", "same", "POST /effect", []byte(`{"x":1}`), func(tx pgx.Tx) (int, any, error) {
				calls.Add(1)
				_, err := tx.Exec(ctx, "INSERT INTO test_effects(id) VALUES(1)")
				return 201, map[string]any{"ok": true}, err
			})
			if err != nil {
				failures <- err
				return
			}
			if status != 201 || string(response) != `{"ok":true}` {
				failures <- fmt.Errorf("unexpected receipt %d %s", status, response)
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("action ran %d times", calls.Load())
	}
	for _, test := range []struct{ route, body string }{{"POST /effect", `{"x":2}`}, {"POST /different", `{"x":1}`}} {
		_, _, err := s.Command(ctx, "principal", "same", test.route, []byte(test.body), func(pgx.Tx) (int, any, error) { t.Error("conflicting command action invoked"); return 200, nil, nil })
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected conflict, got %v", err)
		}
	}
}

func TestCommandRollbackIncludesEffectAndReceipt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, "CREATE TABLE test_effects (id integer PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Command(ctx, "p", "rollback", "POST /effect", []byte(`{}`), func(tx pgx.Tx) (int, any, error) {
		if _, err := tx.Exec(ctx, "INSERT INTO test_effects(id) VALUES(1)"); err != nil {
			return 0, nil, err
		}
		return 200, make(chan int), nil
	})
	if err == nil {
		t.Fatal("unsupported response did not fail")
	}
	for _, table := range []string{"test_effects", "knotra_commands"} {
		var count int
		if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s was not rolled back", table)
		}
	}
}

func TestHumanFirstValidResponseWins(t *testing.T) {
	s := testStore(t)
	runID, _ := setupRun(t, s)
	ctx := context.Background()
	request := engine.Request{ID: uuid.NewString(), RunID: runID, InstanceID: runID + "/root/human", Kind: "human", Status: "open", Deadline: time.Now().Add(time.Hour), Outputs: map[string]contract.Port{"value": {Schema: json.RawMessage(`{"type":"integer"}`)}}}
	if err := s.SaveRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			key := fmt.Sprintf("response-%d", i)
			_, _, err := s.Command(ctx, "p", key, "POST /response", []byte(key), func(tx pgx.Tx) (int, any, error) {
				err := Respond(ctx, tx, request.ID, key, contract.Values{"value": {JSON: json.RawMessage(`1e0`)}})
				return 200, map[string]bool{"accepted": true}, err
			})
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if winners.Load() != 1 {
		t.Fatalf("expected one accepted answer, got %d", winners.Load())
	}
	accepted, err := s.Answer(ctx, engine.AnswerRequest{RunID: runID, RequestID: request.ID, CloseIfAbsent: "expired"})
	if err != nil || accepted == nil {
		t.Fatalf("committed answer discarded by timeout: %v", err)
	}
	decoded, err := contract.DecodeJSON(accepted.Values["value"].JSON)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.(float64); !ok {
		t.Fatalf("database lost double token type: %#v", decoded)
	}
	var count int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_outbox WHERE run_id=$1 AND kind='human'", runID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one durable human signal, got %d", count)
	}
	request.Status = "expired"
	if err := s.SaveRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Request(ctx, request.ID)
	if err != nil || saved.Status != "answered" {
		t.Fatal("late status projection overwrote accepted answer", saved.Status, err)
	}
}

func TestHumanRejectsInvalidExpiredAndWrongKind(t *testing.T) {
	s := testStore(t)
	runID, _ := setupRun(t, s)
	ctx := context.Background()
	for _, test := range []struct {
		name, kind string
		deadline   time.Time
		value      string
		conflict   bool
	}{{"invalid", "human", time.Now().Add(time.Hour), `"wrong"`, false}, {"expired", "human", time.Now().Add(-time.Second), `1`, true}, {"resolution", "resolution", time.Now().Add(time.Hour), `1`, true}} {
		t.Run(test.name, func(t *testing.T) {
			request := engine.Request{ID: uuid.NewString(), RunID: runID, Kind: test.kind, Status: "open", Deadline: test.deadline, Outputs: map[string]contract.Port{"value": {Schema: json.RawMessage(`{"type":"integer"}`)}}}
			if err := s.SaveRequest(ctx, request); err != nil {
				t.Fatal(err)
			}
			tx, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			err = Respond(ctx, tx, request.ID, "response", contract.Values{"value": {JSON: json.RawMessage(test.value)}})
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			if test.conflict && !errors.Is(err, ErrConflict) {
				t.Fatalf("expected conflict, got %v", err)
			}
		})
	}
}

func TestBudgetAtomicAcrossScopes(t *testing.T) {
	s := testStore(t)
	runID, _ := setupRun(t, s)
	ctx := context.Background()
	scopes := []engine.BudgetScope{{ID: runID, Limits: contract.Limits{MaxModelCalls: 3}}, {ID: runID + "/child", Limits: contract.Limits{MaxModelCalls: 1}}}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if s.Reserve(ctx, runID, "model", scopes) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("overspent child budget: %d", accepted.Load())
	}
	rows, err := s.Pool.Query(ctx, "SELECT scope,used FROM knotra_budgets WHERE run_id=$1", runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var scope string
		var used int
		if err := rows.Scan(&scope, &used); err != nil {
			t.Fatal(err)
		}
		if used != 1 {
			t.Fatalf("partial budget reservations persisted for %s: %d", scope, used)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatal("missing ancestor counters")
	}
}

func TestProjectionOrderingAndDeduplication(t *testing.T) {
	s := testStore(t)
	runID, _ := setupRun(t, s)
	ctx := context.Background()
	now := time.Now().UTC()
	events := []engine.Projection{{RunID: runID, Sequence: 1, Kind: "run", Status: "running", Time: now}, {RunID: runID, Sequence: 3, InstanceID: "node", NodeID: "node", Kind: "node", Status: "succeeded", Time: now}, {RunID: runID, Sequence: 2, Kind: "run", Status: "waiting_human", Time: now}, {RunID: runID, Sequence: 4, Kind: "run", Status: "succeeded", Time: now}, {RunID: runID, Sequence: 5, Kind: "run", Status: "running", Time: now}}
	for _, event := range events {
		if err := s.Project(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Project(ctx, events[1]); err != nil {
		t.Fatal(err)
	}
	run, err := s.Run(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "succeeded" {
		t.Fatal("terminal state regressed", run.Status)
	}
	if len(run.Instances) != 1 || run.Instances[0].Status != "succeeded" {
		t.Fatal(run.Instances)
	}
	stored, err := s.Events(ctx, runID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 5 {
		t.Fatalf("duplicate event persisted: %d", len(stored))
	}
	next, err := s.Events(ctx, runID, stored[2].ID)
	if err != nil || len(next) != 2 {
		t.Fatal("event cursor replay failed", len(next), err)
	}
}

func TestOperationIntentAndCompletion(t *testing.T) {
	s := testStore(t)
	runID, _ := setupRun(t, s)
	ctx := context.Background()
	operation := uuid.NewString()
	first, err := s.BeginOperation(ctx, operation, runID, "tool", "write")
	if err != nil || first.Started || first.Completed {
		t.Fatal(first, err)
	}
	second, err := s.BeginOperation(ctx, operation, runID, "tool", "write")
	if err != nil || !second.Started || second.Completed {
		t.Fatal(second, err)
	}
	if err := s.CompleteOperation(ctx, operation, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOperation(ctx, operation, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOperation(ctx, operation, json.RawMessage(`{"ok":false}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("different completion accepted", err)
	}
	third, err := s.BeginOperation(ctx, operation, runID, "tool", "write")
	if err != nil || !third.Completed || string(third.Response) != `{"ok":true}` {
		t.Fatal(third, err)
	}
}

func TestArtifactIntegrity(t *testing.T) {
	s := testStore(t)
	storage := Artifacts{Root: t.TempDir(), Store: s}
	ctx := context.Background()
	artifact, err := storage.Put(ctx, "test.txt", "text/plain", []byte("original"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := storage.Get(ctx, artifact.ID)
	if err != nil || string(got) != "original" {
		t.Fatal(string(got), err)
	}
	if err := os.WriteFile(storage.Root+"/"+artifact.SHA256, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Get(ctx, artifact.ID); err == nil {
		t.Fatal("corrupted artifact served")
	}
}

func TestStagedArtifactsPublishWithSuccessfulNode(t *testing.T) {
	s := testStore(t)
	id, _ := setupRun(t, s)
	ctx := context.Background()
	files := Artifacts{Root: t.TempDir(), Store: s}
	art, err := files.Put(ctx, "report", "text/plain", []byte("validated result"), map[string]string{"runId": id, "instanceId": "producer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Artifact(ctx, art.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("draft artifact escaped", err)
	}
	if _, err = files.Get(ctx, art.ID); err != nil {
		t.Fatal("trusted replay lost draft", err)
	}
	outputs := contract.Values{"file": {Artifacts: []contract.Artifact{art}}}
	if err = s.Project(ctx, engine.Projection{RunID: id, Sequence: 1, Kind: "node", InstanceID: "producer", NodeID: "producer", Status: "running", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Artifact(ctx, art.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("running node published draft")
	}
	if err = s.Project(ctx, engine.Projection{RunID: id, Sequence: 2, Kind: "node", InstanceID: "producer", NodeID: "producer", Status: "succeeded", Outputs: outputs, Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Artifact(ctx, art.ID); err != nil {
		t.Fatal("successful output not published", err)
	}
	if err = s.Project(ctx, engine.Projection{RunID: id, Sequence: 3, Kind: "request", InstanceID: "producer", Status: "rejected", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Run(ctx, id)
	if err != nil || len(run.Instances) != 1 || run.Instances[0].Status != "succeeded" {
		t.Fatalf("request event corrupted instance: %+v %v", run, err)
	}
}

func TestExclusiveServiceLeaseAndSingleConnectionDelivery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	lease, err := s.AcquireLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := s.AcquireLease(ctx); err == nil {
		other.Close(ctx)
		t.Fatal("two processes own one engine")
	}
	if err = lease.Close(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err = s.AcquireLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close(ctx)
	id, _ := setupRun(t, s)
	config := s.Pool.Config()
	config.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	s.Pool.Close()
	s.Pool = single
	deadline, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	// Delivery exposes its own transaction for state reads; no nested pool checkout.
	if err = s.Deliver(deadline, func(ctx context.Context, q Querier, runID, kind string, payload []byte) error {
		if runID != id || kind != "start" {
			t.Fatal(runID, kind)
		}
		status, err := ReadRunStatus(ctx, q, runID)
		if status != "pending" {
			t.Fatal(status)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionsRemainReachableBeyondFirstTwoHundred(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for i := 0; i < 205; i++ {
		d := protocol.Definition{ID: fmt.Sprintf("def-%03d", i), PackageDigest: fmt.Sprintf("digest-%03d", i), Name: "test", CreatedAt: time.Now()}
		if _, err = PutDefinition(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	count := 0
	cursor := ""
	for {
		items, err := s.Definitions(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		n := len(items)
		if n > 100 {
			n = 100
		}
		count += n
		cursor = items[n-1].ID
		if len(items) <= 100 {
			break
		}
	}
	if count != 205 {
		t.Fatalf("only %d definitions reachable", count)
	}
}
