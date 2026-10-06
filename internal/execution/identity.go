// Package execution defines backend-independent durable execution identities.
package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const StateFormatVersion = 1

func ValidateOutcomeKey(key string) error {
	if !validKey(key) {
		return errors.New("invalid outcome key")
	}
	return nil
}

// AttemptID remains stable across queue redelivery. Dispatch and ownership
// generations deliberately do not create another business attempt.
type AttemptID struct {
	RunID      string `json:"runId"`
	InstanceID string `json:"instanceId"`
	Number     int    `json:"number"`
}

func (id AttemptID) Validate() error {
	if id.RunID == "" || id.InstanceID == "" || id.Number < 1 {
		return errors.New("attempt requires run, instance and positive attempt number")
	}
	return nil
}

// Ownership identifies one process incarnation's claim on an attempt. It is
// included in outcome identity so a late worker cannot replace another result.
type Ownership struct {
	AttemptID
	WorkerID   string `json:"workerId"`
	Generation int64  `json:"generation"`
}

func (o Ownership) Validate() error {
	if err := o.AttemptID.Validate(); err != nil {
		return err
	}
	if o.WorkerID == "" || o.Generation < 1 {
		return errors.New("ownership requires worker incarnation and positive generation")
	}
	return nil
}

// OutcomeKey is recorded when ownership is claimed, before external execution.
// It can therefore be recovered without a successful post-execution DB write.
func (o Ownership) OutcomeKey() (string, error) {
	if err := o.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
