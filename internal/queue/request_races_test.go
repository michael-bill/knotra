package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/api"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/store"
)

const requestRaceBlockersSQL = `WITH RECURSIVE blocked(pid) AS (
	SELECT $1::integer UNION
	SELECT a.pid FROM blocked b JOIN pg_stat_activity a ON b.pid=ANY(pg_blocking_pids(a.pid))
) `

func TestRuntimeRequestCommandsArbitrateUnderContention(t *testing.T) {
	for _, kind := range []string{"human", "resolution"} {
		modes := []string{"different_keys", "same_key", "same_key_different_body", "answer_then_cancel", "cancel_then_answer", "accepted_before_deadline", "wait_past_deadline", "accepted_before_root_deadline", "wait_past_root_deadline"}
		if kind == "resolution" {
			modes = append(modes, "answer_then_failed", "failed_then_answer")
		}
		for _, mode := range modes {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				r := newTestRuntime(t)
				ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
				defer cancel()
				rootExpiry := mode == "accepted_before_root_deadline" || mode == "wait_past_root_deadline"
				waitsPast := mode == "wait_past_deadline" || mode == "wait_past_root_deadline"
				delayedAcceptance := mode == "accepted_before_deadline" || mode == "accepted_before_root_deadline"
				var calls atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					_, _ = fmt.Fprint(w, `{"message":{"role":"assistant","content":"{\"approved\":true}"},"done":true,"done_reason":"stop"}`)
				}))
				defer provider.Close()
				plan := runtimePlan(provider.URL)
				port := contract.Port{Schema: json.RawMessage(`{"type":"boolean"}`)}
				human := contract.Node{Type: "human", Human: &contract.HumanNode{Prompt: contract.TextSource{Text: "Approve?"}}, Outputs: map[string]contract.Port{"approved": port}, Execution: contract.Execution{Timeout: "30s"}}
				review := human
				if kind == "resolution" {
					review.Type, review.Human = "llm", nil
					review.LLM = &contract.LLMNode{Model: "model", Prompt: contract.TextSource{Text: "Return approval."}}
					if _, err := r.Store.Pool.Exec(ctx, `CREATE FUNCTION lose_race_response() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.completed THEN RAISE EXCEPTION 'fixture lost model reply'; END IF; RETURN NEW; END $$;
						CREATE TRIGGER lose_race_response BEFORE UPDATE ON knotra_operations FOR EACH ROW EXECUTE FUNCTION lose_race_response()`); err != nil {
						t.Fatal(err)
					}
				}
				if delayedAcceptance || waitsPast {
					review.Execution.Timeout = "3s"
				}
				if rootExpiry {
					plan.Profile.Spec.Limits.Timeout = "3s"
				}
				export := port
				export.Bind = &contract.Binding{From: "nodes.review.outputs.approved"}
				plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"review": review, "wait": human}, Outputs: map[string]contract.Port{"result": export}}
				if err := r.Start(ctx); err != nil {
					t.Fatal(err)
				}
				id := admitRuntimeRun(t, r, plan)
				instance := execution.StableID("n", id+"/root/review")
				await(t, 5*time.Second, func() (bool, error) {
					var open int
					err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'", id).Scan(&open)
					return open == 2, err
				})
				var requestID string
				var deadline, rootDeadline time.Time
				if err := r.Store.Pool.QueryRow(ctx, `SELECT q.id,(q.document->>'deadline')::timestamptz,r.execution_deadline
					FROM knotra_requests q JOIN knotra_runs r ON r.id=q.run_id WHERE q.run_id=$1 AND q.document->>'instanceId'=$2`, id, instance).Scan(&requestID, &deadline, &rootDeadline); err != nil {
					t.Fatal(err)
				}
				handler := (&api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}).Handler()
				path, body := "/v1/requests/"+requestID+"/response", `{"outputs":{"approved":true}}`
				otherBody := `{"outputs":{"approved":false}}`
				acceptedCode, acceptedStatus, acceptanceSQL := 200, "accepted", "AcceptHumanAnswer"
				if kind == "resolution" {
					path = "/v1/runs/" + id + "/instances/" + instance + "/resolve"
					body = `{"outcome":"succeeded","evidence":"provider record","outputs":{"approved":true}}`
					otherBody = `{"outcome":"succeeded","evidence":"different record","outputs":{"approved":false}}`
					acceptedCode, acceptedStatus, acceptanceSQL = 202, "resolved", "AcceptResolution"
				}
				firstPath, secondPath, firstBody, secondBody := path, path, body, otherBody
				firstKey, secondKey := "answer-first", "answer-second"
				firstCode, secondCode := acceptedCode, 409
				wantState, wantRequest, wantRun := "succeeded", acceptedStatus, "running"
				if mode == "same_key" {
					secondKey, secondBody, secondCode = firstKey, firstBody, acceptedCode
				}
				if mode == "same_key_different_body" {
					secondKey = firstKey
				}
				if mode == "answer_then_failed" {
					secondBody = `{"outcome":"failed","evidence":"operator failure record"}`
				}
				if mode == "failed_then_answer" {
					firstBody = `{"outcome":"failed","evidence":"operator failure record"}`
					wantState, wantRun = "failed", "failed"
				}
				if mode == "answer_then_cancel" {
					secondPath, secondBody, secondCode = "/v1/runs/"+id+"/cancel", `{}`, 202
					wantRun = "cancelled"
				}
				if mode == "cancel_then_answer" {
					firstPath, firstBody, firstCode = "/v1/runs/"+id+"/cancel", `{}`, 202
					wantState, wantRequest, wantRun = "cancelled", "cancelled", "cancelled"
				}
				if waitsPast {
					firstCode = 409
					wantState, wantRequest, wantRun = "failed", "expired", "failed"
					if rootExpiry {
						wantState = "cancelled"
					}
				}
				if rootExpiry {
					wantRun = "failed"
				}
				barrier, err := r.Store.Pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer barrier.Release()
				barrierPID := int32(barrier.Conn().PgConn().PID())
				var locked pgx.Tx
				var middleDone <-chan error
				if waitsPast {
					locked, err = barrier.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = locked.Rollback(context.Background()) }()
					if _, err := locked.Exec(ctx, "SELECT id FROM knotra_runs WHERE id=$1 FOR UPDATE", id); err != nil {
						t.Fatal(err)
					}
					// A queued tuple-lock holder can become the API's immediate
					// blocker while itself waiting on this transaction. Exercise
					// that chain instead of assuming a direct pg_blocking_pids edge.
					pending, pid := make(chan error, 1), make(chan int32, 1)
					middleDone = pending
					go func() {
						pending <- pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
							pid <- int32(tx.Conn().PgConn().PID())
							state, err := store.LockExecutionRun(ctx, tx, id)
							if err == nil && state.Now.Before(deadline) {
								return fmt.Errorf("database clock preceded the expired lock wait")
							}
							return err
						})
					}()
					middlePID := <-pid
					await(t, 5*time.Second, func() (bool, error) {
						var blocked bool
						// A maintenance transaction can hold the queued tuple lock
						// ahead of this waiter. Follow the same blocking ancestry as
						// the API check below instead of requiring a direct edge.
						err := r.Store.Pool.QueryRow(ctx, requestRaceBlockersSQL+"SELECT $2 IN (SELECT pid FROM blocked)", barrierPID, middlePID).Scan(&blocked)
						return blocked, err
					})
				} else {
					if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273653)"); err != nil {
						t.Fatal(err)
					}
					defer func() { _, _ = barrier.Exec(context.Background(), "SELECT pg_advisory_unlock(918273653)") }()
					table, when := "knotra_requests", "OLD.status='open' AND NEW.status IN ('answered','resolved')"
					if mode == "cancel_then_answer" {
						table, when = "knotra_runs", "NOT OLD.cancel_requested AND NEW.cancel_requested"
						acceptanceSQL = "SetRunCancelled"
					}
					if _, err := r.Store.Pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION request_race_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273653); RETURN NEW; END $$;
						CREATE TRIGGER request_race_barrier BEFORE UPDATE ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION request_race_barrier()`, table, when)); err != nil {
						t.Fatal(err)
					}
				}
				first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
				go func() { first <- runtimeCommand(t, handler, firstPath, firstKey, firstBody) }()
				var firstPID int32
				waitSQL := acceptanceSQL
				if locked != nil {
					waitSQL = "LockRunCommand"
				}
				await(t, 5*time.Second, func() (bool, error) {
					err := r.Store.Pool.QueryRow(ctx, requestRaceBlockersSQL+`SELECT COALESCE((SELECT pid FROM pg_stat_activity WHERE pid<>$1 AND pid IN (SELECT pid FROM blocked) AND query LIKE '%'||$2||'%' LIMIT 1),0)`, barrierPID, waitSQL).Scan(&firstPID)
					return firstPID != 0, err
				})
				if locked == nil {
					go func() { second <- runtimeCommand(t, handler, secondPath, secondKey, secondBody) }()
					waitSQL = "LockRunCommand"
					if secondKey == firstKey {
						waitSQL = "LockTransactionKey"
					}
					await(t, 5*time.Second, func() (bool, error) {
						var blocked bool
						err := r.Store.Pool.QueryRow(ctx, requestRaceBlockersSQL+`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid<>$1 AND pid IN (SELECT pid FROM blocked) AND query LIKE '%'||$2||'%')`, firstPID, waitSQL).Scan(&blocked)
						return blocked, err
					})
				}
				if delayedAcceptance || waitsPast {
					await(t, 5*time.Second, func() (bool, error) {
						var expired bool
						err := r.Store.Pool.QueryRow(ctx, "SELECT clock_timestamp() > $1::timestamptz", deadline).Scan(&expired)
						return expired, err
					})
				}
				if locked != nil {
					if err := r.advanceTx(ctx, locked, plan, id, ""); err != nil {
						t.Fatal(err)
					}
					if err := locked.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					if err := <-middleDone; err != nil {
						t.Fatal(err)
					}
					go func() { second <- runtimeCommand(t, handler, secondPath, secondKey, secondBody) }()
				} else if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273653)"); err != nil {
					t.Fatal(err)
				}
				var replies [2]*httptest.ResponseRecorder
				for i, pending := range []chan *httptest.ResponseRecorder{first, second} {
					select {
					case replies[i] = <-pending:
					case <-ctx.Done():
						t.Fatal("concurrent command did not finish", ctx.Err())
					}
				}
				if replies[0].Code != firstCode || replies[1].Code != secondCode {
					t.Fatalf("concurrent replies=%d:%s / %d:%s, want %d/%d", replies[0].Code, replies[0].Body.String(), replies[1].Code, replies[1].Body.String(), firstCode, secondCode)
				}
				if mode == "same_key" && replies[0].Body.String() != replies[1].Body.String() {
					t.Fatal("concurrent duplicate changed its receipt")
				}
				for i, command := range []struct{ path, key, body string }{{firstPath, firstKey, firstBody}, {secondPath, secondKey, secondBody}} {
					replayed := runtimeCommand(t, handler, command.path, command.key, command.body)
					if replayed.Code != replies[i].Code || replayed.Body.String() != replies[i].Body.String() {
						t.Fatal("winning or losing receipt changed on replay")
					}
				}
				if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error { return r.advanceTx(ctx, tx, plan, id, "") }); err != nil {
					t.Fatal(err)
				}
				var node, request, run, output, responseID, stopCode string
				var attempts, budget, slots, open, receipts, events, outbox int
				var finalDeadline, finalRootDeadline, finalRequestDeadline time.Time
				var acceptedBefore bool
				if err := r.Store.Pool.QueryRow(ctx, `SELECT n.state,q.status,r.document->>'status',COALESCE(n.outputs->'approved'->>'json',''),COALESCE(q.response_id,''),COALESCE(r.stop_cause->>'code',''),
					(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
					COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),0),
					(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1),
					(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'),
					(SELECT count(*) FROM knotra_commands WHERE id IN ('answer-first','answer-second')),
					(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->'data'->>'nodeId'='review' AND document->'data'->>'status'='succeeded'),
					(SELECT count(*) FROM knotra_outbox WHERE run_id=$1),n.deadline,r.execution_deadline,(q.document->>'deadline')::timestamptz,
					COALESCE(q.accepted_at < n.deadline,false)
					FROM knotra_runs r JOIN knotra_execution_nodes n ON n.run_id=r.id AND n.id=$2 JOIN knotra_requests q ON q.id=$3 WHERE r.id=$1`, id, instance, requestID).
					Scan(&node, &request, &run, &output, &responseID, &stopCode, &attempts, &budget, &slots, &open, &receipts, &events, &outbox, &finalDeadline, &finalRootDeadline, &finalRequestDeadline, &acceptedBefore); err != nil {
					t.Fatal(err)
				}
				wantAttempts, wantOpen, wantReceipts, wantEvents := 0, 1, 2, 1
				wantOutput, wantResponse, wantStop, wantAccepted := "true", firstKey, "", true
				if kind == "resolution" {
					wantAttempts = 1
				}
				if secondKey == firstKey {
					wantReceipts = 1
				}
				if wantRun == "cancelled" || wantRun == "failed" {
					wantOpen = 0
					wantStop = "CANCELLED"
				}
				if wantState != "succeeded" {
					wantOutput, wantEvents = "", 0
					if mode != "failed_then_answer" {
						wantResponse, wantAccepted = "", false
					}
				}
				if wantRun == "failed" {
					wantStop = "DEADLINE_EXCEEDED"
				}
				if mode == "failed_then_answer" {
					wantStop = "EXTERNAL_FAILED"
				}
				if node != wantState || request != wantRequest || run != wantRun || output != wantOutput || responseID != wantResponse || stopCode != wantStop || attempts != wantAttempts || budget != wantAttempts || calls.Load() != int32(wantAttempts) || slots != 0 || open != wantOpen || receipts != wantReceipts || events != wantEvents || outbox != 0 || acceptedBefore != wantAccepted || !deadline.Equal(finalDeadline) || !deadline.Equal(finalRequestDeadline) || !rootDeadline.Equal(finalRootDeadline) {
					t.Fatalf("node/request/run=%s/%s/%s output=%s response=%s cause=%s attempts/budget/calls=%d/%d/%d slots=%d open=%d receipts=%d events=%d outbox=%d acceptedBefore=%v deadlines=%v/%v/%v", node, request, run, output, responseID, stopCode, attempts, budget, calls.Load(), slots, open, receipts, events, outbox, acceptedBefore, deadline.Equal(finalDeadline), deadline.Equal(finalRequestDeadline), rootDeadline.Equal(finalRootDeadline))
				}
			})
		}
	}
}
