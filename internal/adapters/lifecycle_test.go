package adapters

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLeaseCanBeRenewedRepeatedly(t *testing.T) {
	dir := t.TempDir()
	for range 3 {
		expires, err := renewLease(dir)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "lease"))
		if err != nil {
			t.Fatal(err)
		}
		actual, err := time.Parse(time.RFC3339Nano, string(data))
		if err != nil || !actual.Equal(expires) {
			t.Fatalf("invalid renewed lease: %s, %v", data, err)
		}
	}
}

func TestLeaseWriterRetriesTransientErrorsOnlyWithinIssuedLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var calls atomic.Int32
	keepLease(ctx, time.Now().Add(200*time.Millisecond), 10*time.Millisecond, func() (time.Time, error) {
		if calls.Add(1) < 3 {
			return time.Time{}, errors.New("transient I/O failure")
		}
		cancel()
		return time.Now().Add(time.Second), nil
	})
	if calls.Load() != 3 {
		t.Fatalf("stopped after transient renewal failure: %d calls", calls.Load())
	}
	started := time.Now()
	keepLease(
		context.Background(),
		time.Now().Add(50*time.Millisecond),
		10*time.Millisecond,
		func() (time.Time, error) { return time.Time{}, errors.New("persistent I/O failure") },
	)
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("writer kept trying to resurrect an expired lease")
	}
}

func TestDockerLeaseTransientReadFailureDoesNotRevokeValidExpiry(t *testing.T) {
	runner := integrationRunner(t)
	req := testRequest("")
	req.Node.Sandbox = "box"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sandbox, err := runner.nodeSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.close()
	// Ensure PID 1 has observed its initial lease, then emulate a brief missing
	// inode during shared-filesystem rename visibility without renewing it.
	if _, err = sandbox.helper(ctx, "exec", map[string]any{"command": []string{"python", "-c", "import time;time.sleep(1)"}}); err != nil {
		t.Fatal(err)
	}
	sandbox.stopLease()
	<-sandbox.leaseDone
	if err = os.Remove(filepath.Join(sandbox.dir, "control", "lease")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if _, err = sandbox.helper(ctx, "write", map[string]any{"path": "alive", "content": "yes"}); err != nil {
		t.Fatalf("valid lease was revoked by transient read failure: %v", err)
	}
	if _, err = renewLease(filepath.Join(sandbox.dir, "control")); err != nil {
		t.Fatal(err)
	}
	if _, err = sandbox.helper(ctx, "read", map[string]any{"path": "alive"}); err != nil {
		t.Fatal(err)
	}
}

func TestDockerCleanupIsBoundToEngineIdentity(t *testing.T) {
	runner := integrationRunner(t)
	runner.EngineID = "cleanup-" + uuid.NewString()
	other := integrationRunner(t)
	other.EngineID = "other-" + uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req := testRequest("")
	req.Node.Sandbox = "box"
	owned, err := runner.nodeSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.close()
	unrelated, err := other.nodeSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer unrelated.close()
	if err = runner.CleanupOwned(ctx); err != nil {
		t.Fatal(err)
	}
	assertContainerGone(t, ctx, owned.docker, owned.id)
	if _, err = os.Stat(owned.dir); !os.IsNotExist(err) {
		t.Fatalf("orphan files remain: %v", err)
	}
	var inspect map[string]any
	if err = unrelated.docker.json(ctx, "GET", "/containers/"+unrelated.id+"/json", nil, &inspect); err != nil {
		t.Fatalf("cleanup affected other engine: %v", err)
	}
	if err = runner.CleanupOwned(ctx); err != nil {
		t.Fatalf("cleanup is not idempotent: %v", err)
	}
}

func TestDockerHardDeadlineCannotBeRenewed(t *testing.T) {
	runner := integrationRunner(t)
	req := testRequest("")
	req.Node.Type, req.Node.Sandbox = "code", "box"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sandbox, err := runner.nodeSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.close()
	waitContainerStopped(t, sandbox.docker, sandbox.id, 5*time.Second)
	if err := sandbox.diagnose(errors.New("original")); !strings.Contains(err.Error(), "hard deadline expired") {
		t.Fatal(err)
	}
}

// The owner really dies without running defers. Its container must stop without
// any replacement engine process or explicit Docker cleanup call.
func TestDockerWatchdogSurvivesOwnerSIGKILL(t *testing.T) {
	if os.Getenv("KNOTRA_SANDBOX_OWNER_CHILD") == "1" {
		runner := integrationRunner(t)
		runner.EngineID = "crash-" + uuid.NewString()
		req := testRequest("")
		req.Node.Type, req.Node.Sandbox = "code", "box"
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sandbox, err := runner.nodeSandbox(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		defer sandbox.close()
		go func() {
			_, _ = sandbox.helper(
				ctx,
				"exec",
				map[string]any{"command": []string{"python", "-c", "import time;time.sleep(120)"}, "timeout": "2m"},
			)
		}()
		b, _ := json.Marshal(map[string]string{"id": sandbox.id, "dir": sandbox.dir, "engine": runner.EngineID})
		fmt.Println("KNOTRA_SANDBOX_READY " + string(b))
		<-ctx.Done()
		return
	}
	runner := integrationRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDockerWatchdogSurvivesOwnerSIGKILL$")
	cmd.Env = append(os.Environ(), "KNOTRA_SANDBOX_OWNER_CHILD=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var ready struct{ ID, Dir, Engine string }
	scanner := bufio.NewScanner(stdout)

	for scanner.Scan() {
		if text, ok := strings.CutPrefix(scanner.Text(), "KNOTRA_SANDBOX_READY "); ok {
			if err = json.Unmarshal([]byte(text), &ready); err != nil {
				t.Fatal(err)
			}
			break
		}
	}

	if ready.ID == "" {
		t.Fatal("owner did not create sandbox", scanner.Err())
	}
	// Transfer cleanup responsibility before killing the owner; test failures must
	// never leave a live test workload or its host package directory behind.
	docker, err := runner.docker()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = docker.remove(context.Background(), ready.ID); _ = os.RemoveAll(ready.Dir) }()
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	t.Log("owner killed; waiting for independent 45-second container lease expiry")
	waitContainerStopped(t, docker, ready.ID, sandboxLeaseTTL+8*time.Second)
	if _, err = os.Stat(filepath.Join(ready.Dir, "control", "lease")); err != nil {
		t.Fatalf("watchdog test accidentally ran host cleanup: %v", err)
	}
	runner.EngineID = ready.Engine
	if err = runner.CleanupOwned(ctx); err != nil {
		t.Fatal(err)
	}
	assertContainerGone(t, ctx, docker, ready.ID)
}

func assertContainerGone(t *testing.T, ctx context.Context, docker *dockerClient, id string) {
	t.Helper()
	var inspect map[string]any
	err := docker.json(ctx, "GET", "/containers/"+id+"/json", nil, &inspect)
	var apiErr *dockerAPIError
	if !errors.As(err, &apiErr) || apiErr.status != http.StatusNotFound {
		t.Fatalf("container %s still exists or inspection failed: %v", id, err)
	}
}

func waitContainerStopped(t *testing.T, docker *dockerClient, id string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	for {
		var inspect struct {
			State struct {
				Running  bool
				ExitCode int
			}
		}
		err := docker.json(ctx, "GET", "/containers/"+id+"/json", nil, &inspect)
		if err == nil && !inspect.State.Running {
			return
		}
		if err != nil {
			t.Fatal(err)
		}

		select {
		case <-ctx.Done():
			t.Fatalf("container %s outlived its lease: %v", id, ctx.Err())
		case <-tick.C:
		}
	}
}
