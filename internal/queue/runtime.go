package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/executor"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/scheduler"
	"github.com/michael-bill/knotra/internal/store"
	"github.com/michael-bill/knotra/internal/store/db"
	"github.com/michael-bill/knotra/internal/telemetry"
)

const stepTransitions = 64

// Runtime composes the pure scheduler, PostgreSQL mutations and River delivery.
// Create Client with Handlers before Start. Stop it before closing the store.
type Runtime struct {
	Store    *store.Store
	Host     *executor.Host
	Outcomes *execution.OutcomeFiles
	Client   *Client
	WorkerID string
	HostID   string
	Failure  chan error

	workCtx context.Context
	cancel  context.CancelCauseFunc
	done    chan struct{}
	// ponytail: scan 64 expired outcome keys per tick; the cursor prevents a
	// missing envelope from starving later keys. Partition if scans dominate.
	outcomeCursor   string
	deliveryCursor  string
	resourceCursor  string
	containerCursor string
	sessionCursor   string
}

func (r *Runtime) Handlers() Handlers {
	return Handlers{Advance: r.advance, Execute: r.execute, Finalize: r.finalize, Cleanup: r.cleanup}
}

// Done closes when maintenance stops, including loss of the worker heartbeat.
func (r *Runtime) Done() <-chan struct{} { return r.done }

func (r *Runtime) Err() error {
	if r.workCtx == nil {
		return nil
	}
	return context.Cause(r.workCtx)
}

func (r *Runtime) Start(ctx context.Context) error {
	if r.Store == nil || r.Host == nil || r.Host.Runner == nil || r.Outcomes == nil || r.Client == nil {
		return errors.New("runtime requires store, host, outcomes and queue client")
	}
	if r.workCtx != nil {
		return errors.New("runtime process incarnation cannot be restarted")
	}
	if r.HostID != r.Client.hostID || r.Store.EngineID != r.Client.engineID {
		return errors.New("runtime queue does not match its engine and host identity")
	}
	if (r.Host.Runner.EngineID != "" && r.Host.Runner.EngineID != r.Store.EngineID) || (r.Host.Runner.HostID != "" && r.Host.Runner.HostID != r.HostID) {
		return errors.New("runtime runner does not match its engine and host identity")
	}
	r.Host.Runner.EngineID, r.Host.Runner.HostID = r.Store.EngineID, r.HostID
	r.Host.Runner.ResourceEvidence = r.Outcomes
	if err := r.Store.RegisterWorker(ctx, r.WorkerID, r.HostID, r.Client.role); err != nil {
		return err
	}
	if err := r.Store.BindWorkerDeliveryClient(ctx, r.WorkerID, r.Client.River.ID()); err != nil {
		return err
	}
	r.Client.logger = r.Client.logger.With("actorId", r.WorkerID)
	if r.Host.Log == nil {
		r.Host.Log = r.Client.logger
	}
	r.workCtx, r.cancel = context.WithCancelCause(context.WithoutCancel(ctx))
	r.Failure = make(chan error, 1)
	r.done = make(chan struct{})
	registration, err := r.observeMetrics()
	if err != nil {
		r.cancel(err)
		close(r.done)
		return err
	}
	if err := r.Client.Start(r.workCtx); err != nil {
		_ = registration.Unregister()
		r.cancel(err)
		close(r.done)
		return err
	}
	go r.maintain(registration)
	return nil
}

func (r *Runtime) Stop(ctx context.Context) error {
	if r.cancel == nil {
		return nil
	}
	drainErr := r.Store.DrainWorker(ctx, r.WorkerID)
	err := r.Client.Stop(ctx)
	r.cancel(context.Canceled)
	select {
	case <-r.done:
	case <-ctx.Done():
		return errors.Join(drainErr, err, ctx.Err())
	}
	return errors.Join(drainErr, err)
}

func (r *Runtime) maintain(registration metric.Registration) {
	pollsDone := make(chan struct{})
	go func() {
		defer close(pollsDone)
		var polls []func(context.Context) error
		if r.Client.schedules() {
			polls = append(polls, r.pumpTimers, r.reconcileOutcomes, r.reconcileDeliveries)
		}
		if r.Client.executes() {
			polls = append(polls, r.reconcileResources, r.reconcilePhysicalResources, r.reconcileSessions)
		}
		if len(polls) == 0 {
			return
		}
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-r.workCtx.Done():
				return
			case <-timer.C:
			}
			// Each reconciliation has its own bound and retries independently.
			// A bad envelope or busy run must not suppress unrelated cleanup.
			for _, poll := range polls {
				ctx, cancel := context.WithTimeout(r.workCtx, execution.WorkerHeartbeat)
				err := poll(ctx)
				cancel()
				if err != nil && r.workCtx.Err() == nil {
					select {
					case r.Failure <- err:
					default:
					}
				}
			}
		}
	}()
	defer func() {
		<-pollsDone
		_ = registration.Unregister()
		close(r.done)
	}()
	heartbeat := time.NewTicker(execution.WorkerHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.workCtx.Done():
			return
		case <-heartbeat.C:
			ctx, cancel := context.WithTimeout(r.workCtx, execution.WorkerHeartbeat)
			err := r.Store.HeartbeatWorker(ctx, r.WorkerID)
			cancel()
			if err != nil {
				select {
				case r.Failure <- err:
				default:
				}
				r.cancel(err)
				return
			}
		}
	}
}

