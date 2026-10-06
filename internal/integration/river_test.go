package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
	storedb "github.com/michael-bill/knotra/internal/store/db"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestRiverServiceStopsAfterHeartbeatLoss(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL for River service lifecycle")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	profile := recoveryProfile()
	profile.Spec.Sandboxes = nil
	options, api, done := startRiverService(t, ctx, profile)
	admin, err := pgx.Connect(ctx, options.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(ctx, "UPDATE knotra_workers SET heartbeat_at=clock_timestamp()-interval '21 seconds'"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrExecutionOwnership) {
			t.Fatalf("lost worker heartbeat did not stop the service: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("service kept accepting requests after losing its worker heartbeat")
	}
	if err := api.Get(ctx, "/info", new(map[string]any)); err == nil {
		t.Fatal("HTTP listener survived execution ownership loss")
	}
}

func TestRiverHumanAnswerCommitsBeforeDelayedDelivery(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL for paused queue acceptance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	profile := recoveryProfile()
	profile.Spec.Sandboxes = nil
	options, api, _ := startRiverService(t, ctx, profile)
	admin, err := store.Open(ctx, options.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema, err := storedb.New(admin.Pool).CurrentSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control, err := river.NewClient(riverpgxv5.New(admin.Pool), &river.Config{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	const source = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: human-deadline}
spec:
  limits: {timeout: 1m}
  nodes:
    review:
      type: human
      execution: {timeout: 5s}
      human: {prompt: {text: Approve}}
      outputs:
        approved: {schema: {type: boolean}}
  outputs:
    approved: {schema: {type: boolean}, bind: {from: nodes.review.outputs.approved}}
`
	var definition struct{ Definition protocol.Definition }
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
	if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
		t.Fatal(err)
	}
	var accepted struct{ Run protocol.Run }
	if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &accepted); err != nil {
		t.Fatal(err)
	}
	request := awaitHuman(t, ctx, api, accepted.Run.ID)
	queueName := "knotra_advance_" + admin.EngineID
	if err := control.QueuePause(ctx, queueName, nil); err != nil {
		t.Fatal(err)
	}
	// Let the native notifier apply the committed pause before the answer arrives.
	select {
	case <-time.After(150 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	key := uuid.NewString()
	var receipt any
	for range 2 {
		if err := api.Command(ctx, "/requests/"+request.ID+"/response", map[string]any{"outputs": map[string]any{"approved": true}}, key, &receipt); err != nil {
			t.Fatal(err)
		}
	}
	var result struct{ Run protocol.Run }
	if err := api.Get(ctx, "/runs/"+accepted.Run.ID, &result); err != nil || result.Run.Status != "succeeded" {
		t.Fatalf("accepted answer was left waiting for queue delivery: %+v %v", result.Run, err)
	}
	select {
	case <-time.After(time.Until(request.Deadline) + 500*time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := control.QueueResume(ctx, queueName, nil); err != nil {
		t.Fatal(err)
	}
	if final := awaitTerminal(t, ctx, api, accepted.Run.ID); final.Status != "succeeded" || string(final.Outputs["approved"]) != "true" {
		t.Fatalf("late delivery replaced the accepted answer: %+v", final)
	}
}

func startRiverService(t *testing.T, ctx context.Context, profile contract.Profile) (app.Options, *client.Client, <-chan error) {
	t.Helper()
	directory := t.TempDir()
	profilePath := filepath.Join(directory, "profile.json")
	writeJSON(t, profilePath, profile)
	options := app.Options{Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx),
		TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(directory, "engine"), HelperPath: os.Getenv("KNOTRA_TEST_HELPER"), Profiles: []string{profilePath}}
	serviceCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- app.Serve(serviceCtx, options, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("River service did not stop")
		}
	})
	api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(directory, "client")}
	waitReady(t, ctx, api, done)
	return options, api, done
}
