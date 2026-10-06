//go:build unix

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func TestRiverStopAndSavedResultProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_HELPER") == "" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY, KNOTRA_TEST_HELPER and KNOTRA_TEST_DATABASE_URL for stop/result SIGKILL checks")
	}
	for _, mode := range []string{"cancel_before_recovered_result", "recovered_result_before_cancel", "result_committed_cancel_receipt_lost", "late_result_after_cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			root, err := filepath.Abs("../..")
			if err != nil {
				t.Fatal(err)
			}
			work := t.TempDir()
			profile := recoveryProfile()
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), DockerHost: os.Getenv("DOCKER_HOST"), HelperPath: os.Getenv("KNOTRA_TEST_HELPER"), Profiles: []string{profilePath}, Version: "stop-result-test"}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			firstLog := filepath.Join(work, "first.log")
			first := startEngineProcess(t, ctx, optionsPath, firstLog, api)
			engineID := engineIdentity(t, ctx, api)
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			schema := admin.Pool.Config().ConnConfig.RuntimeParams["search_path"]
			queue, err := river.NewClient(riverpgxv5.New(admin.Pool), &river.Config{Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			advanceQueue, finalizeQueue := "knotra_advance_"+admin.EngineID, "knotra_maintenance_"+admin.EngineID
			committed := mode == "result_committed_cancel_receipt_lost"
			var barrier *pgx.Conn
			if !committed {
				barrier, err = pgx.ConnectConfig(ctx, admin.Pool.Config().ConnConfig.Copy())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273662)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION stop_result_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273662); RETURN NEW; END $$;
					CREATE TRIGGER stop_result_barrier BEFORE UPDATE ON knotra_artifacts FOR EACH ROW WHEN (NOT OLD.published AND NEW.published) EXECUTE FUNCTION stop_result_barrier()`); err != nil {
					t.Fatal(err)
				}
			}
			pkg, err := contract.LoadPackage(filepath.Join(root, "examples/integration/recovery.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			pkg.Source = strings.Replace(pkg.Source, "    prepare:\n      type: code\n", "    prepare:\n      type: code\n      execution: {retry: {maxAttempts: 3, backoff: 1ms}}\n", 1)
			for i := range pkg.Files {
				if pkg.Files[i].Path == pkg.Entrypoint {
					pkg.Files[i].Content = []byte(pkg.Source)
				}
			}
			var definition struct{ Definition protocol.Definition }
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			marker := strings.Repeat("stop-result-input-", 10000)
			var admitted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name, "inputs": map[string]any{"marker": marker}}, uuid.NewString(), &admitted); err != nil {
				t.Fatal(err)
			}
			var publicationPID int32
			if committed {
				awaitHuman(t, ctx, api, admitted.Run.ID)
			} else {
				awaitLifecycleBoundary(t, ctx, first, firstLog, func() bool {
					if err := admin.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, int64(barrier.PgConn().PID())).Scan(&publicationPID); err != nil {
						t.Fatal(err)
					}
					return publicationPID != 0
				})
			}
			for _, name := range []string{advanceQueue, finalizeQueue} {
				if err := queue.QueuePause(ctx, name, nil); err != nil {
					t.Fatal(err)
				}
			}
			var instance, outcomeKey, sandbox string
			var rootDeadline, nodeDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT n.id,a.outcome_key,r.execution_deadline,n.deadline,
				(SELECT id FROM knotra_resources WHERE run_id=r.id AND kind='sandbox')
				FROM knotra_runs r JOIN knotra_execution_nodes n ON n.run_id=r.id JOIN knotra_execution_attempts a ON a.run_id=n.run_id AND a.instance_id=n.id
				WHERE r.id=$1 AND n.node_id='prepare'`, admitted.Run.ID).Scan(&instance, &outcomeKey, &rootDeadline, &nodeDeadline, &sandbox); err != nil {
				t.Fatal(err)
			}
			outcomes, err := execution.OpenOutcomeFiles(filepath.Join(options.DataDir, "outcomes"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = outcomes.Close() }()
			outcome, err := outcomes.Get(outcomeKey)
			if err != nil || outcome.Failure != nil || len(outcome.Artifacts) != 1 || len(outcome.Outputs["document"].Artifacts) != 1 {
				t.Fatalf("real saved code outcome: artifacts=%d failure=%v error=%v", len(outcome.Artifacts), outcome.Failure, err)
			}
			artifact := outcome.Outputs["document"].Artifacts[0]
			content, err := os.ReadFile(filepath.Join(options.DataDir, "artifacts", artifact.SHA256))
			sum := sha256.Sum256(content)
			var document struct{ Marker, Nonce string }
			if err != nil || hex.EncodeToString(sum[:]) != artifact.SHA256 || int64(len(content)) != artifact.Size {
				t.Fatalf("real artifact bytes=%d hash=%s error=%v", len(content), artifact.SHA256, err)
			}
			if err := json.Unmarshal(content, &document); err != nil || document.Marker != marker || document.Nonce == "" || string(outcome.Outputs["nonce"].JSON) != `"`+document.Nonce+`"` {
				t.Fatalf("real code marker/nonce mismatch: marker bytes=%d nonce=%q error=%v", len(document.Marker), document.Nonce, err)
			}
			outcomePath := filepath.Join(options.DataDir, "outcomes", outcomeKey+".json")
			originalEnvelope, err := os.ReadFile(outcomePath)
			if err != nil {
				t.Fatal(err)
			}
			path := "/runs/" + admitted.Run.ID + "/cancel"
			commands, received, release := commandReceiptProxy(t, ctx, api, "/v1"+path, filepath.Join(work, "cancel-client"), committed)
			key := uuid.NewString()
			done := make(chan error, 1)
			var committedReceipt []byte
			if mode != "late_result_after_cancel" {
				go func() { done <- commands.Command(ctx, path, map[string]any{}, key, new(any)) }()
				if committed {
					select {
					case receipt := <-received:
						if receipt.status != 202 {
							t.Fatalf("held cancel receipt=%d %s", receipt.status, receipt.data)
						}
						committedReceipt = receipt.data
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				} else {
					awaitLifecycleBoundary(t, ctx, first, firstLog, func() bool {
						var waiting bool
						if err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)) AND query LIKE '%LockRunCommand%')`, publicationPID).Scan(&waiting); err != nil {
							t.Fatal(err)
						}
						return waiting
					})
				}
			}
			first.kill(t)
			assertLifecycleSIGKILL(t, first)
			if barrier != nil {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273662)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER stop_result_barrier ON knotra_artifacts"); err != nil {
					t.Fatal(err)
				}
			}
			release()
			if mode != "late_result_after_cancel" {
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("cancel receipt survived the killed service")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				pending, err := commands.Commands()
				if err != nil || len(pending) != 1 || pending[0].ID != key || pending[0].Status != "pending" {
					t.Fatalf("pending cancel=%v error=%v", pending, err)
				}
			}
			var published, evidence, results, events, receipts int
			var cancelled bool
			if err := admin.Pool.QueryRow(ctx, `SELECT cancel_requested,
				(SELECT count(*) FROM knotra_artifacts WHERE id=$2 AND published),
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND outcome IS NOT NULL),
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND result IS NOT NULL),
				(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->'data'->>'nodeId'='prepare' AND document->'data'->>'status'='succeeded'),
				(SELECT count(*) FROM knotra_commands WHERE id=$3) FROM knotra_runs WHERE id=$1`, admitted.Run.ID, artifact.ID, key).Scan(&cancelled, &published, &evidence, &results, &events, &receipts); err != nil {
				t.Fatal(err)
			}
			want := 0
			if committed {
				want = 1
			}
			if cancelled != committed || published != want || evidence != want || results != want || events != want || receipts != want {
				t.Fatalf("post-crash cancel/publication/evidence/result/event/receipt=%v/%d/%d/%d/%d/%d want=%d", cancelled, published, evidence, results, events, receipts, want)
			}
			if mode == "late_result_after_cancel" {
				// Fault the actual fsynced envelope, then restore the same bytes after
				// the lost attempt and operator stop have both committed.
				if err := os.Rename(outcomePath, filepath.Join(work, "unavailable-envelope")); err != nil {
					t.Fatal(err)
				}
			}
			secondLog := filepath.Join(work, "second.log")
			second := startEngineProcess(t, ctx, optionsPath, secondLog, api)
			if engineIdentity(t, ctx, api) != engineID {
				t.Fatal("engine identity changed")
			}
			resultFirst := committed || mode == "recovered_result_before_cancel"
			if mode == "recovered_result_before_cancel" || mode == "late_result_after_cancel" {
				for _, name := range []string{finalizeQueue, advanceQueue} {
					if err := queue.QueueResume(ctx, name, nil); err != nil {
						t.Fatal(err)
					}
				}
				if resultFirst {
					awaitHuman(t, ctx, api, admitted.Run.ID)
				} else {
					awaitLifecycleBoundary(t, ctx, second, secondLog, func() bool {
						var status string
						if err := admin.Pool.QueryRow(ctx, "SELECT state FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2", admitted.Run.ID, instance).Scan(&status); err != nil {
							t.Fatal(err)
						}
						return status == "waiting_resolution"
					})
				}
			}
			var replay json.RawMessage
			if mode == "late_result_after_cancel" {
				err = commands.Command(ctx, path, map[string]any{}, key, &replay)
			} else {
				err = commands.Retry(ctx, key, &replay)
			}
			if err != nil || (committed && !bytes.Equal(replay, committedReceipt)) {
				t.Fatalf("cancel retry/receipt: %s error=%v", replay, err)
			}
			var duplicate json.RawMessage
			if err := api.Command(ctx, path, map[string]any{}, key, &duplicate); err != nil || !bytes.Equal(duplicate, replay) {
				t.Fatalf("fresh cancel receipt replay=%s error=%v", duplicate, err)
			}
			if err := queue.QueueResume(ctx, advanceQueue, nil); err != nil {
				t.Fatal(err)
			}
			if err := queue.QueueResume(ctx, finalizeQueue, nil); err != nil {
				t.Fatal(err)
			}
			final := awaitTerminal(t, ctx, api, admitted.Run.ID)
			if mode == "late_result_after_cancel" {
				if err := os.Rename(filepath.Join(work, "unavailable-envelope"), outcomePath); err != nil {
					t.Fatal(err)
				}
			}
			recovery, stopRecovery := context.WithTimeout(ctx, 30*time.Second)
			defer stopRecovery()
			var retainedCount, remainingSlots, remainingResources int
			defer func() {
				if t.Failed() {
					t.Logf("last late-evidence snapshot: retained=%d slots=%d resources=%d", retainedCount, remainingSlots, remainingResources)
				}
			}()
			awaitLifecycleBoundary(t, recovery, second, secondLog, func() bool {
				if err := admin.Pool.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND outcome IS NOT NULL),
					(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
					(SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND state<>'closed')`, final.ID).Scan(&retainedCount, &remainingSlots, &remainingResources); err != nil {
					t.Fatal(err)
				}
				return retainedCount == 1 && remainingSlots == 0 && remainingResources == 0
			})
			var nodeState, stopCode string
			var attempts, tools, confirmed, slots, requests, successes, terminalEvents, outbox, verify int
			var finalRootDeadline, finalNodeDeadline time.Time
			var storedOutcome []byte
			if err := admin.Pool.QueryRow(ctx, `SELECT r.stop_cause->>'code',r.execution_deadline,n.state,n.deadline,a.outcome,
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=r.id),
				(SELECT used FROM knotra_budgets WHERE run_id=r.id AND scope=r.id AND kind='tool'),
				(SELECT count(*) FROM knotra_operations WHERE run_id=r.id AND kind='tool' AND completed),
				(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=r.id AND id=r.id),
				(SELECT count(*) FROM knotra_requests WHERE run_id=r.id AND status='open'),
				(SELECT count(*) FROM knotra_events WHERE run_id=r.id AND document->'data'->>'nodeId'='prepare' AND document->'data'->>'status'='succeeded'),
				(SELECT count(*) FROM knotra_events WHERE run_id=r.id AND document->>'type'='run' AND document->'data'->>'status'='cancelled'),
				(SELECT count(*) FROM knotra_outbox),
				(SELECT count(*) FROM knotra_execution_attempts a JOIN knotra_execution_nodes n ON n.run_id=a.run_id AND n.id=a.instance_id WHERE a.run_id=r.id AND n.node_id='verify'),
				(SELECT count(*) FROM knotra_artifacts WHERE id=$3 AND published),
				(SELECT count(*) FROM knotra_commands WHERE id=$4)
				FROM knotra_runs r JOIN knotra_execution_nodes n ON n.run_id=r.id JOIN knotra_execution_attempts a ON a.run_id=n.run_id AND a.instance_id=n.id
				WHERE r.id=$1 AND n.id=$2`, final.ID, instance, artifact.ID, key).Scan(&stopCode, &finalRootDeadline, &nodeState, &finalNodeDeadline, &storedOutcome, &attempts, &tools, &confirmed, &slots, &requests, &successes, &terminalEvents, &outbox, &verify, &published, &receipts); err != nil {
				t.Fatal(err)
			}
			wantNode, wantPublished := "cancelled", 0
			if resultFirst {
				wantNode, wantPublished = "succeeded", 1
			}
			var retained execution.Outcome
			if err := json.Unmarshal(storedOutcome, &retained); err != nil || retained.Failure != nil || len(retained.Artifacts) != 1 || retained.Artifacts[0] != outcome.Artifacts[0] || string(retained.Outputs["nonce"].JSON) != string(outcome.Outputs["nonce"].JSON) {
				t.Fatalf("late outcome changed: failure=%v artifacts=%v error=%v", retained.Failure, retained.Artifacts, err)
			}
			if final.Status != "cancelled" || len(final.Outputs) != 0 || len(final.Artifacts) != 0 || stopCode != "CANCELLED" || nodeState != wantNode || attempts != 1 || tools != 1 || confirmed != 1 || slots != 0 || requests != 0 || successes != wantPublished || terminalEvents != 1 || outbox != 0 || verify != 0 || published != wantPublished || receipts != 1 || !rootDeadline.Equal(finalRootDeadline) || !nodeDeadline.Equal(finalNodeDeadline) {
				t.Fatalf("final stop/result: status=%s stop=%s node=%s attempts/tools/confirmed/slots/requests/successes/terminal/outbox/verify/published/receipts=%d/%d/%d/%d/%d/%d/%d/%d/%d/%d/%d", final.Status, stopCode, nodeState, attempts, tools, confirmed, slots, requests, successes, terminalEvents, outbox, verify, published, receipts)
			}
			download, err := api.Bytes(ctx, "/artifacts/"+artifact.ID+"/content")
			if (resultFirst && (err != nil || !bytes.Equal(download, content))) || (!resultFirst && (err == nil || !strings.Contains(err.Error(), "HTTP 404"))) {
				t.Fatalf("artifact visibility after stop: bytes=%d error=%v", len(download), err)
			}
			unchanged, err := os.ReadFile(outcomePath)
			if err != nil || !bytes.Equal(unchanged, originalEnvelope) {
				t.Fatalf("saved envelope bytes changed: %v", err)
			}
			inspection, inspectErr := exec.CommandContext(ctx, "docker", "inspect", "sandbox-"+sandbox).CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(inspectErr, &exit) || exit.ExitCode() != 1 || !strings.Contains(strings.ToLower(string(inspection)), "no such object") {
				t.Fatalf("owned sandbox retained: %s error=%v", inspection, inspectErr)
			}
			eventsAfterStop := readEventsUntil(t, ctx, api, final.ID, "", terminalEvent)
			if len(eventsAfterStop) == 0 || !terminalEvent(eventsAfterStop[len(eventsAfterStop)-1]) {
				t.Fatal("terminal cancellation missing from real SSE")
			}
			t.Logf("SIGKILL arbitration kept %s prefix, immutable artifact/envelope and inspectable evidence, one debit/receipt/terminal event, original deadlines and no downstream execution", wantNode)
		})
	}
}