// Wake ensures a validated pending scheduler delivery in the caller's transaction.
func (r *Runtime) Wake(ctx context.Context, tx pgx.Tx, runID string, generation int64) error {
	args := AdvanceArgs{RunID: runID, WakeGeneration: generation, RoutingVersion: RoutingVersion}
	if err := args.Validate(); err != nil {
		return err
	}
	run, err := store.LockExecutionRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	pending, err := r.Client.hasPendingAdvance(ctx, tx, run)
	if err != nil || pending {
		return err
	}
	return r.Client.InsertAdvance(ctx, tx, args)
}

// CommandWake consumes a validated request/stop command in its receipt
// transaction, then inserts its durable delivery. No leaf work runs here.
func (r *Runtime) CommandWake(ctx context.Context, tx pgx.Tx, runID string, generation int64) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	plan, err := store.ReadPlan(ctx, tx, runID)
	if err != nil {
		return err
	}
	if err := r.advanceTx(ctx, tx, plan, runID, ""); err != nil {
		return err
	}
	return r.Wake(ctx, tx, runID, generation)
}

func (r *Runtime) advance(ctx context.Context, job *river.Job[AdvanceArgs]) error {
	plan, err := r.Store.Plan(ctx, job.Args.RunID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		applied, err := store.ApplyWake(ctx, tx, job.Args.RunID, job.Args.WakeGeneration)
		if err != nil {
			return err
		}
		if applied {
			if err := r.advanceTx(ctx, tx, plan, job.Args.RunID, ""); err != nil {
				return err
			}
		}
		_, err = river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job)
		return err
	})
}

func (r *Runtime) advanceTx(ctx context.Context, tx pgx.Tx, plan contract.Plan, runID, focusInstanceID string) error {
	ctx, span := otel.Tracer("knotra/scheduler").Start(ctx, "scheduler.advance", trace.WithAttributes(
		attribute.String("knotra.run.id", runID), attribute.String("knotra.instance.id", focusInstanceID),
		attribute.String("knotra.engine.id", r.Store.EngineID), attribute.String("knotra.host.id", r.HostID), attribute.String("knotra.actor.id", r.WorkerID)))
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	run, err := store.LockExecutionRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if run.Backend != execution.BackendRiver || run.SchedulerVersion != execution.SchedulerVersion || run.StateFormatVersion != execution.StateFormatVersion {
		return store.ErrExecutionVersion
	}
	span.SetAttributes(attribute.Int64("knotra.wake.generation", run.WakeGeneration))
	if protocol.Terminal(run.Status) {
		return nil
	}
	graphs, err := db.New(tx).ListRunnableGraphs(ctx, db.ListRunnableGraphsParams{RunID: runID, GraphCursor: run.GraphCursor, FocusInstanceID: focusInstanceID})
	if err != nil {
		return err
	}
	// Publication visits its affected graph without moving the shared scan
	// cursor. A durable general wake propagates parent results and freed slots.
	remaining, more, progress, visited := stepTransitions, focusInstanceID != "", false, 0
	for _, graph := range graphs {
		if remaining == 0 {
			more = true
			break
		}
		changes, err := r.advanceGraphTx(ctx, tx, plan, runID, graph.ID, focusInstanceID, remaining)
		if err != nil {
			return err
		}
		remaining -= changes.Transitions
		visited++
		progress = progress || changes.Transitions > 0 || changes.Graph != nil
		more = more || changes.More || (changes.Graph != nil && changes.Graph.ParentInstanceID != "" && protocol.Terminal(changes.Graph.State))
		if focusInstanceID == "" {
			if _, err := db.New(tx).SetGraphCursor(ctx, db.SetGraphCursorParams{ID: runID, GraphCursor: graph.Address}); err != nil {
				return err
			}
		}
	}
	if progress {
		if _, err := db.New(tx).MarkGraphScanProgress(ctx, runID); err != nil {
			return err
		}
	}
	if focusInstanceID == "" && visited == len(graphs) {
		if len(graphs) == stepTransitions {
			more = true
		} else {
			run, err = store.LockExecutionRun(ctx, tx, runID)
			if err != nil {
				return err
			}
			more = more || run.GraphScanAgain
			if _, err := db.New(tx).FinishGraphScan(ctx, runID); err != nil {
				return err
			}
		}
	}
	root, err := store.ReadExecutionGraphMetadata(ctx, tx, runID, execution.StableID("g", runID+"/root"))
	if err != nil {
		return err
	}
	if protocol.Terminal(root.State) {
		active, err := db.New(tx).HasActiveExecution(ctx, runID)
		if err != nil {
			return err
		}
		if !active {
			run, err = store.LockExecutionRun(ctx, tx, runID)
			if err != nil {
				return err
			}
			return r.Store.ProjectExecution(ctx, tx, execution.Projection{RunID: runID, Kind: "run", Status: root.State,
				Outputs: root.Outputs, Failure: root.Failure, Time: run.Now})
		}
	}
	if more {
		_, err = store.WakeExecution(ctx, tx, runID, r.Wake)
	}
	return err
}

