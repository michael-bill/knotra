// Package app wires the independently testable engine modules into one service.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	temporalclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/michael-bill/knotra/internal/adapters"
	"github.com/michael-bill/knotra/internal/api"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/engine"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/executor"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/queue"
	"github.com/michael-bill/knotra/internal/store"
	storedb "github.com/michael-bill/knotra/internal/store/db"
	"github.com/michael-bill/knotra/internal/telemetry"
)

type Options struct {
	Backend          string
	HostID           string
	SharedDataDir    string
	ExecutionWorkers int
	Listen           string
	DatabaseURL      string
	TemporalAddress  string
	Namespace        string
	TaskQueue        string
	DataDir          string
	DockerHost       string
	HelperPath       string
	FirewallImage    string
	Token            string
	CORSOrigin       string
	TLSCert          string
	TLSKey           string
	Version          string
	Profiles         []string
}

func Serve(ctx context.Context, o Options, log *slog.Logger) (serveErr error) {
	if o.Backend == "" {
		o.Backend = execution.BackendRiver
	}
	if o.Backend != execution.BackendTemporal && o.Backend != execution.BackendRiver {
		return fmt.Errorf("unsupported execution backend %q", o.Backend)
	}
	if o.HostID == "" {
		o.HostID = "local"
	}
	if err := execution.ValidateHostID(o.HostID); err != nil {
		return err
	}
	if o.ExecutionWorkers == 0 {
		o.ExecutionWorkers = 16
	}
	if o.ExecutionWorkers < 1 {
		return errors.New("execution workers must be positive")
	}
	stopTelemetry, err := telemetry.Start(ctx, o.Version)
	if err != nil {
		return err
	}
	defer func() {
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = stopTelemetry(ctx)
	}()

	host, _, err := net.SplitHostPort(o.Listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !local && (o.Token == "" || o.TLSCert == "" || o.TLSKey == "") {
		return errors.New("remote listeners require bearer authentication and TLS certificate/key")
	}
	profiles := map[string]contract.Profile{}

	for _, path := range o.Profiles {
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		p, d := contract.ParseProfile(b)
		if contract.HasErrors(d) {
			return fmt.Errorf("profile %s: %v", path, d)
		}
		if _, exists := profiles[p.Metadata.Name]; exists {
			return fmt.Errorf("duplicate profile %q", p.Metadata.Name)
		}
		profiles[p.Metadata.Name] = p
	}

	if len(profiles) == 0 {
		return errors.New("at least one --profile file is required")
	}
	db, err := store.Open(ctx, o.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	lease, err := db.AcquireLease(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Close(context.Background()) }()
	// Separate databases must never consume each other's activity tasks.
	o.TaskQueue += "." + db.EngineID
	dataDir, err := filepath.Abs(o.DataDir)
	if err != nil {
		return err
	}
	sharedDir := dataDir
	if o.SharedDataDir != "" {
		sharedDir, err = filepath.Abs(o.SharedDataDir)
		if err != nil {
			return err
		}
	}
	artifacts := store.Artifacts{Root: filepath.Join(sharedDir, "artifacts"), Store: db}
	runner := &adapters.Runner{
		EngineID:      db.EngineID,
		DockerHost:    o.DockerHost,
		HelperPath:    o.HelperPath,
		WorkDir:       filepath.Join(dataDir, "work"),
		FirewallImage: o.FirewallImage,
	}
	defer func() { _ = runner.Close() }()
	srv := &api.Server{Store: db, Artifacts: artifacts, Profiles: profiles, Token: o.Token, Version: o.Version, CORSOrigin: o.CORSOrigin, Backend: o.Backend, HostID: o.HostID, Log: log, Admit: func(ctx context.Context, p *contract.Plan) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return runner.Prepare(ctx, p)
	}}

	var tc temporalclient.Client
	var runtimeDone <-chan struct{}
	var runtimeFailures <-chan error
	var native *queue.Runtime
	backends, err := storedb.New(db.Pool).RequiredExecutionBackends(ctx)
	if err != nil {
		return err
	}
	riverNeeded, temporalNeeded := o.Backend == execution.BackendRiver, o.Backend == execution.BackendTemporal
	for _, backend := range backends {
		riverNeeded = riverNeeded || backend == execution.BackendRiver
		temporalNeeded = temporalNeeded || backend == execution.BackendTemporal
	}
	if temporalNeeded {
		// Legacy cleanup must finish before either worker starts. River resources
		// have durable identities and may only be removed through ownership claims.
		cleanupCtx, stopCleanup := context.WithTimeout(ctx, 30*time.Second)
		cleanupErr := runner.CleanupOwned(cleanupCtx, func(ctx context.Context, id string) (bool, error) {
			_, err := storedb.New(db.Pool).ReadResourceIdentity(ctx, storedb.ReadResourceIdentityParams{ID: id, EngineID: db.EngineID})
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return err == nil, err
		})
		stopCleanup()
		if cleanupErr != nil {
			return fmt.Errorf("clean abandoned sandboxes: %w", cleanupErr)
		}
	}
	if riverNeeded {
		config := queue.Config{EngineID: db.EngineID, HostID: o.HostID, Logger: log, ExecutionWorkers: o.ExecutionWorkers}
		// The combined service holds the exclusive engine lease. Cluster host
		// identities and placement are enabled separately after acceptance.
		for _, profile := range profiles {
			duration, err := contract.Duration(profile.Spec.Limits.Timeout)
			if err != nil {
				return err
			}
			config.ExecutionTimeout = max(config.ExecutionTimeout, duration+time.Minute)
		}
		// Replacing a profile must not shorten an already admitted plan's delivery.
		micros, err := storedb.New(db.Pool).MaximumActiveExecutionDurationMicros(ctx)
		if err != nil {
			return err
		}
		if micros < 0 || micros > int64(365*24*time.Hour/time.Microsecond) {
			return errors.New("stored execution duration exceeds the supported admission bound")
		}
		config.ExecutionTimeout = max(config.ExecutionTimeout, time.Duration(micros)*time.Microsecond+time.Minute)
		config.Schema, err = storedb.New(db.Pool).CurrentSchema(ctx)
		if err != nil {
			return err
		}
		if err := queue.Migrate(ctx, db.Pool, config.Schema); err != nil {
			return err
		}
		outcomes, err := execution.OpenOutcomeFiles(filepath.Join(sharedDir, "outcomes"))
		if err != nil {
			return err
		}
		defer func() { _ = outcomes.Close() }()
		// Runtime.Start sets ownership fields on its runner before use. Keep the
		// legacy/admission runner unchanged when both workers share this service.
		riverRunner := runner.WithHooks(nil)
		native = &queue.Runtime{Store: db, Host: &executor.Host{Store: db, Artifacts: artifacts, Runner: riverRunner},
			Outcomes: outcomes, WorkerID: uuid.NewString(), HostID: config.HostID}
		native.Client, err = queue.New(db.Pool, config, native.Handlers())
		if err != nil {
			return err
		}
		if err := native.Start(ctx); err != nil {
			return err
		}
		defer func() {
			stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if stopErr := native.Stop(stopCtx); stopErr != nil {
				log.Error("execution shutdown failed", "engineId", db.EngineID, "hostId", native.HostID, "workerId", native.WorkerID, "error", telemetry.RedactError(stopErr))
				serveErr = errors.Join(serveErr, stopErr)
			}
		}()
		srv.Wake = native.CommandWake
		srv.ExecutionTimeout = config.ExecutionTimeout
		srv.WorkerBackends = append(srv.WorkerBackends, execution.BackendRiver)
		runtimeDone, runtimeFailures = native.Done(), native.Failure
	}
	if temporalNeeded {
		blobs, err := engine.NewFileBlobStore(filepath.Join(dataDir, "payloads"))
		if err != nil {
			return err
		}
		dc := converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), &engine.PayloadCodec{Store: blobs})
		tc, err = temporalclient.DialContext(ctx, temporalclient.Options{
			HostPort:      o.TemporalAddress,
			Namespace:     o.Namespace,
			DataConverter: dc,
			Logger:        temporalLog{log},
		})
		if err != nil {
			return fmt.Errorf("connect Temporal: %w", err)
		}
		defer tc.Close()
		acts := &activities{db: db, artifacts: artifacts, runner: runner}
		w := worker.New(tc, o.TaskQueue, worker.Options{WorkerStopTimeout: 10 * time.Second})
		w.RegisterWorkflowWithOptions(engine.Workflow, workflow.RegisterOptions{Name: engine.WorkflowName})
		w.RegisterActivityWithOptions(acts.execute, activity.RegisterOptions{Name: engine.ExecuteActivity})
		w.RegisterActivityWithOptions(acts.plan, activity.RegisterOptions{Name: engine.PlanActivity})
		w.RegisterActivityWithOptions(acts.project, activity.RegisterOptions{Name: engine.ProjectActivity})
		w.RegisterActivityWithOptions(db.SaveRequest, activity.RegisterOptions{Name: engine.RequestActivity})
		w.RegisterActivityWithOptions(db.Answer, activity.RegisterOptions{Name: engine.AnswerActivity})
		w.RegisterActivityWithOptions(db.Resolution, activity.RegisterOptions{Name: engine.ResolutionActivity})
		if err = w.Start(); err != nil {
			return err
		}
		defer w.Stop()
		srv.WorkerBackends = append(srv.WorkerBackends, execution.BackendTemporal)
	}
	server := &http.Server{
		Addr:              o.Listen,
		Handler:           telemetry.HTTP(srv.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", o.Listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	deliveryDone := make(chan struct{})
	if tc != nil {
		go func() { defer close(deliveryDone); deliver(serviceCtx, db, tc, o.TaskQueue, log) }()
	} else {
		close(deliveryDone)
	}
	leaseFailed := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-serviceCtx.Done():
				return
			case <-ticker.C:
			}

			checkCtx, stop := context.WithTimeout(serviceCtx, 5*time.Second)
			err := lease.Check(checkCtx)
			stop()
			if err != nil {
				if serviceCtx.Err() == nil {
					leaseFailed <- err
				}
				return
			}
		}
	}()
	failures := make(chan error, 1)
	go func() {
		if o.TLSCert != "" {
			failures <- server.ServeTLS(listener, o.TLSCert, o.TLSKey)
		} else {
			failures <- server.Serve(listener)
		}
	}()
	log.Info(
		"Knotra engine listening",
		"address",
		listener.Addr().String(),
		"engineId",
		db.EngineID,
		"backend",
		o.Backend,
	)

