package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/michael-bill/knotra/internal/contract"
)

type counters struct{ nodes, active int }

type runtime struct {
	in                  RunInput
	cancel              workflow.CancelFunc
	state               Snapshot
	sequence            int64
	budgets             map[string]*counters
	humans              map[string][]HumanSignal
	resolutions         map[string][]ResolutionSignal
	paused              map[string]bool
	publishing          map[string]bool
	failure             *Failure
	projected           map[string]bool
	loops               map[string]LoopCheckpoint
	foreachResults      map[string]map[int]contract.Values
	checkpointRequested bool
	checkpointing       bool
	executingLeaves     int
	waiters             int
	storageActive       int
	historyReserved     int
}

type graphContext struct {
	pipeline    string
	document    *contract.Pipeline
	path        string
	scopes      []BudgetScope
	permissions *contract.Permissions
	deadline    time.Time
}

// Workflow runs a compiled, immutable plan. Register it under WorkflowName.
// All workflow collections are traversed in lexical order; external activity
// completions and signals are ordered by Temporal history.
func Workflow(ctx workflow.Context, input RunInput) (RunResult, error) {
	loadPlan := input.Plan.Version == ""
	if loadPlan {
		if err := workflow.ExecuteActivity(
			storageSummary(ctx, "Load admitted pipeline plan"),
			PlanActivity,
			PlanRequest{RunID: input.RunID},
		).Get(
			ctxWithoutCancel(ctx),
			&input.Plan,
		); err != nil {
			return RunResult{}, err
		}
	}
	if input.Plan.Version != "knotra/v1" || input.Plan.CompilerVersion != contract.CompilerVersion || input.Plan.CELVersion != contract.CELVersion {
		return rejectPlan(
			ctx,
			input,
			failure("PLAN_VERSION_UNSUPPORTED", "admitted plan requires a different contract/compiler/CEL version"),
		)
	}
	ctx, cancel := workflow.WithCancel(ctx)
	defer cancel()
	r := &runtime{in: input, cancel: cancel,
		state: Snapshot{
			RunID:    input.RunID,
			Status:   "running",
			Nodes:    map[string]*NodeSnapshot{},
			Requests: map[string]*Request{},
		},
		budgets: map[string]*counters{}, humans: map[string][]HumanSignal{}, resolutions: map[string][]ResolutionSignal{}, paused: map[string]bool{}, publishing: map[string]bool{}, projected: map[string]bool{}, loops: map[string]LoopCheckpoint{}, foreachResults: map[string]map[int]contract.Values{}}
	r.restore(input.Checkpoint)
	if err := workflow.SetQueryHandler(ctx, SnapshotQuery, func() (Snapshot, error) { return r.state, nil }); err != nil {
		return RunResult{}, err
	}
	document := input.Plan.Pipelines[input.Plan.Root]
	if document == nil {
		return rejectPlan(ctx, input, failure("PLAN_INVALID", "root pipeline missing from admitted plan"))
	}
	limits := restrictLimits(input.Plan.Profile.Spec.Limits, document.Spec.Limits)
	duration, err := contract.Duration(limits.Timeout)
	if err != nil || duration <= 0 {
		return rejectPlan(ctx, input, failure("PLAN_INVALID", "invalid root timeout"))
	}
	acceptedAt := input.AcceptedAt
	if acceptedAt.IsZero() {
		acceptedAt = workflow.Now(ctx)
	}
	deadline := acceptedAt.Add(duration)
	duration = deadline.Sub(workflow.Now(ctx))
	if input.Checkpoint != nil {
		deadline = input.Checkpoint.Deadline
		duration = deadline.Sub(workflow.Now(ctx))
	}
	scope := BudgetScope{ID: input.RunID, Limits: limits}
	if r.budgets[scope.ID] == nil {
		r.budgets[scope.ID] = &counters{}
	}
	r.listen(ctx)
	r.watchCheckpoint(ctx)
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	workflow.Go(timerCtx, func(tctx workflow.Context) {
		if timer(tctx, duration, "Pipeline run deadline") == nil {
			r.stop(failure("DEADLINE_EXCEEDED", "run deadline exceeded"))
		}
	})
	if err := r.emit(ctx, Projection{Kind: "run", Status: "running"}); err != nil {
		return RunResult{}, err
	}
	if !deadline.After(workflow.Now(ctx)) {
		r.stop(failure("DEADLINE_EXCEEDED", "run deadline expired before worker admission"))
	}
	gc := graphContext{
		pipeline: input.Plan.Root,
		document: document,
		path:     input.RunID + "/root",
		scopes:   []BudgetScope{scope},
		deadline: deadline,
	}
	outputs, runErr := r.graph(ctx, gc, document.Spec.Graph, input.Inputs)
	if r.checkpointing && r.failure == nil {
		input.Checkpoint = r.checkpoint(ctxWithoutCancel(ctx), deadline)
		if r.failure == nil {
			if loadPlan {
				input.Plan = contract.Plan{}
			}
			return RunResult{}, workflow.NewContinueAsNewError(ctxWithoutCancel(ctx), WorkflowName, input)
		}
	}
	result := RunResult{Status: "succeeded", Outputs: outputs}
	if r.failure != nil {
		runErr = r.failure
	}
	if runErr != nil {
		f := asFailure(runErr)
		if errors.Is(runErr, workflow.ErrCanceled) || f.Code == "CANCELLED" {
			result.Status = "cancelled"
		} else {
			result.Status = "failed"
		}
		result.Outputs, result.Failure = nil, f
	}
	r.state.Status, r.state.Outputs, r.state.Failure = result.Status, result.Outputs, result.Failure
	if err := r.emit(ctx, Projection{Kind: "run", Status: result.Status, Outputs: result.Outputs, Failure: result.Failure}); err != nil {
		return RunResult{}, err
	}
	return result, nil
}