func (r *Runtime) advanceGraphTx(ctx context.Context, tx pgx.Tx, plan contract.Plan, runID, graphID, focusInstanceID string, bound int) (changes scheduler.Changes, err error) {
	graph, err := store.ReadExecutionGraphMetadata(ctx, tx, runID, graphID)
	if err != nil {
		return changes, err
	}
	run, err := store.LockExecutionRun(ctx, tx, runID)
	if err != nil {
		return changes, err
	}
	definition, err := execution.GraphDefinition(plan, graph.Pipeline, graph.GraphPath)
	if err != nil {
		return changes, err
	}
	nodes, err := store.ReadSchedulableNodes(ctx, tx, run, graph, focusInstanceID, bound)
	if err != nil {
		return changes, err
	}
	instanceIDs, nodeIDs := make([]string, 0, len(nodes)), make([]string, 0, len(nodes))
	var dependencies []string
	for _, node := range nodes {
		instanceIDs = append(instanceIDs, node.ID)
		nodeIDs = append(nodeIDs, node.NodeID)
		if node.State == "pending" {
			dependencies = append(dependencies, definition.Nodes[node.NodeID].Dependencies...)
		}
	}
	if graph.State == "pending" {
		graph.Inputs, err = store.ReadExecutionGraphInputValues(ctx, tx, runID, graph.ID, nil)
	} else {
		// Admission may finish materialization and admit new nodes in this step.
		// Load only statically referenced graph inputs, including conditions.
		needed := map[string]bool{}
		if graph.MaterializationCursor == len(definition.Nodes) {
			for _, node := range nodes {
				if node.State == "pending" {
					needed[node.NodeID] = true
				}
			}
		} else if graph.MaterializationCursor+bound > len(definition.Nodes) {
			for _, node := range nodes {
				if node.State == "pending" {
					needed[node.NodeID] = true
				}
			}
			ids := make([]string, 0, len(definition.Nodes))
			for id := range definition.Nodes {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			for _, id := range ids[graph.MaterializationCursor:] {
				needed[id] = true
			}
		}
		var bindings []contract.Binding
		for id := range needed {
			definitionNode := definition.Nodes[id]
			for _, port := range definitionNode.Inputs {
				if port.Bind != nil {
					bindings = append(bindings, *port.Bind)
				}
			}
			if definitionNode.When != "" {
				bindings = append(bindings, contract.Binding{Expr: definitionNode.When})
			}
		}
		var names []string
		names, err = inputReferenceNames(bindings, "inputs")
		if err == nil {
			graph.Inputs, err = store.ReadExecutionGraphInputValues(ctx, tx, runID, graph.ID, names)
		}
	}
	if err != nil {
		return changes, err
	}
	slices.Sort(dependencies)
	dependencyValues, err := store.ReadExecutionDependencyValues(ctx, tx, runID, graph.ID, slices.Compact(dependencies))
	if err != nil {
		return changes, err
	}
	attempts, err := store.ReadExecutionAttempts(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	scopes, err := store.ReadExecutionScopes(ctx, tx, graph)
	if err != nil {
		return changes, err
	}
	timers, err := store.ReadExecutionRetryTimers(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	requests, err := store.ReadExecutionRequests(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	controls, err := store.ReadExecutionControls(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	controlByID := make(map[string]execution.ControlRecord, len(controls))
	for _, control := range controls {
		controlByID[control.InstanceID] = control
	}
	for i := range nodes {
		node := &nodes[i]
		if !node.InputsOmitted {
			continue
		}
		frozen := definition.Nodes[node.NodeID]
		control, found := controlByID[node.ID]
		if !found || frozen.Foreach == nil {
			return changes, errors.New("foreach snapshot has no frozen control")
		}
		if !run.Paused && run.StopCause == nil && !run.Cancelled && control.NextPosition < control.ElementCount && control.ActiveChildren < control.Concurrency {
			bindings := make([]contract.Binding, 0, len(frozen.Foreach.With))
			for _, binding := range frozen.Foreach.With {
				bindings = append(bindings, binding)
			}
			names, err := inputReferenceNames(bindings, "args")
			if err != nil {
				return changes, err
			}
			node.Inputs, err = store.ReadExecutionNodeInputValues(ctx, tx, runID, node.ID, names)
			if err != nil {
				return changes, err
			}
		}
		frozen.Execution = execution.ExecutionPolicy(plan.Pipelines[graph.Pipeline].Spec.Defaults.Execution, frozen.Execution)
		node.Request = &execution.ExecuteRequest{RunID: runID, InstanceID: node.ID, NodeID: node.NodeID, Pipeline: graph.Pipeline,
			Attempt: 1, Node: frozen, Inputs: node.Inputs, Scopes: graph.Scopes, Permissions: graph.Permissions, Deadline: node.Deadline}
	}
	children, err := store.ReadChildExecutionGraphs(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	unresolved, err := db.New(tx).HasUnresolvedOutsideSelection(ctx, db.HasUnresolvedOutsideSelectionParams{RunID: runID, InstanceIds: instanceIDs})
	if err != nil {
		return changes, err
	}
	active, err := db.New(tx).HasActiveNodesOutsideSelection(ctx, db.HasActiveNodesOutsideSelectionParams{RunID: runID, GraphID: graph.ID, InstanceIds: instanceIDs})
	if err != nil {
		return changes, err
	}
	snapshot := scheduler.Snapshot{Graph: graph, Nodes: append(dependencyValues, nodes...), Attempts: attempts, Scopes: scopes,
		Timers: timers, Requests: requests, Controls: controls, Children: children, UnresolvedElsewhere: unresolved,
		FocusInstanceID: focusInstanceID, SelectedNodeIDs: nodeIDs, ActiveNodesElsewhere: active}
	// At most two pure preparation passes: a selected foreach's collection,
	// then the graph's export references. No proposed mutation is saved yet.
	for preparation := 0; ; preparation++ {
		if preparation > 2 {
			return changes, errors.New("scheduler requested unprepared complete values")
		}
		// Record loading, scope locks and complete exports may cross a deadline.
		snapshot.Run, err = store.LockExecutionRun(ctx, tx, runID)
		if err != nil {
			return changes, err
		}
		changes, err = scheduler.Step(plan, snapshot, bound)
		schedulerCount(ctx, "steps")
		if err != nil {
			return changes, err
		}
		if len(changes.ForeachCollections) == 0 && !changes.GraphExports {
			run = snapshot.Run
			break
		}
		if len(changes.ForeachCollections) > 0 {
			nodeByID := make(map[string]execution.NodeRecord, len(nodes))
			controlByID := make(map[string]execution.ControlRecord, len(controls))
			for _, node := range nodes {
				nodeByID[node.ID] = node
			}
			for _, control := range controls {
				controlByID[control.InstanceID] = control
			}
			if snapshot.ForeachOutputs == nil {
				snapshot.ForeachOutputs = map[string]contract.Values{}
			}
			for _, id := range changes.ForeachCollections {
				node := nodeByID[id]
				if node.Request == nil || node.Request.Node.Foreach == nil {
					return changes, errors.New("foreach node has no frozen collection definition")
				}
				outputs, err := store.ReadForeachCollectedOutputs(ctx, tx, controlByID[id], node.Request.Node.Foreach.Body)
				if err != nil {
					return changes, err
				}
				snapshot.ForeachOutputs[id] = outputs
			}
		}
		if changes.GraphExports {
			dependencies, err := contract.PortDependencies(definition.Outputs)
			if err != nil {
				return changes, err
			}
			exports, err := store.ReadExecutionDependencyValues(ctx, tx, runID, graph.ID, dependencies)
			if err != nil {
				return changes, err
			}
			bindings := make([]contract.Binding, 0, len(definition.Outputs))
			for _, port := range definition.Outputs {
				if port.Bind != nil {
					bindings = append(bindings, *port.Bind)
				}
			}
			names, err := inputReferenceNames(bindings, "inputs")
			if err != nil {
				return changes, err
			}
			inputValues, err := store.ReadExecutionGraphInputValues(ctx, tx, runID, graph.ID, names)
			if err != nil {
				return changes, err
			}
			if snapshot.Graph.Inputs == nil {
				snapshot.Graph.Inputs = contract.Values{}
			}
			for name, value := range inputValues {
				snapshot.Graph.Inputs[name] = value
			}
			snapshot.Nodes = append(exports, snapshot.Nodes...)
			snapshot.GraphExportsLoaded = true
		}
	}
	if changes.ReserveInstances > 0 {
		for _, scope := range scopes {
			tag, err := db.New(tx).ReserveNodeInstances(ctx, db.ReserveNodeInstancesParams{RunID: runID, ID: scope.ID, MaterializedInstances: changes.ReserveInstances})
			if err != nil {
				return changes, err
			}
			if tag.RowsAffected() != 1 {
				return changes, store.ErrConflict
			}
		}
	}
	if changes.StartRun {
		if err := r.Store.ProjectExecution(ctx, tx, execution.Projection{RunID: runID, Kind: "run", Status: "running", Time: run.Now}); err != nil {
			return changes, err
		}
	}
	if changes.Graph != nil {
		if err := store.SaveExecutionGraph(ctx, tx, *changes.Graph); err != nil {
			return changes, err
		}
	}
	for i, node := range changes.Nodes {
		if node.InputsOmitted {
			original, err := store.ReadExecutionNode(ctx, tx, runID, node.ID)
			if err != nil {
				return changes, err
			}
			if original.Request == nil || original.Revision != node.Revision-1 {
				return changes, store.ErrConflict
			}
			node.Inputs, node.Request, node.InputsOmitted = original.Inputs, original.Request, false
			changes.Nodes[i] = node
		}
		if err := store.SaveExecutionNode(ctx, tx, node, definition.Nodes[node.NodeID]); err != nil {
			return changes, err
		}
		if !node.Deadline.IsZero() {
			if _, err := db.New(tx).InsertNodeDeadline(ctx, db.InsertNodeDeadlineParams{RunID: runID, ID: "node/" + node.ID, InstanceID: new(node.ID), DueAt: node.Deadline}); err != nil {
				return changes, err
			}
		}
		projection := execution.Projection{RunID: runID, Kind: "node", InstanceID: node.ID, NodeID: node.NodeID, Pipeline: graph.Pipeline,
			ParentInstanceID: graph.ParentInstanceID, IterationIndex: graph.IterationIndex, GraphPath: graph.GraphPath + "/nodes/" + node.NodeID,
			NodeType: definition.Nodes[node.NodeID].Type, Status: node.State, Attempt: node.AttemptNumber,
			Inputs: node.Inputs, Outputs: node.Outputs, Failure: node.Failure, StartedAt: node.StartedAt, FinishedAt: node.FinishedAt, Reason: node.Reason, Time: run.Now}
		if err := r.Store.ProjectExecution(ctx, tx, projection); err != nil {
			return changes, err
		}
	}
	for _, scope := range changes.NewScopes {
		if err := store.InsertExecutionScope(ctx, tx, scope); err != nil {
			return changes, err
		}
	}
	for _, child := range changes.Children {
		if err := store.InsertExecutionGraph(ctx, tx, child); err != nil {
			return changes, err
		}
	}
	for _, control := range changes.Controls {
		if err := store.SaveExecutionControl(ctx, tx, control); err != nil {
			return changes, err
		}
	}
	for _, request := range changes.Requests {
		if err := r.Store.SaveExecutionRequest(ctx, tx, request); err != nil {
			return changes, err
		}
	}
	for _, timer := range changes.Timers {
		if _, err := db.New(tx).InsertExecutionTimer(ctx, db.InsertExecutionTimerParams{
			RunID:      timer.RunID,
			ID:         timer.ID,
			Generation: timer.Generation,
			InstanceID: new(timer.InstanceID),
			Kind:       timer.Kind,
			DueAt:      timer.DueAt,
		}); err != nil {
			return changes, err
		}
	}
	if changes.Pause {
		if _, err := db.New(tx).PauseRunAdmission(ctx, runID); err != nil {
			return changes, err
		}
		if !run.Paused {
			if err := r.Store.ProjectExecution(ctx, tx, execution.Projection{RunID: runID, Kind: "run", Status: "waiting_resolution", Failure: changes.PauseCause, Time: run.Now}); err != nil {
				return changes, err
			}
		}
	}
	if changes.Resume {
		if _, err := db.New(tx).ResumeRunAdmission(ctx, runID); err != nil {
			return changes, err
		}
		if err := r.Store.ProjectExecution(ctx, tx, execution.Projection{RunID: runID, Kind: "run", Status: "running", Time: run.Now}); err != nil {
			return changes, err
		}
	}
	if changes.StopCause != nil && run.StopCause == nil {
		b, err := json.Marshal(changes.StopCause)
		if err != nil {
			return changes, err
		}
		if _, err := db.New(tx).SetRunStopCause(ctx, db.SetRunStopCauseParams{ID: runID, StopCause: b}); err != nil {
			return changes, err
		}
	}
	for _, attempt := range changes.Attempts {
		if err := store.InsertExecutionAttempt(ctx, tx, attempt); err != nil {
			return changes, err
		}
		if err := r.Client.InsertExecute(ctx, tx, ExecuteArgs{Attempt: attempt.AttemptID, DispatchGeneration: attempt.DispatchGeneration, RoutingVersion: RoutingVersion}); err != nil {
			return changes, err
		}
		if _, err := db.New(tx).ClearAttemptDispatch(ctx, db.ClearAttemptDispatchParams{RunID: attempt.RunID, InstanceID: attempt.InstanceID, Number: attempt.Number}); err != nil {
			return changes, err
		}
	}
	// A capacity miss keeps the unstarted business attempt and changes only its
	// delivery generation. Completed River jobs cannot suppress this redispatch.
	for _, attempt := range attempts {
		if attempt.State != "ready" || !attempt.DispatchPending || (run.Paused && !changes.Resume) || changes.Pause || changes.StopCause != nil {
			continue
		}
		if changes.Transitions >= bound {
			changes.More = true
			break
		}
		changes.Transitions++
		generation := attempt.DispatchGeneration + 1
		if _, err := db.New(tx).SetAttemptDispatchGeneration(ctx, db.SetAttemptDispatchGenerationParams{
			RunID:              attempt.RunID,
			InstanceID:         attempt.InstanceID,
			Number:             attempt.Number,
			DispatchGeneration: generation,
		}); err != nil {
			return changes, err
		}
		if err := r.Client.InsertExecute(ctx, tx, ExecuteArgs{Attempt: attempt.AttemptID, DispatchGeneration: generation, RoutingVersion: RoutingVersion}); err != nil {
			return changes, err
		}
	}
	return changes, nil
}

func (r *Runtime) execute(ctx context.Context, job *river.Job[ExecuteArgs]) error {
	var claim *execution.Claim
	claimCtx, stopClaim := context.WithTimeout(ctx, 5*time.Second)
	err := pgx.BeginFunc(claimCtx, r.Store.Pool, func(tx pgx.Tx) error {
		var err error
		claim, err = r.Store.ClaimAttempt(claimCtx, tx, job.Args.Attempt, job.Args.DispatchGeneration, r.WorkerID)
		if err != nil {
			return err
		}
		if claim == nil {
			_, err = river.JobCompleteTx[*riverpgxv5.Driver](claimCtx, tx, job)
		}
		return err
	})
	stopClaim()
	if err != nil || claim == nil {
		return err
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.OwnershipAttributes(claim.Ownership)...)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("knotra.actor.id", r.WorkerID))
	work, cancel := context.WithCancelCause(ctx)
	leaseDone := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(leaseDone)
		ticker := time.NewTicker(execution.WorkerHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-finished:
				return
			case <-work.Done():
				return
			case <-ticker.C:
			}
			renewCtx, stop := context.WithTimeout(work, execution.WorkerHeartbeat)
			err := pgx.BeginFunc(renewCtx, r.Store.Pool, func(tx pgx.Tx) error {
				_, err := r.Store.RenewAttempt(renewCtx, tx, claim.Ownership)
				return err
			})
			stop()
			if err != nil {
				cancel(err)
				return
			}
		}
	}()
	result := r.Host.Execute(work, claim.Request)
	close(finished)
	<-leaseDone
	cancel(context.Canceled)
	outcome := execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: claim.Ownership, PlanID: claim.PlanID,
		CompletedAt: time.Now().UTC(), Outputs: result.Outputs, Failure: result.Failure, Artifacts: outcomeArtifacts(result.Outputs)}
	key, err := r.Outcomes.Put(outcome)
	if err != nil {
		return err
	}
	// Publication has its own bounded context. A cancelled execution context
	// must not discard the fsynced envelope or prevent a finalization handoff.
	publication, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	err = r.publish(publication, outcome, false, func(tx pgx.Tx) error {
		_, err := river.JobCompleteTx[*riverpgxv5.Driver](publication, tx, job)
		return err
	})
	if err == nil {
		return nil
	}
	handoff, stopHandoff := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopHandoff()
	queueErr := pgx.BeginFunc(handoff, r.Store.Pool, func(tx pgx.Tx) error {
		return r.Client.InsertFinalize(handoff, tx, FinalizeArgs{Attempt: claim.AttemptID, OutcomeKey: key, RoutingVersion: RoutingVersion})
	})
	return errors.Join(err, queueErr)
}

func (r *Runtime) finalize(ctx context.Context, job *river.Job[FinalizeArgs]) error {
	outcome, err := r.Outcomes.Get(job.Args.OutcomeKey)
	if err != nil {
		return err
	}
	if outcome.Ownership.AttemptID != job.Args.Attempt {
		return river.JobCancel(store.ErrConflict)
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.OwnershipAttributes(outcome.Ownership)...)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("knotra.actor.id", r.WorkerID))
	return r.publish(ctx, outcome, true, func(tx pgx.Tx) error {
		_, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job)
		return err
	})
}

