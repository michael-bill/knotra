package execution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/michael-bill/knotra/internal/contract"
)

func TestMCPEvidenceFencesOwnershipAndBoundsRecovery(t *testing.T) {
	directory := t.TempDir()
	files, err := OpenOutcomeFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	resource := ResourceRecord{ID: uuid.NewString(), EngineID: "engine", HostID: "host", Kind: "mcp_http", Lifetime: "run", State: "active", DispatchGeneration: 1,
		Ownership: fixtureOutcome().Ownership, MCP: &MCPSessionRecord{Connection: "tools", SessionID: "original-session", ProtocolVersion: "2025-11-25"}}
	for range 2 {
		if err := files.PutMCPSession(resource); err != nil {
			t.Fatal(err)
		}
	}
	changed := resource
	changed.MCP = &MCPSessionRecord{Connection: "tools", SessionID: "replacement-session", ProtocolVersion: "2025-11-25"}
	if err := files.PutMCPSession(changed); !errors.Is(err, ErrOutcomeConflict) {
		t.Fatalf("replaced initialization evidence: %v", err)
	}
	if _, err := files.GetMCPSession(changed); err == nil {
		t.Fatal("accepted evidence that conflicts with the recorded session")
	}
	expected := resource
	expected.MCP, expected.State, expected.DispatchGeneration = nil, "cleaning", 2
	got, err := files.GetMCPSession(expected)
	if err != nil || got == nil || *got != *resource.MCP {
		t.Fatalf("lost immutable session ID after cleanup generation advanced: identity=%+v error=%v", got, err)
	}
	foreign := expected
	foreign.HostID = "another-host"
	if _, err := files.GetMCPSession(foreign); err == nil {
		t.Fatal("accepted another host's initialization evidence")
	}
	key, err := mcpEvidenceKey(resource)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, key+".json")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxMCPEvidenceBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := files.GetMCPSession(expected); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("accepted oversized initialization evidence: %v", err)
	}
}

func TestMCPCallRecoveryRetainsOnlyCurrentIdentity(t *testing.T) {
	directory := t.TempDir()
	files, err := OpenOutcomeFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	resource := ResourceRecord{ID: uuid.NewString(), EngineID: "engine", HostID: "host", Kind: "mcp_http", Lifetime: "run", State: "active", DispatchGeneration: 1,
		Ownership: fixtureOutcome().Ownership, MCP: &MCPSessionRecord{Connection: "tools", SessionID: "original-session", ProtocolVersion: "2025-11-25"}}
	if err := files.PutMCPSession(resource); err != nil {
		t.Fatal(err)
	}
	for _, id := range []json.RawMessage{[]byte(`null`), []byte(` null `), []byte(`true`), []byte(`1.5`), []byte(`{}`), []byte(`[]`), []byte(`"` + strings.Repeat("x", 1024) + `"`)} {
		if err := files.PutMCPCall(resource, id, false); err == nil {
			t.Fatalf("accepted invalid request ID: %s", id)
		}
	}
	for i := range 10 {
		id := json.RawMessage([]byte(fmt.Sprint(i)))
		if err := files.PutMCPCall(resource, id, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	files, err = OpenOutcomeFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	expected := resource
	expected.State, expected.DispatchGeneration = "cleaning", 2
	id, err := files.GetMCPCall(expected)
	if err != nil || string(id) != "9" {
		t.Fatalf("did not recover last dispatched request: %s, %v", id, err)
	}
	foreign := expected
	foreign.HostID = "another-host"
	if _, err := files.GetMCPCall(foreign); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("accepted another host's call: %v", err)
	}
	if err := files.PutMCPCall(resource, []byte(`"finished"`), true); err != nil {
		t.Fatal(err)
	}
	if id, err := files.GetMCPCall(expected); err != nil || len(id) != 0 {
		t.Fatalf("cancelled a confirmed response: %s, %v", id, err)
	}
	if err := files.PutMCPSession(resource); err != nil {
		t.Fatalf("current-call updates changed immutable initialization evidence: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 2 {
		t.Fatalf("call tracking grew with RPC count: files=%d error=%v", len(entries), err)
	}
}

func fixtureOutcome() Outcome {
	return Outcome{
		FormatVersion: StateFormatVersion,
		Ownership:     Ownership{AttemptID: AttemptID{RunID: "run", InstanceID: "root/a", Number: 1}, WorkerID: "worker", Generation: 1},
		PlanID:        "frozen-plan", CompletedAt: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
		Outputs: contract.Values{"value": {JSON: []byte(`{"text":"complete result"}`)}},
	}
}

func TestHostIdentityValidation(t *testing.T) {
	for _, id := range []string{"local", "worker-01.example", "Host_2", strings.Repeat("a", 128)} {
		if err := ValidateHostID(id); err != nil {
			t.Fatalf("rejected portable host identity %q: %v", id, err)
		}
	}
	for _, id := range []string{"", "../host", "/host", ".host", "-host", "host\nforeign", " host", "host ", "хост", strings.Repeat("a", 129)} {
		if err := ValidateHostID(id); err == nil {
			t.Fatalf("accepted invalid host identity %q", id)
		}
	}
}

func TestOutcomeSurvivesProcessTermination(t *testing.T) {
	if os.Getenv("KNOTRA_OUTCOME_CHILD") == "1" {
		files, err := OpenOutcomeFiles(os.Getenv("KNOTRA_OUTCOME_DIRECTORY"))
		if err != nil {
			os.Exit(2)
		}
		if _, err := files.Put(fixtureOutcome()); err != nil {
			os.Exit(3)
		}
		os.Exit(0) // deliberately skip Close and all test cleanup
	}
	directory := t.TempDir()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOutcomeSurvivesProcessTermination$")
	cmd.Env = append(os.Environ(), "KNOTRA_OUTCOME_CHILD=1", "KNOTRA_OUTCOME_DIRECTORY="+directory)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writer process: %v: %s", err, output)
	}
	files, err := OpenOutcomeFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	expected := fixtureOutcome()
	key, _ := expected.Ownership.OutcomeKey()
	got, err := files.Get(key)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("recover full evidence: %+v, %v", got, err)
	}
}

