//go:build unix

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	nativequeue "github.com/michael-bill/knotra/internal/queue"
	"github.com/michael-bill/knotra/internal/store"
)

func TestRiverBackupRestoresHumanAndUncommittedOutcome(t *testing.T) {
	container := os.Getenv("KNOTRA_TEST_POSTGRES_CONTAINER")
	if container == "" || os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_HELPER") == "" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_POSTGRES_CONTAINER, KNOTRA_TEST_RECOVERY, KNOTRA_TEST_HELPER and KNOTRA_TEST_DATABASE_URL for backup/restore acceptance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	profile := recoveryProfile()
	profilePath := filepath.Join(work, "profile.json")
	writeJSON(t, profilePath, profile)
	options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx),
		TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), SharedDataDir: filepath.Join(work, "shared"),
		DockerHost: os.Getenv("DOCKER_HOST"), HelperPath: os.Getenv("KNOTRA_TEST_HELPER"), Profiles: []string{profilePath}, Version: "backup-test"}
	optionsPath := filepath.Join(work, "server.json")
	writeJSON(t, optionsPath, options)
	api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
	firstLog := filepath.Join(work, "first.log")
	first := startEngineProcess(t, ctx, optionsPath, firstLog, api)
	identity := engineIdentity(t, ctx, api)
	waiting := startCLIRun(t, ctx, api, filepath.Join(root, "examples/integration/recovery.yaml"), "waiting-"+identity)
	human := awaitHuman(t, ctx, api, waiting.ID)
	prefix := readEventsUntil(t, ctx, api, waiting.ID, "", func(event protocol.Event) bool {
		return event.InstanceID == human.InstanceID && event.Message == "waiting_human"
	})
	admin, err := store.Open(ctx, options.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	config := admin.Pool.Config().ConnConfig.Copy()
	schema := config.RuntimeParams["search_path"]
	barrier, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = barrier.Close(context.Background()) }()
	if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273663)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION backup_publication_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273663); RETURN NEW; END $$;
		CREATE TRIGGER backup_publication_barrier BEFORE UPDATE ON knotra_artifacts FOR EACH ROW WHEN (NOT OLD.published AND NEW.published) EXECUTE FUNCTION backup_publication_barrier()`); err != nil {
		t.Fatal(err)
	}
	marker := strings.Repeat("backup-complete-input-", 10000)
	pending := startCLIRun(t, ctx, api, filepath.Join(root, "examples/integration/recovery.yaml"), marker)
	awaitLifecycleBoundary(t, ctx, first, firstLog, func() bool {
		var blocked bool
		if err := admin.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)))", int64(barrier.PgConn().PID())).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		return blocked
	})
	var key, instance string
	var deadline time.Time
	if err := admin.Pool.QueryRow(ctx, `SELECT a.outcome_key,a.instance_id,r.execution_deadline FROM knotra_execution_attempts a
		JOIN knotra_runs r ON r.id=a.run_id WHERE a.run_id=$1`, pending.ID).Scan(&key, &instance, &deadline); err != nil {
		t.Fatal(err)
	}
	files, err := execution.OpenOutcomeFiles(filepath.Join(options.SharedDataDir, "outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := files.Get(key)
	_ = files.Close()
	if err != nil || outcome.Failure != nil || len(outcome.Artifacts) != 1 {
		t.Fatalf("uncommitted physical outcome=%+v error=%v", outcome, err)
	}
	artifact := outcome.Artifacts[0]
	content, err := os.ReadFile(filepath.Join(options.SharedDataDir, "artifacts", artifact.SHA256))
	if err != nil {
		t.Fatal(err)
	}
	var document struct{ Marker, Nonce string }
	if err := json.Unmarshal(content, &document); err != nil || document.Marker != marker || document.Nonce == "" {
		t.Fatalf("real code evidence marker bytes=%d nonce=%q error=%v", len(document.Marker), document.Nonce, err)
	}
	envelopePath := filepath.Join(options.SharedDataDir, "outcomes", key+".json")
	envelope, err := os.ReadFile(envelopePath)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := river.NewClient(riverpgxv5.New(admin.Pool), &river.Config{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	queues := []string{"knotra_advance_" + identity, "knotra_maintenance_" + identity}
	for _, name := range queues {
		if err := queue.QueuePause(ctx, name, nil); err != nil {
			t.Fatal(err)
		}
	}
	first.kill(t)
	assertLifecycleSIGKILL(t, first)
	if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273663)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER backup_publication_barrier ON knotra_artifacts; DROP FUNCTION backup_publication_barrier()"); err != nil {
		t.Fatal(err)
	}
	var published, stored int
	if err := admin.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM knotra_artifacts WHERE id=$1 AND published),
		(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$2 AND outcome IS NOT NULL)`, artifact.ID, pending.ID).Scan(&published, &stored); err != nil || published+stored != 0 {
		t.Fatalf("backup boundary published=%d imported=%d error=%v", published, stored, err)
	}
	snapshot := func(s *store.Store) map[string][]byte {
		t.Helper()
		result := map[string][]byte{}
		rows, err := s.Pool.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname=$1 AND tablename LIKE 'knotra_%' ORDER BY tablename", schema)
		if err != nil {
			t.Fatal(err)
		}
		tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range tables {
			var rows []byte
			query := "SELECT COALESCE(jsonb_agg(row ORDER BY row::text),'[]') FROM (SELECT to_jsonb(t) row FROM " + pgx.Identifier{table}.Sanitize() + " t) records"
			if err := s.Pool.QueryRow(ctx, query).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			result[table] = rows
		}
		return result
	}
	before := snapshot(admin)
	save := func(name string, command *exec.Cmd) {
		t.Helper()
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = file.Close() }()
		var stderr bytes.Buffer
		command.Stdout, command.Stderr = file, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("backup command failed: %v (%s)", err, &stderr)
		}
		if err := file.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	dump := filepath.Join(work, "database.dump")
	archive := filepath.Join(work, "state.tar")
	save(dump, exec.CommandContext(ctx, "docker", "exec", container, "pg_dump", "-U", config.User, "-d", config.Database, "--schema", schema, "--format=custom", "--no-owner", "--no-acl"))
	save(archive, exec.CommandContext(ctx, "tar", "-cf", "-", "-C", work, "engine", "shared"))
	admin.Close()
	// These exact schema/directories were created by this fixture. No baseline
	// database, unrelated schema or other engine's storage is destroyed.
	if _, err := barrier.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{options.DataDir, options.SharedDataDir} {
		if err := os.RemoveAll(directory); err != nil {
			t.Fatal(err)
		}
	}
	input, err := os.Open(dump)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	restore := exec.CommandContext(ctx, "docker", "exec", "-i", container, "pg_restore", "-U", config.User, "-d", config.Database,
		"--no-owner", "--no-acl", "--exit-on-error", "--single-transaction")
	restore.Stdin = input
	if output, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("pg_restore failed: %v (%s)", err, output)
	}
	if output, err := exec.CommandContext(ctx, "tar", "-xpf", archive, "-C", work).CombinedOutput(); err != nil {
		t.Fatalf("state restore failed: %v (%s)", err, output)
	}
	restored, err := store.Open(ctx, options.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.EngineID != identity || !reflect.DeepEqual(before, snapshot(restored)) {
		t.Fatal("restored domain records, receipts or engine identity changed")
	}
	unchanged, err := os.ReadFile(envelopePath)
	if err != nil || !bytes.Equal(unchanged, envelope) {
		t.Fatalf("uncommitted envelope was not backed up byte for byte: %v", err)
	}
	second := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "restored.log"), api)
	recoveredHuman := awaitHuman(t, ctx, api, waiting.ID)
	if recoveredHuman.ID != human.ID || !recoveredHuman.Deadline.Equal(human.Deadline) {
		t.Fatal("restored human request identity/deadline changed")
	}
	replayed := readEventsUntil(t, ctx, api, waiting.ID, "", func(event protocol.Event) bool { return event.ID == prefix[len(prefix)-1].ID })
	if !reflect.DeepEqual(prefix, replayed) {
		t.Fatal("restored SSE prefix changed")
	}
	if _, err := api.Bytes(ctx, "/artifacts/"+artifact.ID+"/content"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("private uncommitted artifact became visible: %v", err)
	}
	queue, err = river.NewClient(riverpgxv5.New(restored.Pool), &river.Config{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range queues {
		if err := queue.QueueResume(ctx, name, nil); err != nil {
			t.Fatal(err)
		}
	}
	recoveredPending := awaitHuman(t, ctx, api, pending.ID)
	downloading, err := api.Bytes(ctx, "/artifacts/"+artifact.ID+"/content")
	if err != nil || !bytes.Equal(content, downloading) {
		t.Fatalf("restored publication changed original artifact bytes: %v", err)
	}
	for _, request := range []protocol.HumanRequest{recoveredHuman, recoveredPending} {
		if output, err := executeCLI(ctx, api, "requests", "respond", request.ID, "--output", "approved=true"); err != nil {
			t.Fatalf("restored CLI answer failed: %v (%s)", err, output)
		}
		final := awaitTerminal(t, ctx, api, request.RunID)
		if final.Status != "succeeded" || string(final.Outputs["verified"]) != "true" {
			t.Fatalf("restored run=%s diagnostics=%v", final.Status, final.Diagnostics)
		}
		assertSinglePreparation(t, final, readEventsUntil(t, ctx, api, final.ID, "", terminalEvent))
	}
	second.kill(t)
	thirdLog := filepath.Join(work, "repeated-recovery.log")
	third := startEngineProcess(t, ctx, optionsPath, thirdLog, api)
	// Expiring queue retention cannot turn a repeated finalizer into another
	// attempt, debit, artifact publication or terminal event.
	if _, err := restored.Pool.Exec(ctx, "DELETE FROM river_job WHERE kind='knotra_finalize_v1' AND args->>'outcomeKey'=$1 AND state='completed'", key); err != nil {
		t.Fatal(err)
	}
	delivery, err := queue.Insert(ctx, nativequeue.FinalizeArgs{Attempt: outcome.Ownership.AttemptID, OutcomeKey: key, RoutingVersion: nativequeue.RoutingVersion},
		&river.InsertOpts{Queue: "knotra_maintenance_" + identity})
	if err != nil {
		t.Fatal(err)
	}
	awaitLifecycleBoundary(t, ctx, third, thirdLog, func() bool {
		var completed bool
		if err := restored.Pool.QueryRow(ctx, "SELECT state='completed' FROM river_job WHERE id=$1", delivery.Job.ID).Scan(&completed); err != nil {
			t.Fatal(err)
		}
		return completed
	})
	var attempts, slots, resources, debits, terminals int
	var restoredDeadline time.Time
	if err := restored.Pool.QueryRow(ctx, `SELECT execution_deadline,
		(SELECT count(*) FROM knotra_execution_attempts),
		(SELECT count(*) FROM knotra_execution_slots WHERE released_at IS NULL),
		(SELECT count(*) FROM knotra_resources WHERE state<>'closed'),
		(SELECT sum(used) FROM knotra_budgets WHERE kind='tool'),
		(SELECT count(*) FROM knotra_events WHERE document->>'type'='run' AND document->'data'->>'status'='succeeded')
		FROM knotra_runs WHERE id=$1`, pending.ID).Scan(&restoredDeadline, &attempts, &slots, &resources, &debits, &terminals); err != nil || attempts != 4 || slots != 0 || resources != 0 || debits != 4 || terminals != 2 || !deadline.Equal(restoredDeadline) {
		t.Fatalf("idempotent recovery attempts/slots/resources/debits/terminals=%d/%d/%d/%d/%d deadline=%v error=%v", attempts, slots, resources, debits, terminals, restoredDeadline, err)
	}
	var nonce string
	if err := restored.Pool.QueryRow(ctx, "SELECT outputs->'nonce'->>'json' FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2", pending.ID, instance).Scan(&nonce); err != nil || nonce != document.Nonce {
		t.Fatalf("saved physical work repeated: nonce=%q expected=%q error=%v", nonce, document.Nonce, err)
	}
	t.Logf("real pg_dump/tar disaster restore retained %d complete domain tables, receipts, human deadline/SSE and uncommitted evidence; publication and repeated finalizer after queue retention did not repeat physical preparation", len(before))
}