func (r *Runtime) publish(ctx context.Context, outcome execution.Outcome, recovery bool, complete func(pgx.Tx) error) (err error) {
	attrs := telemetry.OwnershipAttributes(outcome.Ownership)
	attrs = append(attrs, attribute.String("knotra.engine.id", r.Store.EngineID), attribute.String("knotra.host.id", r.HostID),
		attribute.String("knotra.actor.id", r.WorkerID), attribute.Bool("recovery", recovery))
	ctx, span := otel.Tracer("knotra/scheduler").Start(ctx, "outcome.publish", trace.WithAttributes(attrs...))
	defer span.End()
	defer func() {
		span.SetAttributes(attribute.Bool("failed", err != nil))
		telemetry.Event(ctx, telemetry.OwnershipLogger(r.Host.Log, outcome.Ownership), "outcome.publication", err)
		if err != nil {
			schedulerCount(ctx, "publication.failures")
		}
	}()
	if err := r.verifyArtifacts(ctx, outcome); err != nil {
		return err
	}
	plan, err := r.Store.Plan(ctx, outcome.Ownership.RunID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		var eligible bool
		var err error
		if recovery {
			eligible, err = store.RecoverOutcome(ctx, tx, outcome)
		} else {
			eligible, err = r.Store.StageOutcome(ctx, tx, outcome)
		}
		if err != nil {
			return err
		}
		if eligible {
			if err := r.advanceTx(ctx, tx, plan, outcome.Ownership.RunID, outcome.Ownership.InstanceID); err != nil {
				return err
			}
		}
		if _, err := store.ReleaseAttemptSlots(ctx, tx, outcome.Ownership, r.Wake); err != nil {
			return err
		}
		return complete(tx)
	})
}

