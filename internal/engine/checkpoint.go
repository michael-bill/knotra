package engine

import (
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/michael-bill/knotra/internal/contract"
)

// Rotate well below Temporal's hard history limit. The remainder is available
// to finish already executing leaves and to durably publish their outcomes.
const checkpointHistoryEvents = 8000

const (
	maxOutstandingStorage = 64
	maxActiveLeafAttempts = 64
	maxWaitingHumans      = 64
	maxReservedHistory    = 30000
)

func (r *runtime) beginStorage(ctx workflow.Context, deferable bool) error {
	if deferable {
		if err := r.boundary(ctx); err != nil {
			return err
		}
	}
	if err := workflow.Await(ctxWithoutCancel(ctx), func() bool { return r.storageActive < maxOutstandingStorage }); err != nil {
		return err
	}
	if deferable {
		if err := r.boundary(ctx); err != nil {
			return err
		}
	}
	r.storageActive++
	return nil
}

func (r *runtime) beginLeaf(ctx workflow.Context, attempts int) (int, error) {
	// Each attempt has bounded orchestration transitions; external agent steps
	// remain inside one activity. Reserve enough history to finish safe retries
	// after admission is stopped for a continuation.
	reserve := attempts*24 + 24
	if err := r.admit(ctx); err != nil {
		return 0, err
	}
	if err := workflow.Await(ctx, func() bool { return r.historyReserved+reserve <= maxReservedHistory }); err != nil {
		return 0, err
	}
	if err := r.admit(ctx); err != nil {
		return 0, err
	}
	r.historyReserved += reserve
	r.executingLeaves++
	return reserve, nil
}

func (r *runtime) considerCheckpoint(ctx workflow.Context) {
	info := workflow.GetInfo(ctx)
	if info.GetCurrentHistoryLength() >= checkpointHistoryEvents || info.GetCurrentHistorySize() >= 16*1024*1024 || info.GetContinueAsNewSuggested() {
		r.checkpointRequested = true
	}
}

func (r *runtime) watchCheckpoint(ctx workflow.Context) {
	workflow.Go(ctx, func(ctx workflow.Context) {
		if err := workflow.Await(ctx, func() bool {
			return r.checkpointRequested && r.executingLeaves == 0 && r.waiters == 0 && r.storageActive == 0 && len(r.paused) == 0
		}); err != nil {
			return
		}
		if r.failure == nil && ctx.Err() == nil {
			r.checkpointing = true
			r.cancel()
		}
	})
}

func (r *runtime) boundary(ctx workflow.Context) error {
	r.considerCheckpoint(ctx)
	if !r.checkpointRequested {
		return ctx.Err()
	}
	// No new effects begin while the already admitted attempts finish. Their
	// retry/backoff belongs to the same attempt lifecycle and may still finish.
	return workflow.Await(ctx, func() bool { return r.failure != nil })
}

func (r *runtime) restore(saved *Checkpoint) {
	if saved == nil {
		return
	}
	r.state, r.sequence = saved.State, saved.Sequence
	r.projected, r.loops, r.foreachResults = saved.Projected, saved.Loops, saved.Foreach
	r.humans, r.resolutions = saved.Humans, saved.Resolutions
	if r.projected == nil {
		r.projected = map[string]bool{}
	}
	if r.loops == nil {
		r.loops = map[string]LoopCheckpoint{}
	}
	if r.foreachResults == nil {
		r.foreachResults = map[string]map[int]contract.Values{}
	}
	if r.humans == nil {
		r.humans = map[string][]HumanSignal{}
	}
	if r.resolutions == nil {
		r.resolutions = map[string][]ResolutionSignal{}
	}

	for _, id := range keys(saved.NodeCounts) {
		r.budgets[id] = &counters{nodes: saved.NodeCounts[id]}
	}
}

func (r *runtime) checkpoint(ctx workflow.Context, deadline time.Time) *Checkpoint {
	// Do not lose signals delivered in the same workflow task that chose to
	// rotate history. A concurrent cancel takes precedence over continuation.
	r.drainSignals(ctx)
	counts := make(map[string]int, len(r.budgets))

	for _, id := range keys(r.budgets) {
		counts[id] = r.budgets[id].nodes
	}

	return &Checkpoint{State: r.state, Sequence: r.sequence, Deadline: deadline, NodeCounts: counts,
		Projected: r.projected, Loops: r.loops, Foreach: r.foreachResults, Humans: r.humans, Resolutions: r.resolutions}
}

func (r *runtime) drainSignals(ctx workflow.Context) {
	human, resolve, cancel := workflow.GetSignalChannel(ctx, HumanSignalName), workflow.GetSignalChannel(ctx, ResolveSignalName), workflow.GetSignalChannel(ctx, CancelSignalName)

	for {
		var signal HumanSignal
		if !human.ReceiveAsync(&signal) {
			break
		}
		r.humans[signal.RequestID] = append(r.humans[signal.RequestID], signal)
	}

	for {
		var signal ResolutionSignal
		if !resolve.ReceiveAsync(&signal) {
			break
		}
		r.resolutions[signal.InstanceID] = append(r.resolutions[signal.InstanceID], signal)
	}

	for {
		var signal CancelSignal
		if !cancel.ReceiveAsync(&signal) {
			break
		}
		reason := signal.Reason
		if reason == "" {
			reason = "run cancelled"
		}
		r.stop(failure("CANCELLED", reason))
	}
}
