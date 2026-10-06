package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/store/db"
	"github.com/michael-bill/knotra/internal/telemetry"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type Handlers struct {
	Advance  func(context.Context, *river.Job[AdvanceArgs]) error
	Execute  func(context.Context, *river.Job[ExecuteArgs]) error
	Finalize func(context.Context, *river.Job[FinalizeArgs]) error
	Cleanup  func(context.Context, *river.Job[CleanupArgs]) error
}

type Config struct {
	Role             execution.Role
	EngineID         string
	HostID           string
	Schema           string
	Logger           *slog.Logger
	ExecutionWorkers int
	// ExecutionTimeout must exceed the greatest admitted attempt duration.
	// An unlimited attempt needs a separate policy before enabling admission.
	ExecutionTimeout time.Duration
}

type Client struct {
	role             execution.Role
	River            *river.Client[pgx.Tx]
	advance          string
	execute          string
	maintenance      string
	cleanup          string
	executionTimeout time.Duration
	engineID         string
	hostID           string
	logger           *slog.Logger
}

// New requires each handler consumed by the selected role. Absent implementation
// must never silently acknowledge work from that role's queues.
func New(pool *pgxpool.Pool, config Config, handlers Handlers) (*Client, error) {
	if config.Role == "" {
		config.Role = execution.RoleAll
	}
	if err := config.Role.Validate(); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(config.EngineID); err != nil {
		return nil, errors.New("queue requires a UUID engine identity")
	}
	if err := execution.ValidateHostID(config.HostID); err != nil {
		return nil, err
	}
	if config.Role != execution.RoleAPI && config.ExecutionTimeout <= 0 {
		return nil, errors.New("queue requires a positive execution timeout")
	}
	if (config.Role == execution.RoleAll || config.Role == execution.RoleExecutor) && config.ExecutionWorkers < 1 {
		return nil, errors.New("queue requires positive execution concurrency and timeout")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	config.Logger = config.Logger.With("engineId", config.EngineID, "hostId", config.HostID, "role", config.Role)
	hostHash := sha256.Sum256([]byte(config.EngineID + "\x00" + config.HostID))
	c := &Client{
		role:             config.Role,
		advance:          "knotra_advance_" + config.EngineID,
		execute:          "knotra_execute_" + config.EngineID,
		maintenance:      "knotra_maintenance_" + config.EngineID,
		cleanup:          "knotra_cleanup_" + hex.EncodeToString(hostHash[:16]),
		executionTimeout: config.ExecutionTimeout,
		engineID:         config.EngineID,
		hostID:           config.HostID,
		logger:           config.Logger,
	}
	var workers *river.Workers
	var queues map[string]river.QueueConfig
	if c.role != execution.RoleAPI {
		workers = river.NewWorkers()
		queues = make(map[string]river.QueueConfig)
	}
	if c.schedules() {
		if handlers.Advance == nil || handlers.Finalize == nil {
			return nil, errors.New("scheduler requires advance and finalize handlers")
		}
		river.AddWorker(workers, &deliveryWorker[AdvanceArgs]{work: handlers.Advance, timeout: 30 * time.Second, client: c})
		river.AddWorker(workers, &deliveryWorker[FinalizeArgs]{work: handlers.Finalize, timeout: 30 * time.Second, client: c})
		queues[c.advance], queues[c.maintenance] = river.QueueConfig{MaxWorkers: 2}, river.QueueConfig{MaxWorkers: 2}
	}
	if c.executes() {
		if handlers.Execute == nil || handlers.Cleanup == nil {
			return nil, errors.New("executor requires execute and cleanup handlers")
		}
		river.AddWorker(workers, &deliveryWorker[ExecuteArgs]{work: handlers.Execute, timeout: config.ExecutionTimeout, client: c})
		river.AddWorker(workers, &deliveryWorker[CleanupArgs]{work: handlers.Cleanup, timeout: 30 * time.Second, client: c})
		queues[c.execute], queues[c.cleanup] = river.QueueConfig{MaxWorkers: config.ExecutionWorkers}, river.QueueConfig{MaxWorkers: 2}
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema: config.Schema, Logger: config.Logger, Workers: workers,
		Queues: queues,
		// Split roles enqueue typed, validated jobs for other clients. River's
		// consumer registry remains restricted to this role's own handlers.
		SkipUnknownJobCheck: c.role != execution.RoleAll,
		// River rescue must not race a still valid long-running attempt. Domain
		// lease reconciliation remains authoritative and does not wait for rescue.
		JobTimeout: config.ExecutionTimeout, RescueStuckJobsAfter: config.ExecutionTimeout + time.Hour,
		// Short control transactions enqueue the next graph before River's
		// default 100ms notification throttle expires. Keep those wakeups
		// responsive without increasing idle polling frequency.
		FetchCooldown: time.Millisecond, FetchPollInterval: time.Second,
	})
	if err != nil {
		return nil, err
	}
	c.River = client
	return c, nil
}