func outcomeArtifacts(outputs contract.Values) []execution.ArtifactReference {
	var artifacts []execution.ArtifactReference
	seen := map[string]bool{}
	keys := make([]string, 0, len(outputs))
	for key := range outputs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		for _, artifact := range outputs[key].Artifacts {
			if !seen[artifact.ID] {
				seen[artifact.ID] = true
				artifacts = append(artifacts, execution.ArtifactReference{ID: artifact.ID, SHA256: artifact.SHA256, Size: artifact.Size})
			}
		}
	}
	return artifacts
}

func (r *Runtime) verifyArtifacts(ctx context.Context, outcome execution.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	refs := make(map[string]execution.ArtifactReference, len(outcome.Artifacts))
	for _, ref := range outcome.Artifacts {
		data, err := r.Host.Artifacts.Get(ctx, ref.ID)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if ref.Size != int64(len(data)) || ref.SHA256 != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("outcome artifact %s does not match immutable bytes", ref.ID)
		}
		refs[ref.ID] = ref
	}
	for _, value := range outcome.Outputs {
		for _, artifact := range value.Artifacts {
			ref, found := refs[artifact.ID]
			if !found || ref.SHA256 != artifact.SHA256 || ref.Size != artifact.Size {
				return errors.New("output artifact is not covered by verified outcome references")
			}
		}
	}
	return nil
}

