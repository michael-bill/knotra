package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestCommandLockSurvivesContentionAndOwnerDeath(t *testing.T) {
	if dir := os.Getenv("KNOTRA_TEST_CLIENT_LOCK_DIR"); dir != "" {
		client := Client{StateDir: dir}
		unlock, err := client.lockCommand(context.Background(), "shared")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		_, _ = fmt.Fprintln(os.Stdout, "locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	client := Client{StateDir: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandLockSurvivesContentionAndOwnerDeath$")
	cmd.Env = append(os.Environ(), "KNOTRA_TEST_CLIENT_LOCK_DIR="+client.StateDir)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("child failed to acquire lock: %q, %v", line, err)
	}
	waitCtx, stopWait := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stopWait()
	if unlock, err := client.lockCommand(waitCtx, "shared"); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("contended lock must respect cancellation: %v", err)
	}
	// A lock on one command must not block other command identities.
	unlock, err := client.lockCommand(ctx, "independent")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// The owner died without defers: the OS must release its lock automatically.
	unlock, err = client.lockCommand(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
