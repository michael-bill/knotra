// Package queue integrates River OSS delivery with Knotra execution identities.
// Job arguments carry identities only; authoritative state belongs to Knotra.
package queue

import (
	"errors"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/michael-bill/knotra/internal/execution"
)

const RoutingVersion = 1

// completed is intentionally included. Generation changes permit new work even
// before retention removes old jobs; after retention, domain guards still apply.
func uniqueDelivery() river.UniqueOpts {
	return river.UniqueOpts{ByArgs: true, ByQueue: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStateCompleted,
		rivertype.JobStatePending, rivertype.JobStateRunning,
		rivertype.JobStateRetryable, rivertype.JobStateScheduled,
	}}
}

type AdvanceArgs struct {
	RunID          string `json:"runId"`
	WakeGeneration int64  `json:"wakeGeneration"`
	RoutingVersion int    `json:"routingVersion"`
}

func (AdvanceArgs) Kind() string { return "knotra_advance_v1" }
func (a AdvanceArgs) Validate() error {
	if a.RunID == "" || a.WakeGeneration < 1 || a.RoutingVersion != RoutingVersion {
		return errors.New("invalid advance identity or routing version")
	}
	return nil
}
func (AdvanceArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 25, UniqueOpts: uniqueDelivery()}
}

type ExecuteArgs struct {
	Attempt            execution.AttemptID `json:"attempt"`
	DispatchGeneration int64               `json:"dispatchGeneration"`
	RoutingVersion     int                 `json:"routingVersion"`
}

func (ExecuteArgs) Kind() string { return "knotra_execute_v1" }
func (a ExecuteArgs) Validate() error {
	if err := a.Attempt.Validate(); err != nil {
		return err
	}
	if a.DispatchGeneration < 1 || a.RoutingVersion != RoutingVersion {
		return errors.New("invalid execute generation or routing version")
	}
	return nil
}
func (ExecuteArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: uniqueDelivery()}
}

type FinalizeArgs struct {
	Attempt        execution.AttemptID `json:"attempt"`
	OutcomeKey     string              `json:"outcomeKey"`
	RoutingVersion int                 `json:"routingVersion"`
}

func (FinalizeArgs) Kind() string { return "knotra_finalize_v1" }
func (a FinalizeArgs) Validate() error {
	if err := a.Attempt.Validate(); err != nil {
		return err
	}
	if a.RoutingVersion != RoutingVersion {
		return errors.New("unsupported finalize routing version")
	}
	return execution.ValidateOutcomeKey(a.OutcomeKey)
}
func (FinalizeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 25, UniqueOpts: uniqueDelivery()}
}

type CleanupArgs struct {
	HostID              string `json:"hostId"`
	WorkerID            string `json:"workerId"`
	ResourceID          string `json:"resourceId"`
	OwnershipGeneration int64  `json:"ownershipGeneration"`
	DispatchGeneration  int64  `json:"dispatchGeneration"`
	RoutingVersion      int    `json:"routingVersion"`
}

func (CleanupArgs) Kind() string { return "knotra_cleanup_v1" }
func (a CleanupArgs) Validate() error {
	if err := execution.ValidateHostID(a.HostID); err != nil {
		return err
	}
	id, err := uuid.Parse(a.ResourceID)
	if err != nil || id.String() != a.ResourceID || a.HostID == "" || a.WorkerID == "" || a.OwnershipGeneration < 1 || a.DispatchGeneration < 1 || a.RoutingVersion != RoutingVersion {
		return errors.New("invalid cleanup identity or routing version")
	}
	return nil
}
func (CleanupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 25, UniqueOpts: uniqueDelivery()}
}