func TestConcurrentOutcomePublicationCannotReplaceEvidence(t *testing.T) {
	files, err := OpenOutcomeFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	a := fixtureOutcome()
	b := a
	b.Outputs = contract.Values{"value": {JSON: []byte(`null`)}}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcome := a
			if i%2 == 1 {
				outcome = b
			}
			_, err := files.Put(outcome)
			errorsSeen <- err
		}()
	}
	wg.Wait()
	close(errorsSeen)
	successes, conflicts := 0, 0
	for err := range errorsSeen {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrOutcomeConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != 16 || conflicts != 16 {
		t.Fatalf("one immutable winner: successes=%d conflicts=%d", successes, conflicts)
	}
	key, _ := a.Ownership.OutcomeKey()
	got, err := files.Get(key)
	if err != nil || !reflect.DeepEqual(got, a) && !reflect.DeepEqual(got, b) {
		t.Fatalf("conflicting evidence corrupted winner: %+v, %v", got, err)
	}
}

func TestOutcomeRejectsCorruptionAndWrongIdentity(t *testing.T) {
	dir := t.TempDir()
	files, err := OpenOutcomeFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	outcome := fixtureOutcome()
	key, err := files.Put(outcome)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	otherKey := strings.Repeat("b", 64)
	if err := os.WriteFile(filepath.Join(dir, otherKey+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Get(otherKey); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatal("accepted evidence under another ownership key")
	}
	corrupt := bytes.Replace(data, []byte("complete result"), []byte("tampered result"), 1)
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Get(key); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatal("accepted corrupted output")
	}
	if _, err := files.Put(outcome); err == nil {
		t.Fatal("silently repaired corrupted evidence")
	}
	if _, err := files.Get("../outside"); err == nil {
		t.Fatal("accepted traversal key")
	}
}

func TestOutcomePreservesLargeValuesAndOptionalAbsence(t *testing.T) {
	files, err := OpenOutcomeFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	outcome := fixtureOutcome()
	outcome.Outputs = contract.Values{
		"large": {JSON: []byte(`"` + strings.Repeat("x", 2<<20) + `"`)},
		"null":  {JSON: []byte("null")},
	}
	key, err := files.Put(outcome)
	if err != nil {
		t.Fatal(err)
	}
	got, err := files.Get(key)
	if err != nil || !reflect.DeepEqual(got, outcome) {
		t.Fatalf("complete values changed during handoff: %v", err)
	}
}

func TestOutcomeIdentityAndFormatValidation(t *testing.T) {
	outcome := fixtureOutcome()
	for _, mutate := range []func(*Outcome){
		func(o *Outcome) { o.FormatVersion++ },
		func(o *Outcome) { o.PlanID = "" },
		func(o *Outcome) { o.CompletedAt = time.Time{} },
		func(o *Outcome) { o.Ownership.Number = 0 },
		func(o *Outcome) { o.Ownership.Generation = 0 },
		func(o *Outcome) { o.Failure = &Failure{Code: "unsafe"} },
		func(o *Outcome) { o.Artifacts = []ArtifactReference{{ID: "a", SHA256: "invalid"}} },
	} {
		invalid := outcome
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatal("accepted invalid envelope")
		}
	}
	first, _ := outcome.Ownership.OutcomeKey()
	outcome.Ownership.Generation++
	second, _ := outcome.Ownership.OutcomeKey()
	if first == second {
		t.Fatal("ownership generation did not change outcome identity")
	}
}
