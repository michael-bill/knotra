package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/michael-bill/knotra/internal/contract"
)

func (r *runtime) leaf(ctx workflow.Context, gc graphContext, state *NodeSnapshot, node contract.Node, args contract.Values) (contract.Values, error) {
	reservation, err := r.beginLeaf(ctx, node.Execution.Retry.MaxAttempts)
	if err != nil {
		return nil, err
	}
	defer func() { r.executingLeaves--; r.historyReserved -= reservation }()
	var arguments json.RawMessage
	if node.Type == "tool" {
		if node.Tool == nil {
			return nil, failure("PLAN_INVALID", "missing tool configuration")
		}
		value, present, err := contract.EvalBinding(node.Tool.Arguments, contract.Scope{Args: args})
		if err != nil {
			return nil, failure("INPUT_INVALID", err.Error())
		}
		if !present || value.Artifacts != nil || value.Collection {
			return nil, failure("INPUT_INVALID", "tool arguments must be a JSON object")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(value.JSON, &object); err != nil || object == nil {
			return nil, failure("INPUT_INVALID", "tool arguments must be a JSON object")
		}
		arguments = value.JSON
	}

	for attempt := 1; attempt <= node.Execution.Retry.MaxAttempts; attempt++ {
		if err := r.acquire(ctx, gc.scopes); err != nil {
			return nil, err
		}
		state.Attempt = attempt
		if err := r.transition(ctx, state, "running", nil, nil, ""); err != nil {
			r.release(gc.scopes)
			return nil, err
		}
		remaining := gc.deadline.Sub(workflow.Now(ctx))
		if remaining <= 0 {
			r.release(gc.scopes)
			return nil, failure("DEADLINE_EXCEEDED", "node deadline exceeded")
		}
		activityCtx := workflow.WithActivityOptions(
			ctx,
			workflow.ActivityOptions{
				StartToCloseTimeout: remaining,
				HeartbeatTimeout:    20 * time.Second,
				WaitForCancellation: true,
				RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
				Summary:             fmt.Sprintf("%s: %s (attempt %d)", node.Type, state.NodeID, attempt),
			},
		)
		request := ExecuteRequest{
			RunID:         r.in.RunID,
			InstanceID:    state.ID,
			NodeID:        state.NodeID,
			Pipeline:      gc.pipeline,
			Attempt:       attempt,
			Node:          node,
			Inputs:        args,
			ToolArguments: arguments,
			Scopes:        gc.scopes,
			Permissions:   gc.permissions,
			Deadline:      gc.deadline,
		}
		var result ExecuteResult
		err := workflow.ExecuteActivity(activityCtx, ExecuteActivity, request).Get(ctx, &result)
		r.release(gc.scopes)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			// A worker can disappear after an external system accepted a request.
			// Retrying the activity wholesale would replay those side effects.
			result.Failure = &Failure{
				Code:                  "OUTCOME_UNKNOWN",
				Message:               err.Error(),
				Unknown:               true,
				OperationID:           fmt.Sprintf("%s/attempt/%d", state.ID, attempt),
				CanRetryIfNotExecuted: node.Type != "agent",
			}
		}
		if result.Failure == nil {
			return result.Outputs, nil
		}
		f := result.Failure
		if f.Unknown {
			if node.Execution.OnUnknownOutcome == "fail" {
				return nil, f
			}
			var completed bool
			result, completed, err = r.resolve(ctx, gc, state, node, f)
			if err != nil {
				return nil, err
			}
			if completed {
				return result.Outputs, nil
			}
			f = result.Failure
		}
		if !f.Retryable || attempt == node.Execution.Retry.MaxAttempts {
			return nil, f
		}
		if err := r.transition(ctx, state, "retry_wait", nil, f, "safe retry permitted"); err != nil {
			return nil, err
		}
		backoff, err := contract.Duration(node.Execution.Retry.Backoff)
		if err != nil {
			return nil, failure("PLAN_INVALID", "invalid retry backoff")
		}
		if err := timer(ctx, backoff, "Retry backoff: "+state.NodeID); err != nil {
			return nil, err
		}
	}

	return nil, failure("PLAN_INVALID", "no node attempt permitted")
}