func (r *Runtime) pumpTimers(ctx context.Context) error {
	rows, err := db.New(r.Store.Pool).ListDueTimerRuns(ctx)
	if err != nil {
		return err
	}
	ids := rows
	for _, id := range ids {
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			run, err := store.LockExecutionRun(ctx, tx, id)
			if err != nil {
				return err
			}
			tag, err := db.New(tx).ConsumeDueTimers(ctx, db.ConsumeDueTimersParams{RunID: id, ConsumedAt: new(run.Now)})
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 || protocol.Terminal(run.Status) {
				return nil
			}
			_, err = store.WakeExecution(ctx, tx, id, r.Wake)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) reconcileOutcomes(ctx context.Context) error {
	rows, err := db.New(r.Store.Pool).ListExpiredOutcomeAttempts(ctx, new(r.outcomeCursor))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		r.outcomeCursor = ""
		return nil
	}
	for _, record := range rows {
		if record.OutcomeKey == nil || record.Owner == nil {
			return errors.New("claimed attempt is missing ownership/outcome identity")
		}
		r.outcomeCursor = *record.OutcomeKey
		token := execution.Ownership{AttemptID: execution.AttemptID{RunID: record.RunID, InstanceID: record.InstanceID, Number: record.Number}, WorkerID: *record.Owner, Generation: record.OwnershipGeneration}
		if _, err := r.Outcomes.Get(*record.OutcomeKey); err != nil {
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, execution.ErrInvalidOutcome) {
				return err
			}
			// ponytail: missing fenced envelopes stay in the bounded scan until
			// retention; storage notifications can replace polling at high volume.
			if record.State != "claimed" {
				continue
			}
			plan, err := r.Store.Plan(ctx, token.RunID)
			if err != nil {
				return err
			}
			if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
				staged, err := r.Store.StageLostAttempt(ctx, tx, token)
				if err != nil || !staged {
					return err
				}
				if err := r.advanceTx(ctx, tx, plan, token.RunID, token.InstanceID); err != nil {
					return err
				}
				_, err = store.ReleaseAttemptSlots(ctx, tx, token, r.Wake)
				return err
			}); err != nil {
				return err
			}
			continue
		}
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			return r.Client.InsertFinalize(ctx, tx, FinalizeArgs{Attempt: token.AttemptID, OutcomeKey: *record.OutcomeKey, RoutingVersion: RoutingVersion})
		}); err != nil {
			return err
		}
	}
	return nil
}

