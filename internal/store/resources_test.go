package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/execution"
)

func TestOwnedMCPIdentityIsImmutableAndSurvivesAttemptEnd(t *testing.T) {
	s, ids, workers, _ := attemptFixture(t)
	ctx := t.Context()
	claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[0])
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v error=%v", claim, err)
	}
	resource, err := s.RegisterOwnedResource(ctx, claim.Ownership, uuid.NewString(), "mcp_http", "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_execution_attempts SET state='completed' WHERE run_id=$1 AND instance_id=$2", claim.RunID, claim.InstanceID); err != nil {
		t.Fatal(err)
	}
	identity := execution.MCPSessionRecord{Connection: "tools", SessionID: "original-session", ProtocolVersion: "2025-11-25"}
	for range 2 {
		if err := s.RecordOwnedMCPSession(ctx, claim.Ownership, resource.ID, identity); err != nil {
			t.Fatal(err)
		}
	}
	changed := identity
	changed.SessionID = "replacement-session"
	if err := s.RecordOwnedMCPSession(ctx, claim.Ownership, resource.ID, changed); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("replaced immutable remote identity: %v", err)
	}
	foreign := claim.Ownership
	foreign.Generation++
	if err := s.RecordOwnedMCPSession(ctx, foreign, resource.ID, identity); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("foreign ownership supplied cleanup identity: %v", err)
	}
	for _, invalid := range []execution.MCPSessionRecord{
		{Connection: "tools", SessionID: "injected\r\nHeader: value", ProtocolVersion: "2025-11-25"},
		{Connection: "tools", SessionID: strings.Repeat("s", 8193), ProtocolVersion: "2025-11-25"},
		{Connection: "tools", SessionID: "session", ProtocolVersion: "invalid"},
	} {
		if err := s.RecordOwnedMCPSession(ctx, claim.Ownership, resource.ID, invalid); err == nil {
			t.Fatal("accepted invalid cleanup header")
		}
	}
	if got, err := s.ClaimResourceCleanup(ctx, resource.ID, resource.HostID, claim.WorkerID, claim.Generation); err != nil || got != nil {
		t.Fatalf("completed attempt exposed a live run session to cleanup: resource=%+v error=%v", got, err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_workers SET heartbeat_at=clock_timestamp()-interval '21 seconds' WHERE id=$1", claim.WorkerID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimResourceCleanup(ctx, resource.ID, resource.HostID, claim.WorkerID, claim.Generation)
	if err != nil || got == nil || got.MCP == nil || *got.MCP != identity || got.Ownership != claim.Ownership {
		t.Fatalf("lost durable MCP cleanup identity: resource=%+v error=%v", got, err)
	}
}

func TestResourceCleanupProtectsLiveOwnersAndSessionLifetime(t *testing.T) {
	for _, mode := range []string{"live", "attempt_ended", "lease_expired", "worker_dead", "run_terminal"} {
		t.Run(mode, func(t *testing.T) {
			s, ids, workers, _ := attemptFixture(t)
			ctx := context.Background()
			claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[0])
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v error=%v", claim, err)
			}
			records := map[string]execution.ResourceRecord{}
			for _, lifetime := range []string{"attempt", "run"} {
				record, err := s.RegisterOwnedResource(ctx, claim.Ownership, uuid.NewString(), "sandbox", lifetime)
				if err != nil {
					t.Fatal(err)
				}
				if record.HostID != "test-host" || record.Ownership != claim.Ownership || record.EngineID != s.EngineID {
					t.Fatalf("resource identity=%+v", record)
				}
				records[lifetime] = record
			}
			if _, err := s.RegisterOwnedResource(ctx, claim.Ownership, "../outside", "sandbox", "attempt"); err == nil {
				t.Fatal("accepted path as resource identity")
			}
			wrong := claim.Ownership
			wrong.Generation++
			if err := s.CloseOwnedResource(ctx, wrong, records["attempt"].ID); !errors.Is(err, ErrExecutionOwnership) {
				t.Fatalf("foreign owner closed resource: %v", err)
			}
			switch mode {
			case "attempt_ended":
				_, err = s.Pool.Exec(ctx, "UPDATE knotra_execution_attempts SET state='completed' WHERE run_id=$1 AND instance_id=$2", claim.RunID, claim.InstanceID)
			case "lease_expired":
				_, err = s.Pool.Exec(ctx, "UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1 AND instance_id=$2", claim.RunID, claim.InstanceID)
			case "worker_dead":
				_, err = s.Pool.Exec(ctx, "UPDATE knotra_workers SET heartbeat_at=clock_timestamp()-interval '21 seconds' WHERE id=$1", claim.WorkerID)
			case "run_terminal":
				_, err = s.Pool.Exec(ctx, "UPDATE knotra_runs SET document=jsonb_set(document::jsonb,'{status}','\"succeeded\"')::json WHERE id=$1", claim.RunID)
			}
			if err != nil {
				t.Fatal(err)
			}
			for lifetime, record := range records {
				for _, identity := range []struct {
					host, worker string
					generation   int64
				}{{"other-host", claim.WorkerID, claim.Generation}, {record.HostID, workers[1], claim.Generation}, {record.HostID, claim.WorkerID, claim.Generation + 1}} {
					if got, err := s.ClaimResourceCleanup(ctx, record.ID, identity.host, identity.worker, identity.generation); err != nil || got != nil {
						t.Fatalf("foreign cleanup=%+v error=%v", got, err)
					}
				}
				got, err := s.ClaimResourceCleanup(ctx, record.ID, record.HostID, claim.WorkerID, claim.Generation)
				want := mode == "worker_dead" || mode == "run_terminal" || (lifetime == "attempt" && (mode == "attempt_ended" || mode == "lease_expired"))
				if err != nil || (got != nil) != want {
					t.Fatalf("lifetime=%s cleanup=%+v want=%v error=%v", lifetime, got, want, err)
				}
				if want {
					if again, err := s.ClaimResourceCleanup(ctx, record.ID, record.HostID, claim.WorkerID, claim.Generation); err != nil || again == nil {
						t.Fatalf("cleanup retry=%+v error=%v", again, err)
					}
					for range 2 {
						if err := s.CloseOwnedResource(ctx, claim.Ownership, record.ID); err != nil {
							t.Fatal(err)
						}
					}
					if again, err := s.ClaimResourceCleanup(ctx, record.ID, record.HostID, claim.WorkerID, claim.Generation); err != nil || again != nil {
						t.Fatalf("closed resource reclaimed=%+v error=%v", again, err)
					}
				}
			}
			if mode == "lease_expired" || mode == "worker_dead" || mode == "run_terminal" || mode == "attempt_ended" {
				if _, err := s.RegisterOwnedResource(ctx, claim.Ownership, uuid.NewString(), "sandbox", "attempt"); !errors.Is(err, ErrExecutionOwnership) {
					t.Fatalf("invalid owner created new resource: %v", err)
				}
			}
			var debits int
			if err := s.Pool.QueryRow(ctx, "SELECT COALESCE(sum(used),0) FROM knotra_budgets WHERE run_id=$1", claim.RunID).Scan(&debits); err != nil || debits != 0 {
				t.Fatalf("resource preparation spent call budget=%d error=%v", debits, err)
			}
		})
	}
}

