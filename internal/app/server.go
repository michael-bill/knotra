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
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
	"github.com/michael-bill/knotra/internal/telemetry"
)

type Options struct {
	Listen          string
	DatabaseURL     string
	TemporalAddress string
	Namespace       string
	TaskQueue       string
	DataDir         string
	DockerHost      string
	HelperPath      string
	FirewallImage   string
	Token           string
	CORSOrigin      string
	TLSCert         string
	TLSKey          string
	Version         string
	Profiles        []string
}

func Serve(ctx context.Context, o Options, log *slog.Logger) error {
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
	defer lease.Close(context.Background())
	// Separate databases must never consume each other's activity tasks.
	o.TaskQueue += "." + db.EngineID
	dataDir, err := filepath.Abs(o.DataDir)
	if err != nil {
		return err
	}
	blobs, err := engine.NewFileBlobStore(filepath.Join(dataDir, "payloads"))
	if err != nil {
		return err
	}
	dc := converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), &engine.PayloadCodec{Store: blobs})
	tc, err := temporalclient.Dial(temporalclient.Options{
		HostPort:      o.TemporalAddress,
		Namespace:     o.Namespace,
		DataConverter: dc,
		Logger:        temporalLog{log},
	})
	if err != nil {
		return fmt.Errorf("connect Temporal: %w", err)
	}
	defer tc.Close()
	artifacts := store.Artifacts{Root: filepath.Join(dataDir, "artifacts"), Store: db}
	runner := &adapters.Runner{
		EngineID:      db.EngineID,
		DockerHost:    o.DockerHost,
		HelperPath:    o.HelperPath,
		WorkDir:       filepath.Join(dataDir, "work"),
		FirewallImage: o.FirewallImage,
	}
	defer runner.Close()
	cleanupCtx, stopCleanup := context.WithTimeout(ctx, 30*time.Second)
	cleanupErr := runner.CleanupOwned(cleanupCtx)
	stopCleanup()
	if cleanupErr != nil {
		return fmt.Errorf("clean abandoned sandboxes: %w", cleanupErr)
	}
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
	srv := &api.Server{Store: db, Artifacts: artifacts, Profiles: profiles, Token: o.Token, Version: o.Version, CORSOrigin: o.CORSOrigin, Log: log, Admit: func(ctx context.Context, p *contract.Plan) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return runner.Prepare(ctx, p)
	}}
	server := &http.Server{
		Addr:              o.Listen,
		Handler:           telemetry.HTTP(srv.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	listener, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	deliveryDone := make(chan struct{})
	go func() { defer close(deliveryDone); deliver(serviceCtx, db, tc, o.TaskQueue, log) }()
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
		"taskQueue",
		o.TaskQueue,
	)

	select {
	case <-ctx.Done():
	case err = <-leaseFailed:
		log.Error("engine ownership connection lost; stopping service")
	case err = <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
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
		a.runner.ReleaseRun(p.RunID)
	}
	return nil
}

func (a *activities) execute(ctx context.Context, q engine.ExecuteRequest) (engine.ExecuteResult, error) {
	ctx, span := otel.Tracer("knotra/engine").Start(ctx, "node."+q.Node.Type)
	defer span.End()
	span.SetAttributes(
		attribute.String("knotra.run.id", q.RunID),
		attribute.String("knotra.instance.id", q.InstanceID),
		attribute.Int("knotra.attempt", q.Attempt),
	)

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
	var loadErr error
	q.Plan, loadErr = a.db.Plan(ctx, q.RunID)
	if loadErr != nil {
		return engine.ExecuteResult{Failure: &engine.Failure{Code: "STORAGE_UNAVAILABLE", Message: "cannot load admitted plan", Retryable: true}}, nil
	}
	h := &executionHooks{store: a.db, artifacts: a.artifacts, request: q}
	scopeID := q.RunID
	if len(q.Scopes) > 0 {
		scopeID = q.Scopes[len(q.Scopes)-1].ID
	}
	outputs, e := a.runner.WithHooks(h).Execute(
		ctx,
		adapters.Request{
			RunID:         q.RunID,
			InstanceID:    q.InstanceID,
			Attempt:       q.Attempt,
			Pipeline:      q.Pipeline,
			ScopeID:       scopeID,
			Plan:          &q.Plan,
			Node:          q.Node,
			Inputs:        q.Inputs,
			ToolArguments: q.ToolArguments,
		},
	)
	if e == nil {
		return engine.ExecuteResult{Outputs: outputs}, nil
	}
	var f *adapters.Failure
	if errors.As(e, &f) {
		return engine.ExecuteResult{Failure: &engine.Failure{
			Code:                  f.Code,
			Message:               f.Message,
			Retryable:             f.Retryable,
			Unknown:               f.Unknown,
			OperationID:           f.OperationID,
			CanRetryIfNotExecuted: q.Node.Type != "agent",
		}}, nil
	}
	code := "EXECUTION_FAILED"
	if strings.Contains(e.Error(), "BUDGET_EXCEEDED") {
		code = "BUDGET_EXCEEDED"
	}
	if errors.Is(e, context.Canceled) {
		code = "CANCELLED"
	}
	if errors.Is(e, context.DeadlineExceeded) {
		code = "TIMEOUT"
	}
	return engine.ExecuteResult{Failure: &engine.Failure{Code: code, Message: e.Error()}}, nil
}

type executionHooks struct {
	store     *store.Store
	artifacts store.Artifacts
	request   engine.ExecuteRequest
}

func (h *executionHooks) Reserve(ctx context.Context, kind string) error {
	return h.store.Reserve(ctx, h.request.RunID, kind, h.request.Scopes)
}

func (h *executionHooks) PutArtifact(ctx context.Context, name, mime string, b []byte) (contract.Artifact, error) {
	return h.artifacts.Put(
		ctx,
		name,
		mime,
		b,
		map[string]string{
			"runId":      h.request.RunID,
			"instanceId": h.request.InstanceID,
			"attemptId":  fmt.Sprintf("%s.a%d", h.request.InstanceID, h.request.Attempt),
		},
	)
}

func (h *executionHooks) GetArtifact(ctx context.Context, id string) ([]byte, error) {
	return h.artifacts.Get(ctx, id)
}

func (h *executionHooks) BeginOperation(ctx context.Context, op adapters.Operation) (adapters.OperationState, error) {
	s, e := h.store.BeginOperation(ctx, op.ID, h.request.RunID, op.Kind, op.Effect)
	return adapters.OperationState{Started: s.Started, Completed: s.Completed, Response: s.Response}, e
}

func (h *executionHooks) CompleteOperation(ctx context.Context, id string, b json.RawMessage) error {
	return h.store.CompleteOperation(ctx, id, b)
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