func (c *Client) schedules() bool {
	return c.role == execution.RoleAll || c.role == execution.RoleScheduler
}

func (c *Client) executes() bool {
	return c.role == execution.RoleAll || c.role == execution.RoleExecutor
}

func (c *Client) Start(ctx context.Context) error {
	if c.role == execution.RoleAPI {
		return nil
	}
	return c.River.Start(ctx)
}

// Stop first stops fetching and drains for the supplied context's bounded
// period. If that expires, active handlers are cancelled with a separate grace
// period. Ownership and reconciliation decide interrupted domain outcomes.
func (c *Client) Stop(ctx context.Context) error {
	if c.role == execution.RoleAPI {
		return nil
	}
	if err := c.River.Stop(ctx); err != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cancelErr := c.River.StopAndCancel(cancelCtx); cancelErr != nil {
			return errors.Join(err, cancelErr)
		}
		return err
	}
	return nil
}

func (c *Client) InsertAdvance(ctx context.Context, tx pgx.Tx, args AdvanceArgs) error {
	if err := args.Validate(); err != nil {
		return err
	}
	metadata, err := json.Marshal(args)
	if err != nil {
		return err
	}
	_, err = c.River.InsertTx(ctx, tx, args, &river.InsertOpts{Queue: c.advance, Metadata: metadata})
	return err
}

func (c *Client) InsertExecute(ctx context.Context, tx pgx.Tx, args ExecuteArgs) error {
	if err := args.Validate(); err != nil {
		return err
	}
	metadata, err := json.Marshal(args)
	if err != nil {
		return err
	}
	_, err = c.River.InsertTx(ctx, tx, args, &river.InsertOpts{Queue: c.execute, Metadata: metadata})
	return err
}

func (c *Client) InsertFinalize(ctx context.Context, tx pgx.Tx, args FinalizeArgs) error {
	if err := args.Validate(); err != nil {
		return err
	}
	_, err := c.River.InsertTx(ctx, tx, args, &river.InsertOpts{Queue: c.maintenance})
	return err
}

func (c *Client) InsertCleanup(ctx context.Context, tx pgx.Tx, args CleanupArgs) error {
	if err := args.Validate(); err != nil {
		return err
	}
	if args.HostID != c.hostID {
		return errors.New("cleanup delivery targets a different host")
	}
	metadata, err := json.Marshal(args)
	if err != nil {
		return err
	}
	_, err = c.River.InsertTx(ctx, tx, args, &river.InsertOpts{Queue: c.cleanup, Metadata: metadata})
	return err
}

type deliveryArgs interface {
	river.JobArgs
	Validate() error
}

type deliveryWorker[T deliveryArgs] struct {
	river.WorkerDefaults[T]
	work    func(context.Context, *river.Job[T]) error
	timeout time.Duration
	client  *Client
}

func (w *deliveryWorker[T]) Timeout(*river.Job[T]) time.Duration { return w.timeout }

func (w *deliveryWorker[T]) Work(ctx context.Context, job *river.Job[T]) error {
	// River persists and logs panic values. Keep its panic handling and stack,
	// but never allow an arbitrary value to expose an execution payload.
	defer func() {
		if value := recover(); value != nil {
			panic(fmt.Sprintf("execution panic (%T)", value))
		}
	}()
	// Validate again on fetch: manual SQL, retained old jobs or another client
	// must not deliver an incompatible identity to a physical executor.
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(telemetry.RedactError(err))
	}
	attrs := deliveryAttributes(job.Args)
	attrs = append(attrs, attribute.Int64("river.job.id", job.ID), attribute.String("river.queue", job.Queue),
		attribute.String("knotra.engine.id", w.client.engineID), attribute.String("knotra.host.id", w.client.hostID))
	ctx, span := otel.Tracer("knotra/scheduler").Start(ctx, "river."+job.Args.Kind(), trace.WithAttributes(attrs...))
	defer span.End()
	logAttrs := make([]any, 0, len(attrs))
	for _, attr := range attrs {
		logAttrs = append(logAttrs, slog.Any(string(attr.Key), attr.Value.AsInterface()))
	}
	log := w.client.logger.With(logAttrs...)
	log.DebugContext(ctx, "delivery.started")
	err := w.work(ctx, job)
	span.SetAttributes(attribute.Bool("failed", err != nil))
	telemetry.Event(ctx, log, "delivery.finished", err)
	return telemetry.RedactError(err)
}

