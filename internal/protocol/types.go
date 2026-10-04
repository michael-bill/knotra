// Package protocol contains the HTTP/CLI wire types shared by clients and server.
package protocol

import (
	"encoding/json"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

const Version = "knotra.desktop/1"

type Error struct {
	Code        string                `json:"code"`
	Message     string                `json:"message"`
	Diagnostics []contract.Diagnostic `json:"diagnostics"`
}

type Definition struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Title         string           `json:"title"`
	PackageDigest string           `json:"packageDigest"`
	CreatedAt     time.Time        `json:"createdAt"`
	Package       contract.Package `json:"package"`
}

type Instance struct {
	ParentInstanceID string               `json:"parentInstanceId,omitempty"`
	IterationIndex   *int                 `json:"iterationIndex,omitempty"`
	GraphPath        string               `json:"graphPath,omitempty"`
	NodeType         string               `json:"nodeType,omitempty"`
	Inputs           map[string]any       `json:"inputs,omitempty"`
	Outputs          map[string]any       `json:"outputs,omitempty"`
	DataTruncated    bool                 `json:"dataTruncated,omitempty"`
	StartedAt        *time.Time           `json:"startedAt,omitempty"`
	FinishedAt       *time.Time           `json:"finishedAt,omitempty"`
	UpdatedAt        *time.Time           `json:"updatedAt,omitempty"`
	Reason           string               `json:"reason,omitempty"`
	ID               string               `json:"id"`
	NodeID           string               `json:"nodeId"`
	Scope            string               `json:"scope"`
	Status           string               `json:"status"`
	AttemptID        string               `json:"attemptId,omitempty"`
	Error            *contract.Diagnostic `json:"error,omitempty"`
}

type Run struct {
	ID               string                     `json:"id"`
	DefinitionID     string                     `json:"definitionId"`
	Title            string                     `json:"title"`
	Status           string                     `json:"status"`
	CreatedAt        time.Time                  `json:"createdAt"`
	UpdatedAt        time.Time                  `json:"updatedAt"`
	Profile          string                     `json:"profile"`
	Package          contract.Package           `json:"package"`
	Inputs           map[string]json.RawMessage `json:"inputs"`
	InputArtifacts   map[string]any             `json:"inputArtifacts"`
	Outputs          map[string]json.RawMessage `json:"outputs"`
	Artifacts        []contract.Artifact        `json:"artifacts"`
	Instances        []Instance                 `json:"instances"`
	Diagnostics      []contract.Diagnostic      `json:"diagnostics"`
	AvailableActions []string                   `json:"availableActions"`
}

type Event struct {
	ID          string    `json:"id"`
	RunID       string    `json:"runId"`
	At          time.Time `json:"at"`
	Type        string    `json:"type"`
	Message     string    `json:"message"`
	InstanceID  string    `json:"instanceId,omitempty"`
	AttemptID   string    `json:"attemptId,omitempty"`
	OperationID string    `json:"operationId,omitempty"`
	Data        any       `json:"data,omitempty"`
}

type HumanRequest struct {
	ID             string          `json:"id"`
	RunID          string          `json:"runId"`
	InstanceID     string          `json:"instanceId"`
	AttemptID      string          `json:"attemptId"`
	Status         string          `json:"status"`
	Prompt         string          `json:"prompt"`
	CreatedAt      time.Time       `json:"createdAt"`
	Deadline       time.Time       `json:"deadline"`
	Inputs         any             `json:"inputs"`
	ResponseSchema json.RawMessage `json:"responseSchema"`
}

type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

func Terminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "cancelled" || status == "skipped"
}