serveLoop:
	for {
		select {
		case <-ctx.Done():
			break serveLoop
		case problem := <-runtimeFailures:
			log.Warn("execution background check failed", "engineId", db.EngineID, "hostId", native.HostID, "workerId", native.WorkerID, "error", telemetry.RedactError(problem))
		case <-runtimeDone:
			err = native.Err()
			break serveLoop
		case err = <-leaseFailed:
			log.Error("engine ownership connection lost; stopping service")
			break serveLoop
		case err = <-failures:
			break serveLoop
		}
	}

	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	<-deliveryDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return shutdownErr
}

func deliver(ctx context.Context, db *store.Store, tc temporalclient.Client, queue string, log *slog.Logger) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := db.Deliver(callCtx, func(ctx context.Context, query store.Querier, runID, kind string, payload []byte) error {
			switch kind {
			case "start":
				var input engine.RunInput
				if e := json.Unmarshal(payload, &input); e != nil {
					return e
				}
				name, e := store.ReadRunName(ctx, query, runID)
				if e != nil {
					return e
				}
				_, e = tc.ExecuteWorkflow(
					ctx,
					temporalclient.StartWorkflowOptions{
						ID:                    runID,
						TaskQueue:             queue,
						WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
						StaticSummary:         "Knotra · " + name,
						StaticDetails:         "Pipeline: " + name + "\n\nKnotra run: " + runID,
					},
					engine.WorkflowName,
					input,
				)
				var exists *serviceerror.WorkflowExecutionAlreadyStarted
				if errors.As(e, &exists) {
					return nil
				}
				return e
			case "human":
				var signal engine.HumanSignal
				if e := json.Unmarshal(payload, &signal); e != nil {
					return e
				}
				return closedSignal(ctx, query, runID, tc.SignalWorkflow(ctx, runID, "", engine.HumanSignalName, signal))
			case "resolve":
				var signal engine.ResolutionSignal
				if e := json.Unmarshal(payload, &signal); e != nil {
					return e
				}
				return closedSignal(ctx, query, runID, tc.SignalWorkflow(ctx, runID, "", engine.ResolveSignalName, signal))
			case "cancel":
				var signal engine.CancelSignal
				if e := json.Unmarshal(payload, &signal); e != nil {
					return e
				}
				e := tc.SignalWorkflow(ctx, runID, "", engine.CancelSignalName, signal)
				return closedSignal(ctx, query, runID, e)
			default:
				return fmt.Errorf("unknown outbox command %q", kind)
			}
		})
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Warn("durable command delivery will retry", "error", err)
		}
	}
}