func deliveryAttributes(args deliveryArgs) []attribute.KeyValue {
	var attempt execution.AttemptID
	var attrs []attribute.KeyValue
	switch args := args.(type) {
	case AdvanceArgs:
		return []attribute.KeyValue{attribute.String("knotra.run.id", args.RunID), attribute.Int64("knotra.wake.generation", args.WakeGeneration)}
	case ExecuteArgs:
		attempt = args.Attempt
		attrs = append(attrs, attribute.Int64("knotra.dispatch.generation", args.DispatchGeneration))
	case FinalizeArgs:
		attempt = args.Attempt
	case CleanupArgs:
		return []attribute.KeyValue{attribute.String("knotra.resource.id", args.ResourceID), attribute.String("knotra.worker.id", args.WorkerID),
			attribute.Int64("knotra.ownership.generation", args.OwnershipGeneration), attribute.Int64("knotra.dispatch.generation", args.DispatchGeneration)}
	}
	return append(attrs, attribute.String("knotra.run.id", attempt.RunID), attribute.String("knotra.instance.id", attempt.InstanceID), attribute.Int("knotra.attempt", attempt.Number))
}

// A pending advance consumes all committed generations under the run lock.
// Keep one viable unconsumed delivery rather than queueing every result wake.
func (c *Client) hasPendingAdvance(ctx context.Context, tx pgx.Tx, run execution.RunState) (bool, error) {
	identity, err := json.Marshal(map[string]any{"runId": run.RunID, "routingVersion": RoutingVersion})
	if err != nil {
		return false, err
	}
	jobs, err := c.River.JobListTx(ctx, tx, river.NewJobListParams().First(1).
		Kinds((AdvanceArgs{}).Kind()).Queues(c.advance).Metadata(string(identity)).
		OrderBy(river.JobListOrderByID, river.SortOrderDesc).States(
		rivertype.JobStateAvailable, rivertype.JobStateRetryable, rivertype.JobStateScheduled, rivertype.JobStateRunning))
	if err != nil || len(jobs.Jobs) == 0 {
		return false, err
	}
	job := jobs.Jobs[0]
	if job.State != rivertype.JobStateRunning && job.ScheduledAt.After(run.Now) {
		return false, nil // A backoff delivery cannot carry an immediate wake.
	}
	var args AdvanceArgs
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		return false, nil //nolint:nilerr // A malformed retained job must not suppress a valid wake; fetch validation rejects it.
	}
	if args.Validate() != nil || args.RunID != run.RunID || args.WakeGeneration <= run.AppliedGeneration || args.WakeGeneration > run.WakeGeneration {
		return false, nil //nolint:nilerr // Invalid or consumed retained identities must not suppress a valid wake.
	}
	return c.hasDelivery(ctx, tx, args, run.Now)
}

// hasDelivery uses River's indexed metadata lookup, independently of job retention.
// The metadata contains the same validated identities as args, never execution data.
func (c *Client) hasDelivery(ctx context.Context, tx pgx.Tx, args deliveryArgs, now time.Time) (bool, error) {
	queue, timeout := c.advance, 30*time.Second
	if _, ok := args.(ExecuteArgs); ok {
		queue, timeout = c.execute, c.executionTimeout
	}
	if _, ok := args.(CleanupArgs); ok {
		queue = c.cleanup
	}
	identity, err := json.Marshal(args)
	if err != nil {
		return false, err
	}
	jobs, err := c.River.JobListTx(ctx, tx, river.NewJobListParams().First(1).Kinds(args.Kind()).Queues(queue).Metadata(string(identity)).States(
		rivertype.JobStateAvailable, rivertype.JobStateRetryable, rivertype.JobStateScheduled, rivertype.JobStateRunning))
	if err != nil || len(jobs.Jobs) == 0 {
		return false, err
	}
	job := jobs.Jobs[0]
	if job.State == rivertype.JobStateRunning {
		if job.AttemptedAt == nil || !job.AttemptedAt.Add(timeout).After(now) {
			return false, nil
		}
		if len(job.AttemptedBy) > 0 {
			heartbeat, err := db.New(tx).ReadDeliveryWorkerHeartbeat(ctx, db.ReadDeliveryWorkerHeartbeatParams{EngineID: c.engineID, DeliveryClientID: new(job.AttemptedBy[len(job.AttemptedBy)-1])})
			if err == nil {
				return heartbeat.Add(execution.LeaseDuration).After(now), nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return false, err
			}
		}
		return true, nil
	}
	return true, nil
}