func rejectPlan(ctx workflow.Context, input RunInput, reason *Failure) (RunResult, error) {
	sequence := int64(1)
	if input.Checkpoint != nil {
		sequence = input.Checkpoint.Sequence + 1
	}
	result := RunResult{Status: "failed", Failure: reason}
	event := Projection{
		RunID:    input.RunID,
		Sequence: sequence,
		Time:     workflow.Now(ctx),
		Kind:     "run",
		Status:   result.Status,
		Failure:  reason,
	}
	if err := workflow.ExecuteActivity(storageSummary(ctx, "Persist incompatible plan failure"), ProjectActivity, event).Get(
		ctxWithoutCancel(ctx),
		nil,
	); err != nil {
		return RunResult{}, err
	}
	return result, nil
}

func (r *runtime) stop(f *Failure) {
	if r.failure == nil {
		r.failure = f
		r.state.Failure = f
		r.cancel()
	}
}

func (r *runtime) listen(ctx workflow.Context) {
	workflow.Go(ctx, func(ctx workflow.Context) {
		human := workflow.GetSignalChannel(ctx, HumanSignalName)
		resolve := workflow.GetSignalChannel(ctx, ResolveSignalName)
		cancel := workflow.GetSignalChannel(ctx, CancelSignalName)

		for ctx.Err() == nil {
			sel := workflow.NewSelector(ctx)
			sel.AddReceive(human, func(c workflow.ReceiveChannel, _ bool) {
				var v HumanSignal
				c.Receive(ctx, &v)
				if v.RequestID != "" {
					r.humans[v.RequestID] = append(r.humans[v.RequestID], v)
				}
			})
			sel.AddReceive(resolve, func(c workflow.ReceiveChannel, _ bool) {
				var v ResolutionSignal
				c.Receive(ctx, &v)
				if v.InstanceID != "" {
					r.resolutions[v.InstanceID] = append(r.resolutions[v.InstanceID], v)
				}
			})
			sel.AddReceive(cancel, func(c workflow.ReceiveChannel, _ bool) {
				var v CancelSignal
				c.Receive(ctx, &v)
				reason := v.Reason
				if reason == "" {
					reason = "run cancelled"
				}
				r.stop(failure("CANCELLED", reason))
			})
			sel.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
			sel.Select(ctx)
		}
	})
}

// Storage projection is independent of execution cancellation: once a state
// transition is chosen, it must be made durable even while stopping the run.
// There is no overall retry deadline: a database outage must not erase an
// external outcome after the operation itself has already finished.
func storageContext(ctx workflow.Context) workflow.Context {
	ctx, _ = workflow.NewDisconnectedContext(ctx)
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 10 * time.Second},
	})
}

func storageSummary(ctx workflow.Context, summary string) workflow.Context {
	ctx = storageContext(ctx)
	options := workflow.GetActivityOptions(ctx)
	options.Summary = summary
	return workflow.WithActivityOptions(ctx, options)
}

func timer(ctx workflow.Context, duration time.Duration, summary string) error {
	return workflow.NewTimerWithOptions(ctx, duration, workflow.TimerOptions{Summary: summary}).Get(ctx, nil)
}

