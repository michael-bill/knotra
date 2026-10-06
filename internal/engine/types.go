// Package engine implements deterministic, durable orchestration. External I/O
// belongs to named activities; workflows only evaluate the admitted plan.
package engine

import (
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

const (
	WorkflowName       = "knotra.run"
	PlanActivity       = "knotra.plan"
	ExecuteActivity    = "knotra.execute"
	ProjectActivity    = "knotra.project"
	RequestActivity    = "knotra.request"
	AnswerActivity     = "knotra.answer"
	ResolutionActivity = "knotra.resolution"
	HumanSignalName    = "knotra.human"
	ResolveSignalName  = "knotra.resolve"
	CancelSignalName   = "knotra.cancel"
	SnapshotQuery      = "knotra.snapshot"
)

type RunInput struct {
	RunID      string          `json:"runId"`
	AcceptedAt time.Time       `json:"acceptedAt"`
	Plan       contract.Plan   `json:"plan,omitzero"`
	Inputs     contract.Values `json:"inputs"`
	Checkpoint *Checkpoint     `json:"checkpoint,omitempty"`
}

// PlanActivity loads the immutable admitted plan. Production starts carry only
// the run identity, keeping source packages out of continuation checkpoints.
type PlanRequest struct {
	RunID string `json:"runId"`
}

type RunResult = execution.RunResult

// Failure is an explicit activity outcome. Retryable must only be set when the
// adapter has proved that starting a fresh attempt cannot duplicate an effect.
// A transport error from the activity itself is never proof of that property.
type Failure = execution.Failure

// BudgetScope identifies one durable counter set. Every physical model/tool
// request must atomically consume a counter in every enclosing scope. A scope's
// node and active-attempt counters are maintained by the workflow.
type BudgetScope = execution.BudgetScope

type ExecuteRequest = execution.ExecuteRequest

type ExecuteResult = execution.ExecuteResult

// Projection is an idempotent event: (RunID, Sequence) is its unique key.
// The storage activity must preserve an existing event on duplicate delivery.
type Projection = execution.Projection

// RequestActivity durably creates a human/resolution request, or closes an
// existing request. Calls are idempotent by (RunID, ID, Status, ResponseID).
type Request = execution.Request

type HumanSignal = execution.HumanSignal

// AnswerActivity arbitrates a deadline/cancellation race with the API's durable
// response transaction. It returns the first accepted answer. When no answer
// exists and CloseIfAbsent is set, it atomically closes the request instead.
type AnswerRequest = execution.AnswerRequest

type ResolutionSignal = execution.ResolutionSignal

type CancelSignal = execution.CancelSignal

type NodeSnapshot struct {
	ParentInstanceID string          `json:"parentInstanceId,omitempty"`
	IterationIndex   *int            `json:"iterationIndex,omitempty"`
	GraphPath        string          `json:"graphPath,omitempty"`
	NodeType         string          `json:"nodeType,omitempty"`
	Inputs           contract.Values `json:"inputs,omitempty"`
	DataTruncated    bool            `json:"dataTruncated,omitempty"`
	StartedAt        *time.Time      `json:"startedAt,omitempty"`
	FinishedAt       *time.Time      `json:"finishedAt,omitempty"`
	Reason           string          `json:"reason,omitempty"`

	ID            string          `json:"id"`
	NodeID        string          `json:"nodeId"`
	Pipeline      string          `json:"pipeline"`
	Status        string          `json:"status"`
	Attempt       int             `json:"attempt,omitempty"`
	Outputs       contract.Values `json:"outputs,omitempty"`
	Failure       *Failure        `json:"failure,omitempty"`
	Deadline      time.Time       `json:"deadline,omitempty"`
	ChildDeadline time.Time       `json:"childDeadline,omitempty"`
}

// Checkpoint moves orchestration state into the next Temporal history without
// creating a new logical run. Only quiescent executions may produce one.
type Checkpoint struct {
	State       Snapshot                           `json:"state"`
	Sequence    int64                              `json:"sequence"`
	Deadline    time.Time                          `json:"deadline"`
	NodeCounts  map[string]int                     `json:"nodeCounts"`
	Projected   map[string]bool                    `json:"projected"`
	Loops       map[string]LoopCheckpoint          `json:"loops"`
	Foreach     map[string]map[int]contract.Values `json:"foreach"`
	Humans      map[string][]HumanSignal           `json:"humans"`
	Resolutions map[string][]ResolutionSignal      `json:"resolutions"`
}

type LoopCheckpoint struct {
	Index int             `json:"index"`
	State contract.Values `json:"state"`
}

type Snapshot struct {
	RunID    string                   `json:"runId"`
	Status   string                   `json:"status"`
	Nodes    map[string]*NodeSnapshot `json:"nodes"`
	Requests map[string]*Request      `json:"requests"`
	Outputs  contract.Values          `json:"outputs,omitempty"`
	Failure  *Failure                 `json:"failure,omitempty"`
}