type activities struct {
	db        *store.Store
	artifacts store.Artifacts
	runner    *adapters.Runner
}

func (a *activities) project(ctx context.Context, p engine.Projection) error {
	if e := a.db.Project(ctx, p); e != nil {
		return e
	}
	if p.InstanceID == "" && protocol.Terminal(p.Status) {
		return a.runner.ReleaseRun(p.RunID)
	}
	return nil
}

func (a *activities) execute(ctx context.Context, q engine.ExecuteRequest) (engine.ExecuteResult, error) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				activity.RecordHeartbeat(ctx, q.InstanceID)
			}
		}
	}()
	host := executor.Host{Store: a.db, Artifacts: a.artifacts, Runner: a.runner}
	return host.Execute(ctx, q), nil
}

type temporalLog struct{ log *slog.Logger }

func (l temporalLog) Debug(msg string, keyvals ...any) { l.log.Debug(msg, keyvals...) }

func (l temporalLog) Info(msg string, keyvals ...any) { l.log.Info(msg, keyvals...) }

func (l temporalLog) Warn(msg string, keyvals ...any) { l.log.Warn(msg, keyvals...) }

func (l temporalLog) Error(msg string, keyvals ...any) { l.log.Error(msg, keyvals...) }

func closedSignal(ctx context.Context, query store.Querier, runID string, err error) error {
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		status, readErr := store.ReadRunStatus(ctx, query, runID)
		if readErr == nil && protocol.Terminal(status) {
			return nil
		}
	}
	return err
}

func (a *activities) plan(ctx context.Context, q engine.PlanRequest) (contract.Plan, error) {
	return a.db.Plan(ctx, q.RunID)
}
