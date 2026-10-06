package execution

import (
	"errors"
	"fmt"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

// Failure describes a business outcome, independently of queue delivery errors.
// Retryable is proof of safe re-execution, never inferred from a transport error.
type Failure struct {
	Code                  string `json:"code"`
	Message               string `json:"message"`
	Retryable             bool   `json:"retryable,omitempty"`
	Unknown               bool   `json:"unknown,omitempty"`
	OperationID           string `json:"operationId,omitempty"`
	CanRetryIfNotExecuted bool   `json:"canRetryIfNotExecuted,omitempty"`
}

func (f *Failure) Error() string { return fmt.Sprintf("%s: %s", f.Code, f.Message) }

// ArtifactReference identifies immutable bytes already written by the executor.
// An outcome is evidence; it does not by itself publish artifact metadata.
type ArtifactReference struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Outcome is the durable handoff between physical execution and publication.
// External responses must still be journaled separately: this file cannot turn
// an unjournaled external write into a confirmed successful operation.
type Outcome struct {
	FormatVersion int                 `json:"formatVersion"`
	Ownership     Ownership           `json:"ownership"`
	PlanID        string              `json:"planId"`
	CompletedAt   time.Time           `json:"completedAt"`
	Outputs       contract.Values     `json:"outputs,omitempty"`
	Failure       *Failure            `json:"failure,omitempty"`
	Artifacts     []ArtifactReference `json:"artifacts,omitempty"`
}

func (o Outcome) Validate() error {
	if o.FormatVersion != StateFormatVersion {
		return errors.New("unsupported outcome format version")
	}
	if err := o.Ownership.Validate(); err != nil {
		return err
	}
	if o.PlanID == "" || o.CompletedAt.IsZero() {
		return errors.New("outcome requires frozen plan identity and completion time")
	}
	if o.Failure != nil && (o.Failure.Code == "" || len(o.Outputs) > 0) {
		return errors.New("failed outcome requires a failure code and no published outputs")
	}
	seen := make(map[string]bool, len(o.Artifacts))
	for _, a := range o.Artifacts {
		if a.ID == "" || !validKey(a.SHA256) || a.Size < 0 || seen[a.ID] {
			return errors.New("invalid or duplicate outcome artifact reference")
		}
		seen[a.ID] = true
	}
	return nil
}
