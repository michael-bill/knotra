//go:build unix

package queue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func TestRuntimeReconcilesKilledSandboxOwnerAndProtectsLivePeer(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RUNTIME_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_RUNTIME_RECOVERY and KNOTRA_TEST_HELPER for real sandbox owner SIGKILL")
	}
	r := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	directory := t.TempDir()
	r.Host.Runner.WorkDir = filepath.Join(directory, "work")
	plan := resourceCodePlan(t, r, 45*time.Second)
	id := admitRuntimeRun(t, r, plan)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeProcessCrashBoundaries$", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), "KNOTRA_RUNTIME_CHILD=1", "KNOTRA_RUNTIME_MODE=resource_owner", "KNOTRA_RUNTIME_DSN="+r.Store.Pool.Config().ConnString(), "KNOTRA_RUNTIME_DIRECTORY="+directory, "KNOTRA_RUNTIME_RUN="+id, "KNOTRA_RUNTIME_TIMEOUT=2m", "KNOTRA_RUNTIME_HOST="+r.HostID, "KNOTRA_RUNTIME_HELPER="+r.Host.Runner.HelperPath)
	log, err := os.Create(filepath.Join(directory, "owner.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	resourceForRun := func(runID string) string {
		t.Helper()
		var resource string
		await(t, 15*time.Second, func() (bool, error) {
			var count int
			err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_resources WHERE run_id=$1", runID).Scan(&count)
			return count == 1, err
		})
		if err := r.Store.Pool.QueryRow(ctx, "SELECT id FROM knotra_resources WHERE run_id=$1", runID).Scan(&resource); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_, _ = exec.CommandContext(cleanupCtx, "docker", "rm", "--force", "sandbox-"+resource).CombinedOutput()
		})
		await(t, 10*time.Second, func() (bool, error) {
			output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .State.Running}}", "sandbox-"+resource).Output()
			var missing *exec.ExitError
			if errors.As(err, &missing) {
				return false, nil
			}
			return string(output) == "true\n", err
		})
		return resource
	}
	owned := resourceForRun(id)
	labelBytes, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .Config.Labels}}", "sandbox-"+owned).Output()
	var ownedLabels map[string]string
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(labelBytes, &ownedLabels); err != nil {
		t.Fatal(err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		var debits int
		err := r.Store.Pool.QueryRow(ctx, "SELECT COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0)", id).Scan(&debits)
		return debits == 1, err
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("owner process did not receive SIGKILL")
	}
	// An unrelated incompatible expired claim makes outcome reconciliation fail.
	// It must not suppress sandbox reconciliation or the live peer's progress.
	badPlan := deliveryPlan("")
	badID := admitRuntimeRun(t, r, badPlan)
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if err := r.advanceTx(ctx, tx, badPlan, badID, ""); err != nil {
			return err
		}
		claim, err := r.Store.ClaimAttempt(ctx, tx, execution.AttemptID{RunID: badID, InstanceID: execution.StableID("n", badID+"/root/a"), Number: 1}, 1, ownedLabels["io.knotra.worker"])
		if err == nil && claim == nil {
			return errors.New("incompatible run fixture did not claim its attempt")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1", badID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_runs SET scheduler_version=$2 WHERE id=$1", badID, execution.SchedulerVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOutcomes(ctx); !errors.Is(err, store.ErrExecutionVersion) {
		t.Fatalf("fixture did not fail outcome reconciliation: %v", err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	peerID := admitRuntimeRun(t, r, plan)
	peer := resourceForRun(peerID)
	await(t, 35*time.Second, func() (bool, error) {
		var closed bool
		err := r.Store.Pool.QueryRow(ctx, "SELECT state='closed' FROM knotra_resources WHERE id=$1", owned).Scan(&closed)
		return closed, err
	})
	if output, err := exec.CommandContext(ctx, "docker", "inspect", "sandbox-"+owned).CombinedOutput(); err == nil {
		t.Fatalf("replacement retained killed owner's container: %s", output)
	}
	output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .State.Running}}", "sandbox-"+peer).Output()
	if err != nil || string(output) != "true\n" {
		t.Fatalf("replacement deleted live peer: %s %v", output, err)
	}
	var completed, attempts, debits int
	if err := r.Store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM river_job WHERE kind='knotra_cleanup_v1' AND state='completed'),
		(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool')`, id).Scan(&completed, &attempts, &debits); err != nil || completed != 1 || attempts != 1 || debits != 1 {
		t.Fatalf("cleanup=%d attempts=%d calls=%d error=%v", completed, attempts, debits, err)
	}
	// Model the daemon completing an unanswered create after the ledger closed.
	// This is another physical container, never another business execution.
	create := []string{"create", "--name", "sandbox-" + owned}
	for key, value := range ownedLabels {
		create = append(create, "--label", key+"="+value)
	}
	create = append(create, "python:3.13-alpine", "/bin/true")
	if output, err := exec.CommandContext(ctx, "docker", create...).CombinedOutput(); err != nil {
		t.Fatalf("late container fixture: %s %v", output, err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		var count int
		err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind='knotra_cleanup_v1' AND state='completed'").Scan(&count)
		return count == 2, err
	})
	if output, err := exec.CommandContext(ctx, "docker", "inspect", "sandbox-"+owned).CombinedOutput(); err == nil {
		t.Fatalf("inventory retained late container after cleanup: %s", output)
	}
	output, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .State.Running}}", "sandbox-"+peer).Output()
	if err != nil || string(output) != "true\n" {
		t.Fatalf("late container cleanup deleted live peer: %s %v", output, err)
	}
	await(t, 55*time.Second, func() (bool, error) {
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, peerID)
		return protocol.Terminal(status), err
	})
	peerRun, err := r.Store.Run(ctx, peerID)
	if err != nil || peerRun.Status != "succeeded" || string(peerRun.Outputs["result"]) != `"confirmed"` {
		data, _ := json.Marshal(peerRun)
		t.Fatalf("live peer did not finish: %s %v", data, err)
	}
}
