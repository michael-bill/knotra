package execution

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

// RequestRecord retains the accepted command independently of delivery timing.
type RequestRecord struct {
	Request
	AcceptedAt time.Time       `json:"acceptedAt"`
	Answer     json.RawMessage `json:"answer"`
}

const (
	BackendTemporal  = "temporal"
	BackendRiver     = "river"
	SchedulerVersion = 1
	WorkerHeartbeat  = 5 * time.Second
	LeaseDuration    = 20 * time.Second
)

// Claim is returned only after all enclosing scopes have reserved capacity.
// The caller commits its transaction before passing Request to the executor.
type Claim struct {
	Ownership
	Request        ExecuteRequest
	OutcomeKey     string
	LeaseExpiresAt time.Time
	PlanID         string
}

// RunState contains control metadata, never a snapshot of all graph values.
type RunState struct {
	RunID              string
	Backend            string
	SchedulerVersion   int
	StateFormatVersion int
	AdmittedAt         time.Time
	Deadline           time.Time
	Revision           int64
	WakeGeneration     int64
	AppliedGeneration  int64
	Cancelled          bool
	Paused             bool
	StopCause          *Failure
	Status             string
	Now                time.Time
	GraphCursor        string
	GraphScanAgain     bool
	DeliveryCursor     string
}

// GraphRecord stores the complete values of one independently advancing graph.
// Address identifies an instance; GraphPath identifies its frozen definition.
type GraphRecord struct {
	RunID                 string                `json:"run_id"`
	ID                    string                `json:"id"`
	ParentInstanceID      string                `json:"parent_instance_id"`
	Address               string                `json:"address"`
	Pipeline              string                `json:"pipeline"`
	GraphPath             string                `json:"graph_path"`
	Inputs                contract.Values       `json:"inputs"`
	Permissions           *contract.Permissions `json:"permissions"`
	Scopes                []BudgetScope         `json:"scopes"`
	Deadline              time.Time             `json:"deadline"`
	State                 string                `json:"state"`
	MaterializationCursor int                   `json:"materialization_cursor"`
	Outputs               contract.Values       `json:"outputs"`
	Failure               *Failure              `json:"failure"`
	Revision              int64                 `json:"revision"`
	IterationIndex        *int                  `json:"iteration_index"`
}

type NodeRecord struct {
	RunID         string          `json:"run_id"`
	ID            string          `json:"id"`
	GraphID       string          `json:"graph_id"`
	NodeID        string          `json:"node_id"`
	Address       string          `json:"address"`
	State         string          `json:"state"`
	Inputs        contract.Values `json:"inputs"`
	Outputs       contract.Values `json:"outputs"`
	Failure       *Failure        `json:"failure"`
	Request       *ExecuteRequest `json:"execution_request"`
	Deadline      time.Time       `json:"deadline"`
	AttemptNumber int             `json:"attempt_number"`
	Revision      int64           `json:"revision"`
	StartedAt     *time.Time      `json:"started_at"`
	FinishedAt    *time.Time      `json:"finished_at"`
	Reason        string          `json:"reason"`
	InputsOmitted bool            `json:"inputs_omitted,omitempty"`
}

type AttemptRecord struct {
	AttemptID
	DispatchGeneration int64 `json:"dispatch_generation"`
	DispatchPending    bool  `json:"dispatch_pending"`
	State              string
	Ownership          *Ownership
	LeaseExpiresAt     time.Time `json:"lease_expires_at"`
	OutcomeKey         string    `json:"outcome_key"`
	Result             *ExecuteResult
	EvidenceKey        string `json:"evidence_key"`
	Outcome            *Outcome
}

// RootGraph uses the same node address seed as the retained v1 engine.
func RootGraph(runID, pipeline string, inputs contract.Values, limits contract.Limits, deadline time.Time) GraphRecord {
	address := runID + "/root"
	return GraphRecord{RunID: runID, ID: StableID("g", address), Address: address, Pipeline: pipeline,
		Inputs: inputs, Scopes: []BudgetScope{{ID: runID, Limits: limits}}, Deadline: deadline, State: "pending"}
}

type ScopeRecord struct {
	RunID                 string          `json:"run_id"`
	ID                    string          `json:"id"`
	Limits                contract.Limits `json:"limits"`
	MaterializedInstances int             `json:"materialized_instances"`
	ActiveAttempts        int             `json:"active_attempts"`
}

type TimerRecord struct {
	RunID      string    `json:"run_id"`
	ID         string    `json:"id"`
	Generation int64     `json:"generation"`
	InstanceID string    `json:"instance_id"`
	Kind       string    `json:"kind"`
	DueAt      time.Time `json:"due_at"`
}

// Controls retain coordination positions independently of leaf attempts. Child
// results remain in graph rows; foreach does not rewrite a growing result array.
type ControlRecord struct {
	RunID              string           `json:"run_id"`
	InstanceID         string           `json:"instance_id"`
	Kind               string           `json:"kind"`
	NextPosition       int              `json:"next_position"`
	CollectionPosition int              `json:"collection_position"`
	ActiveChildren     int              `json:"active_children"`
	SucceededChildren  int              `json:"succeeded_children"`
	FailedChildren     int              `json:"failed_children"`
	Iteration          int              `json:"iteration"`
	Elements           []contract.Value `json:"elements"`
	ElementCount       int              `json:"element_count"`
	CurrentElement     *contract.Value  `json:"current_element"`
	Concurrency        int              `json:"concurrency"`
	Carry              contract.Values  `json:"carry"`
	ChildGraphID       string           `json:"child_graph_id"`
	Revision           int64            `json:"revision"`
}

// NodeRequestID identifies the current wait, excluding prior resolution evidence.
func NodeRequestID(node NodeRecord) string {
	switch node.State {
	case "waiting_human":
		return StableID("h", node.ID+"/human")
	case "waiting_resolution":
		return StableID("q", node.ID+"/resolution/"+fmt.Sprint(node.AttemptNumber))
	}
	return ""
}