func TestObservedResourceReopensOnlyItsMatchingClosedOwner(t *testing.T) {
	s, ids, workers, _ := attemptFixture(t)
	ctx := t.Context()
	claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[0])
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v error=%v", claim, err)
	}
	record, err := s.RegisterOwnedResource(ctx, claim.Ownership, uuid.NewString(), "sandbox", "run")
	if err != nil {
		t.Fatal(err)
	}
	observe := func(record execution.ResourceRecord) *execution.ResourceRecord {
		t.Helper()
		var got *execution.ResourceRecord
		if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			var err error
			got, err = s.ClaimObservedResourceCleanupTx(ctx, tx, record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := observe(record); got != nil {
		t.Fatal("inventory claimed a live run session")
	}
	if err := s.CloseOwnedResource(ctx, claim.Ownership, record.ID); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"engine", "host", "run", "instance", "attempt", "worker", "generation"} {
		foreign := record
		switch field {
		case "engine":
			foreign.EngineID = uuid.NewString()
		case "host":
			foreign.HostID = "foreign"
		case "run":
			foreign.Ownership.RunID = uuid.NewString()
		case "instance":
			foreign.Ownership.InstanceID = "foreign"
		case "attempt":
			foreign.Ownership.Number++
		case "worker":
			foreign.Ownership.WorkerID = workers[1]
		case "generation":
			foreign.Ownership.Generation++
		}
		if got := observe(foreign); got != nil {
			t.Fatalf("foreign %s reopened resource: %+v", field, got)
		}
	}
	if got := observe(record); got == nil || got.State != "cleaning" || got.Ownership != record.Ownership {
		t.Fatalf("matching late container was not reclaimed: %+v", got)
	}
	if got, err := s.ClaimResourceCleanup(ctx, record.ID, record.HostID, record.Ownership.WorkerID, record.Ownership.Generation); err != nil || got == nil {
		t.Fatalf("live original owner suppressed tombstone cleanup: %+v %v", got, err)
	}
}