func (r *runtime) human(ctx workflow.Context, gc graphContext, state *NodeSnapshot, node contract.Node, args contract.Values) (contract.Values, error) {
	if err := r.admit(ctx); err != nil {
		return nil, err
	}
	if err := workflow.Await(ctx, func() bool { return r.waiters < maxWaitingHumans }); err != nil {
		return nil, err
	}
	if err := r.admit(ctx); err != nil {
		return nil, err
	}
	r.waiters++
	defer func() { r.waiters-- }()
	if node.Human == nil {
		return nil, failure("PLAN_INVALID", "missing human configuration")
	}
	id := stableID("h", state.ID+"/human")
	request := Request{
		ID:         id,
		InstanceID: state.ID,
		Kind:       "human",
		Status:     "open",
		Prompt:     node.Human.Prompt.Text,
		Inputs:     withoutArtifactPaths(args),
		Outputs:    node.Outputs,
		Deadline:   gc.deadline,
	}
	if err := r.request(ctx, request); err != nil {
		return nil, err
	}
	if err := r.transition(ctx, state, "waiting_human", nil, nil, ""); err != nil {
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			request.Status = "cancelled"
			if !workflow.Now(ctx).Before(gc.deadline) {
				request.Status = "expired"
			}
			_ = r.request(ctx, request)
		}
	}()
	seen := map[string]bool{}

	for {
		waitErr := workflow.Await(ctx, func() bool { return len(r.humans[id]) > 0 })
		var signal HumanSignal
		if waitErr != nil {
			status := "cancelled"
			if !workflow.Now(ctx).Before(gc.deadline) {
				status = "expired"
			}
			var saved *HumanSignal
			if err := workflow.ExecuteActivity(
				storageSummary(ctx, "Reconcile saved human response: "+state.NodeID),
				AnswerActivity,
				AnswerRequest{RunID: r.in.RunID, RequestID: id, CloseIfAbsent: status},
			).Get(
				ctxWithoutCancel(ctx),
				&saved,
			); err != nil {
				return nil, err
			}
			if saved == nil {
				return nil, waitErr
			}
			signal = *saved
		} else {
			signal = r.humans[id][0]
			r.humans[id] = r.humans[id][1:]
		}
		if signal.ResponseID == "" || seen[signal.ResponseID] {
			if waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		seen[signal.ResponseID] = true
		values, err := contract.ValidatePorts(node.Outputs, signal.Values, false)
		if err != nil {
			if err := r.emit(
				ctx,
				Projection{
					Kind:       "request",
					InstanceID: state.ID,
					Status:     "rejected",
					RequestID:  id,
					ResponseID: signal.ResponseID,
					Failure:    failure("RESPONSE_INVALID", err.Error()),
				},
			); err != nil {
				return nil, err
			}
			if waitErr != nil {
				return nil, failure("RESPONSE_INVALID", "persisted human response violates its admitted contract")
			}
			continue
		}
		if !signal.AcceptedAt.IsZero() && signal.AcceptedAt.After(gc.deadline) {
			if waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		if ctx.Err() != nil && signal.AcceptedAt.IsZero() {
			return nil, ctx.Err()
		}
		request.Status, request.ResponseID, request.Response = "accepted", signal.ResponseID, values
		if err := r.request(ctx, request); err != nil {
			return nil, err
		}
		closed = true
		if err := r.transition(ctx, state, "succeeded", values, nil, "human response accepted"); err != nil {
			return nil, err
		}
		return values, nil
	}
}

func (r *runtime) resolve(ctx workflow.Context, gc graphContext, state *NodeSnapshot, node contract.Node, f *Failure) (ExecuteResult, bool, error) {
	if f.OperationID == "" {
		f.OperationID = fmt.Sprintf("%s/attempt/%d", state.ID, state.Attempt)
	}
	id := stableID("q", state.ID+"/resolution/"+fmt.Sprint(state.Attempt))
	r.paused[state.ID] = true
	r.state.Status = "waiting_resolution"
	request := Request{
		ID:         id,
		InstanceID: state.ID,
		Kind:       "resolution",
		Status:     "open",
		Failure:    f,
		Outputs:    node.Outputs,
		Deadline:   gc.deadline,
	}
	closed := false
	defer func() {
		delete(r.paused, state.ID)
		if !closed {
			request.Status = "cancelled"
			if !workflow.Now(ctx).Before(gc.deadline) {
				request.Status = "expired"
			}
			_ = r.request(ctx, request)
		}
		if len(r.paused) == 0 && r.failure == nil {
			r.state.Status = "running"
			_ = r.emit(ctx, Projection{Kind: "run", Status: "running"})
		}
	}()
	if err := r.request(ctx, request); err != nil {
		return ExecuteResult{}, false, err
	}
	if err := r.transition(ctx, state, "waiting_resolution", nil, f, "external outcome requires evidence"); err != nil {
		return ExecuteResult{}, false, err
	}
	if err := r.emit(ctx, Projection{Kind: "run", Status: "waiting_resolution", Failure: f}); err != nil {
		return ExecuteResult{}, false, err
	}
	seen := map[string]bool{}

	for {
		waitErr := workflow.Await(ctx, func() bool { return len(r.resolutions[state.ID]) > 0 })
		var signal ResolutionSignal
		if waitErr != nil {
			status := "cancelled"
			if !workflow.Now(ctx).Before(gc.deadline) {
				status = "expired"
			}
			var saved *ResolutionSignal
			if err := workflow.ExecuteActivity(
				storageSummary(ctx, "Reconcile external outcome: "+state.NodeID),
				ResolutionActivity,
				AnswerRequest{RunID: r.in.RunID, RequestID: id, CloseIfAbsent: status},
			).Get(
				ctxWithoutCancel(ctx),
				&saved,
			); err != nil {
				return ExecuteResult{}, false, err
			}
			if saved == nil {
				return ExecuteResult{}, false, waitErr
			}
			signal = *saved
		} else {
			signal = r.resolutions[state.ID][0]
			r.resolutions[state.ID] = r.resolutions[state.ID][1:]
		}
		if signal.ResponseID == "" || seen[signal.ResponseID] {
			if waitErr != nil {
				return ExecuteResult{}, false, waitErr
			}
			continue
		}
		seen[signal.ResponseID] = true
		var invalid error
		if signal.InstanceID != state.ID || signal.OperationID != f.OperationID || strings.TrimSpace(signal.Evidence) == "" {
			invalid = fmt.Errorf("resolution must match operation and include evidence")
		}
		var values contract.Values

		switch signal.Decision {
		case "completed":
			if invalid == nil {
				values, invalid = contract.ValidatePorts(node.Outputs, signal.Outputs, false)
			}
		case "not_executed", "failed":
			if len(signal.Outputs) != 0 {
				invalid = fmt.Errorf("outputs are only accepted for completed outcomes")
			}
		default:
			invalid = fmt.Errorf("unknown resolution decision")
		}

		if invalid != nil {
			if err := r.emit(
				ctx,
				Projection{
					Kind:       "request",
					InstanceID: state.ID,
					Status:     "rejected",
					RequestID:  id,
					ResponseID: signal.ResponseID,
					Failure:    failure("RESOLUTION_INVALID", invalid.Error()),
				},
			); err != nil {
				return ExecuteResult{}, false, err
			}
			if waitErr != nil {
				return ExecuteResult{}, false, failure("RESOLUTION_INVALID", "persisted resolution violates its admitted contract")
			}
			continue
		}
		if !signal.AcceptedAt.IsZero() && signal.AcceptedAt.After(gc.deadline) {
			if waitErr != nil {
				return ExecuteResult{}, false, waitErr
			}
			continue
		}
		if ctx.Err() != nil && signal.AcceptedAt.IsZero() {
			return ExecuteResult{}, false, ctx.Err()
		}
		request.Status, request.ResponseID, request.Response, request.Evidence = "resolved", signal.ResponseID, values, signal.Evidence
		if err := r.request(ctx, request); err != nil {
			return ExecuteResult{}, false, err
		}
		closed = true

		switch signal.Decision {
		case "completed":
			if err := r.transition(ctx, state, "succeeded", values, nil, "external outcome resolved"); err != nil {
				return ExecuteResult{}, false, err
			}
			return ExecuteResult{Outputs: values}, true, nil
		case "not_executed":
			return ExecuteResult{Failure: &Failure{
				Code:      "NOT_EXECUTED",
				Message:   "operator confirmed operation did not execute",
				Retryable: f.CanRetryIfNotExecuted,
			}}, false, nil
		default:
			return ExecuteResult{}, false, failure("EXTERNAL_FAILED", "operator confirmed external operation failed")
		}
	}
}