func (r *runtime) emit(ctx workflow.Context, event Projection) error {
	deferable := event.Kind == "node" && (event.Status == "pending" || event.Status == "ready" || (event.Status == "running" && event.Attempt == 0))
	if err := r.beginStorage(ctx, deferable); err != nil {
		return err
	}
	defer func() { r.storageActive--; r.considerCheckpoint(ctx) }()
	r.sequence++
	event.RunID, event.Sequence, event.Time = r.in.RunID, r.sequence, workflow.Now(ctx)
	summary := "Persist run status: " + event.Status
	if event.Kind == "node" {
		summary = "Persist node " + event.NodeID + ": " + event.Status
	} else if event.Kind == "request" {
		summary = "Persist response: " + event.Status
	}
	return workflow.ExecuteActivity(storageSummary(ctx, summary), ProjectActivity, event).Get(ctxWithoutCancel(ctx), nil)
}

func ctxWithoutCancel(ctx workflow.Context) workflow.Context {
	ctx, _ = workflow.NewDisconnectedContext(ctx)
	return ctx
}

func (r *runtime) request(ctx workflow.Context, request Request) error {
	if err := r.beginStorage(ctx, false); err != nil {
		return err
	}
	defer func() { r.storageActive--; r.considerCheckpoint(ctx) }()
	request.RunID = r.in.RunID
	copy := request
	r.state.Requests[request.ID] = &copy
	return workflow.ExecuteActivity(
		storageSummary(ctx, "Persist "+request.Kind+" request: "+request.Status),
		RequestActivity,
		request,
	).Get(
		ctxWithoutCancel(ctx),
		nil,
	)
}

func failure(code, message string) *Failure { return &Failure{Code: code, Message: message} }

func asFailure(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	if temporal.IsCanceledError(err) || errors.Is(err, workflow.ErrCanceled) {
		return failure("CANCELLED", "execution cancelled")
	}
	return failure("EXECUTION_FAILED", err.Error())
}

func keys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))

	for key := range values {
		result = append(result, key)
	}

	sort.Strings(result)
	return result
}

func stableID(kind, address string) string {
	digest := sha256.Sum256([]byte(address))
	return kind + "_" + hex.EncodeToString(digest[:])
}

func minPositive(a, b int) int {
	if a == 0 {
		return b
	}
	if b == 0 || a < b {
		return a
	}
	return b
}

func restrictLimits(parent, own contract.Limits) contract.Limits {
	result := contract.Limits{Timeout: parent.Timeout,
		MaxConcurrentNodes: minPositive(parent.MaxConcurrentNodes, own.MaxConcurrentNodes),
		MaxNodeInstances:   minPositive(parent.MaxNodeInstances, own.MaxNodeInstances),
		MaxModelCalls:      minPositive(parent.MaxModelCalls, own.MaxModelCalls),
		MaxToolCalls:       minPositive(parent.MaxToolCalls, own.MaxToolCalls)}
	if own.Timeout != "" {
		a, ea := contract.Duration(parent.Timeout)
		b, eb := contract.Duration(own.Timeout)
		if eb == nil && (ea != nil || b < a) {
			result.Timeout = own.Timeout
		}
	}
	return result
}

func (r *runtime) admit(ctx workflow.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.failure != nil {
		return r.failure
	}
	if err := r.boundary(ctx); err != nil {
		return err
	}
	if err := workflow.Await(ctx, func() bool { return len(r.paused) == 0 || r.failure != nil }); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.failure != nil {
		return r.failure
	}
	return nil
}

func (r *runtime) materialize(ctx workflow.Context, scopes []BudgetScope, count int) error {
	if err := r.admit(ctx); err != nil {
		return err
	}

	for _, scope := range scopes {
		c := r.budgets[scope.ID]
		if c == nil {
			c = &counters{}
			r.budgets[scope.ID] = c
		}
		if scope.Limits.MaxNodeInstances > 0 && count > scope.Limits.MaxNodeInstances-c.nodes {
			return failure("LIMIT_EXCEEDED", fmt.Sprintf("node instance budget exhausted in %s", scope.ID))
		}
	}

	for _, scope := range scopes {
		r.budgets[scope.ID].nodes += count
	}

	return nil
}

func (r *runtime) acquire(ctx workflow.Context, scopes []BudgetScope) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.failure != nil {
		return r.failure
	}
	if err := workflow.Await(ctx, func() bool {
		if len(r.paused) > 0 {
			return false
		}
		if r.budgets[scopes[0].ID].active >= maxActiveLeafAttempts {
			return false
		}

		for _, scope := range scopes {
			if scope.Limits.MaxConcurrentNodes > 0 && r.budgets[scope.ID].active >= scope.Limits.MaxConcurrentNodes {
				return false
			}
		}

		return true
	}); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.failure != nil {
		return r.failure
	}

	for _, scope := range scopes {
		r.budgets[scope.ID].active++
	}

	return nil
}

func (r *runtime) release(scopes []BudgetScope) {
	for _, scope := range scopes {
		r.budgets[scope.ID].active--
	}
}
