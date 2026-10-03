package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDeadline(t *testing.T, dir, name string, deadline time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(deadline.Format(time.RFC3339Nano)), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorRequiresLeaseAndHonorsHardDeadline(t *testing.T) {
	dir, now := t.TempDir(), time.Now()
	if _, err := loadLease(dir, now); err == nil {
		t.Fatal("missing lease accepted")
	}
	writeDeadline(t, dir, "lease", now.Add(time.Minute))
	writeDeadline(t, dir, "deadline", now.Add(time.Second))
	state, err := loadLease(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	writeDeadline(t, dir, "lease", now.Add(2*time.Minute))
	if err = state.check(dir, now.Add(2*time.Second)); err == nil || !strings.Contains(err.Error(), "hard deadline") {
		t.Fatalf("renewal bypassed immutable deadline: %v", err)
	}
}

func TestSupervisorKeepsVerifiedExpiryAcrossTransientReadFailure(t *testing.T) {
	dir, now := t.TempDir(), time.Now()
	writeDeadline(t, dir, "lease", now.Add(time.Minute))
	state, err := loadLease(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "lease")); err != nil {
		t.Fatal(err)
	}
	if err = state.check(dir, now.Add(10*time.Second)); err != nil {
		t.Fatalf("transient ENOENT revoked valid lease: %v", err)
	}
	writeDeadline(t, dir, "lease", now.Add(-time.Minute))
	if err = state.check(dir, now.Add(20*time.Second)); err != nil {
		t.Fatalf("stale shared filesystem inode revoked valid lease: %v", err)
	}
	writeDeadline(t, dir, "lease", now.Add(2*time.Minute))
	if err = state.check(dir, now.Add(30*time.Second)); err != nil {
		t.Fatalf("fresh heartbeat could not extend lease: %v", err)
	}
	if err = os.WriteFile(filepath.Join(dir, "lease"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = state.check(dir, now.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = state.check(dir, now.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("missing terminal read diagnostic: %v", err)
	}
}