// reconcileDeliveries repairs infrastructure identities, never business attempts.
func (r *Runtime) reconcileDeliveries(ctx context.Context) error {
	runs, err := db.New(r.Store.Pool).ListDeliveryRepairRuns(ctx, r.deliveryCursor)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		r.deliveryCursor = ""
		return nil
	}
	for _, id := range runs {
		r.deliveryCursor = id
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			run, err := store.LockExecutionRun(ctx, tx, id)
			if err != nil {
				return err
			}
			if run.Backend != execution.BackendRiver || run.SchedulerVersion != execution.SchedulerVersion || run.StateFormatVersion != execution.StateFormatVersion {
				return store.ErrExecutionVersion
			}
			if protocol.Terminal(run.Status) {
				return nil
			}
			if run.WakeGeneration > run.AppliedGeneration {
				viable, err := r.Client.hasPendingAdvance(ctx, tx, run)
				if err != nil {
					return err
				}
				if !viable {
					_, err = store.WakeExecution(ctx, tx, id, r.Wake)
				}
				return err
			}
			if run.Cancelled || run.StopCause != nil || run.Paused || !run.Deadline.After(run.Now) {
				return nil //nolint:nilerr // These states deliberately prohibit redispatch; no delivery repair failed.
			}
			ready, err := db.New(tx).ReadReadyDeliveries(ctx, db.ReadReadyDeliveriesParams{RunID: id, Cursor: run.DeliveryCursor, Now: run.Now})
			if err != nil {
				return err
			}
			repaired, cursor := false, run.DeliveryCursor
			for _, attempt := range ready {
				cursor = attempt.InstanceID
				args := ExecuteArgs{Attempt: execution.AttemptID{RunID: id, InstanceID: attempt.InstanceID, Number: attempt.Number}, DispatchGeneration: attempt.DispatchGeneration, RoutingVersion: RoutingVersion}
				viable, err := r.Client.hasDelivery(ctx, tx, args, run.Now)
				if err != nil {
					return err
				}
				if viable {
					continue
				}
				if _, err := db.New(tx).MarkAttemptForDispatch(ctx, db.MarkAttemptForDispatchParams{RunID: id, InstanceID: attempt.InstanceID, Number: attempt.Number}); err != nil {
					return err
				}
				repaired = true
			}
			if len(ready) < stepTransitions {
				cursor = ""
			}
			if cursor != run.DeliveryCursor {
				if _, err := db.New(tx).SetDeliveryCursor(ctx, db.SetDeliveryCursorParams{ID: id, DeliveryCursor: cursor}); err != nil {
					return err
				}
			}
			if repaired {
				_, err = store.WakeExecution(ctx, tx, id, r.Wake)
			}
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// Static references let complete input values remain durable without returning
// unrelated arrays on every control transition. Dynamic names are rejected by
// the contract compiler's shared checked-expression analysis.
func inputReferenceNames(bindings []contract.Binding, namespace string) ([]string, error) {
	names := map[string]bool{}
	for _, binding := range bindings {
		refs, err := contract.BindingReferences(binding)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if strings.HasPrefix(ref, namespace+".") {
				names[strings.Split(ref, ".")[1]] = true
			}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	slices.Sort(result)
	return result, nil
}
