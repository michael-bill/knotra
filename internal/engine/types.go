// Package engine implements deterministic, durable orchestration. External I/O
// belongs to named activities; workflows only evaluate the admitted plan.
package engine

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
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

type RunResult struct {
	Status  string          `json:"status"`
	Outputs contract.Values `json:"outputs,omitempty"`
	Failure *Failure        `json:"failure,omitempty"`
}

// Failure is an explicit activity outcome. Retryable must only be set when the
// adapter has proved that starting a fresh attempt cannot duplicate an effect.
// A transport error from the activity itself is never proof of that property.
type Failure struct {
	Code                  string `json:"code"`
	Message               string `json:"message"`
	Retryable             bool   `json:"retryable,omitempty"`
	Unknown               bool   `json:"unknown,omitempty"`
	OperationID           string `json:"operationId,omitempty"`
	CanRetryIfNotExecuted bool   `json:"canRetryIfNotExecuted,omitempty"`
}

func (f *Failure) Error() string { return fmt.Sprintf("%s: %s", f.Code, f.Message) }

// BudgetScope identifies one durable counter set. Every physical model/tool
// request must atomically consume a counter in every enclosing scope. A scope's
// node and active-attempt counters are maintained by the workflow.
type BudgetScope struct {
	ID     string          `json:"id"`
	Limits contract.Limits `json:"limits"`
}

type ExecuteRequest struct {
	RunID         string          `json:"runId"`
	InstanceID    string          `json:"instanceId"`
	NodeID        string          `json:"nodeId"`
	Pipeline      string          `json:"pipeline"`
	Attempt       int             `json:"attempt"`
	Node          contract.Node   `json:"node"`
	Inputs        contract.Values `json:"inputs"`
	ToolArguments json.RawMessage `json:"toolArguments,omitempty"`
	// Plan is populated by the activity host from its admitted run snapshot.
	// The workflow omits it to avoid copying the source package per attempt.
	Plan   contract.Plan `json:"plan,omitzero"`
	Scopes []BudgetScope `json:"scopes"`
	// Permissions is the intersection of all caller boundaries. Nil means the
	// root document, which is still constrained by the trusted profile.
	Permissions *contract.Permissions `json:"permissions,omitempty"`
	Deadline    time.Time             `json:"deadline"`
}

type ExecuteResult struct {
	Outputs contract.Values `json:"outputs,omitempty"`
	Failure *Failure        `json:"failure,omitempty"`
}

// Projection is an idempotent event: (RunID, Sequence) is its unique key.
// The storage activity must preserve an existing event on duplicate delivery.
type Projection struct {
	ParentInstanceID string          `json:"parentInstanceId,omitempty"`
	IterationIndex   *int            `json:"iterationIndex,omitempty"`
	GraphPath        string          `json:"graphPath,omitempty"`
	NodeType         string          `json:"nodeType,omitempty"`
	Inputs           contract.Values `json:"inputs,omitempty"`
	DataTruncated    bool            `json:"dataTruncated,omitempty"`
	StartedAt        *time.Time      `json:"startedAt,omitempty"`
	FinishedAt       *time.Time      `json:"finishedAt,omitempty"`

	RunID      string          `json:"runId"`
	Sequence   int64           `json:"sequence"`
	Time       time.Time       `json:"time"`
	Kind       string          `json:"kind"`
	InstanceID string          `json:"instanceId,omitempty"`
	NodeID     string          `json:"nodeId,omitempty"`
	Pipeline   string          `json:"pipeline,omitempty"`
	Status     string          `json:"status"`
	Attempt    int             `json:"attempt,omitempty"`
	Outputs    contract.Values `json:"outputs,omitempty"`
	Failure    *Failure        `json:"failure,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	RequestID  string          `json:"requestId,omitempty"`
	ResponseID string          `json:"responseId,omitempty"`
}

// RequestActivity durably creates a human/resolution request, or closes an
// existing request. Calls are idempotent by (RunID, ID, Status, ResponseID).
type Request struct {
	RunID      string                   `json:"runId"`
	ID         string                   `json:"id"`
	InstanceID string                   `json:"instanceId"`
	Kind       string                   `json:"kind"`
	Status     string                   `json:"status"`
	Prompt     string                   `json:"prompt,omitempty"`
	Inputs     contract.Values          `json:"inputs,omitempty"`
	Outputs    map[string]contract.Port `json:"outputs,omitempty"`
	Failure    *Failure                 `json:"failure,omitempty"`
	Deadline   time.Time                `json:"deadline"`
	ResponseID string                   `json:"responseId,omitempty"`
	Response   contract.Values          `json:"response,omitempty"`
	Evidence   string                   `json:"evidence,omitempty"`
}

type HumanSignal struct {
	RequestID  string          `json:"requestId"`
	ResponseID string          `json:"responseId"`
	Values     contract.Values `json:"values"`
	AcceptedAt time.Time       `json:"acceptedAt,omitempty"`
}

// AnswerActivity arbitrates a deadline/cancellation race with the API's durable
// response transaction. It returns the first accepted answer. When no answer
// exists and CloseIfAbsent is set, it atomically closes the request instead.
type AnswerRequest struct {
	RunID         string `json:"runId"`
	RequestID     string `json:"requestId"`
	CloseIfAbsent string `json:"closeIfAbsent,omitempty"` // cancelled or expired
}

type ResolutionSignal struct {
	InstanceID  string          `json:"instanceId"`
	OperationID string          `json:"operationId"`
	ResponseID  string          `json:"responseId"`
	Decision    string          `json:"decision"` // completed, not_executed, or failed
	Outputs     contract.Values `json:"outputs,omitempty"`
	Evidence    string          `json:"evidence"`
	AcceptedAt  time.Time       `json:"acceptedAt,omitempty"`
}

type CancelSignal struct {
	Reason string `json:"reason,omitempty"`
}

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
