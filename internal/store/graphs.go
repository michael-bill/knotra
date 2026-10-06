package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store/db"
)

// These record methods compose inside the caller's run-locked transaction.
// Reads retain full values; projections are never used for scheduler recovery.
func ReadExecutionGraph(ctx context.Context, tx pgx.Tx, runID, id string) (execution.GraphRecord, error) {
	var record execution.GraphRecord
	var b []byte
	b, err := db.New(tx).ReadExecutionGraph(ctx, db.ReadExecutionGraphParams{RunID: runID, ID: id})
	if err == nil {
		err = json.Unmarshal(b, &record)
	}
	return record, classify(err)
}

func ReadExecutionAttempts(ctx context.Context, tx pgx.Tx, runID, graphID string, instanceIDs []string) ([]execution.AttemptRecord, error) {
	documents, err := db.New(tx).ReadExecutionAttempts(ctx, db.ReadExecutionAttemptsParams{RunID: runID, GraphID: graphID, InstanceIds: instanceIDs})
	return decodeRecords[execution.AttemptRecord](documents, err)
}

func ReadExecutionScopes(ctx context.Context, tx pgx.Tx, graph execution.GraphRecord) ([]execution.ScopeRecord, error) {
	ids := make([]string, len(graph.Scopes))
	for i, scope := range graph.Scopes {
		ids[i] = scope.ID
	}
	documents, err := db.New(tx).ReadExecutionScopes(ctx, db.ReadExecutionScopesParams{RunID: graph.RunID, ScopeIDs: ids})
	records, err := decodeRecords[execution.ScopeRecord](documents, err)
	if err == nil && len(records) != len(ids) {
		err = errors.New("missing enclosing graph scope")
	}
	return records, err
}

func ReadExecutionRetryTimers(ctx context.Context, tx pgx.Tx, runID, graphID string, instanceIDs []string) ([]execution.TimerRecord, error) {
	documents, err := db.New(tx).ReadExecutionRetryTimers(ctx, db.ReadExecutionRetryTimersParams{RunID: runID, GraphID: graphID, InstanceIds: instanceIDs})
	return decodeRecords[execution.TimerRecord](documents, err)
}

func ReadExecutionRequests(ctx context.Context, tx pgx.Tx, runID, graphID string, instanceIDs []string) ([]execution.RequestRecord, error) {
	documents, err := db.New(tx).ReadExecutionRequests(ctx, db.ReadExecutionRequestsParams{RunID: runID, GraphID: graphID, InstanceIds: instanceIDs})
	return decodeRecords[execution.RequestRecord](documents, err)
}

// SaveExecutionRequest composes request creation/closure with node transitions.
// Accepted response columns remain immutable evidence even after consumption.
func (s *Store) SaveExecutionRequest(ctx context.Context, tx pgx.Tx, request execution.Request) error {
	if request.Status != "accepted" && request.Status != "resolved" {
		return s.SaveRequestTx(ctx, tx, request)
	}
	b, err := json.Marshal(request)
	if err != nil {
		return err
	}
	tag, err := db.New(tx).SaveExecutionRequest(ctx, db.SaveExecutionRequestParams{
		RunID:      request.RunID,
		ID:         request.ID,
		Status:     request.Status,
		Document:   b,
		ResponseID: new(request.ResponseID),
	})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}

func InsertExecutionGraph(ctx context.Context, tx pgx.Tx, graph execution.GraphRecord) error {
	inputs, err := json.Marshal(graph.Inputs)
	if err != nil {
		return err
	}
	permissions, err := json.Marshal(graph.Permissions)
	if err != nil {
		return err
	}
	scopes, err := json.Marshal(graph.Scopes)
	if err != nil {
		return err
	}
	_, err = db.New(tx).InsertExecutionGraph(ctx, db.InsertExecutionGraphParams{
		RunID:            graph.RunID,
		ID:               graph.ID,
		ParentInstanceID: graph.ParentInstanceID,
		Address:          graph.Address,
		Pipeline:         graph.Pipeline,
		GraphPath:        graph.GraphPath,
		Inputs:           inputs,
		Permissions:      permissions,
		Scopes:           scopes,
		Deadline:         graph.Deadline,
		State:            graph.State,
		Revision:         graph.Revision,
		IterationIndex:   graph.IterationIndex,
	})
	if err == nil && graph.ParentInstanceID != "" {
		tag, childErr := db.New(tx).AddExecutionChild(ctx, db.AddExecutionChildParams{RunID: graph.RunID, InstanceID: graph.ParentInstanceID})
		if childErr != nil {
			return childErr
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	return err
}

func SaveExecutionGraph(ctx context.Context, tx pgx.Tx, graph execution.GraphRecord) error {
	inputs, err := json.Marshal(graph.Inputs)
	if err != nil {
		return err
	}
	outputs, err := json.Marshal(graph.Outputs)
	if err != nil {
		return err
	}
	failure, err := json.Marshal(graph.Failure)
	if err != nil {
		return err
	}
	tag, err := db.New(tx).SaveExecutionGraph(ctx, db.SaveExecutionGraphParams{
		RunID:                 graph.RunID,
		ID:                    graph.ID,
		State:                 graph.State,
		MaterializationCursor: graph.MaterializationCursor,
		Inputs:                inputs,
		Outputs:               outputs,
		Failure:               failure,
		Revision:              graph.Revision,
	})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err == nil && graph.ParentInstanceID != "" && protocol.Terminal(graph.State) {
		tag, err = db.New(tx).FinishExecutionChild(ctx, db.FinishExecutionChildParams{RunID: graph.RunID, InstanceID: graph.ParentInstanceID, State: graph.State})
		if err == nil && tag.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	return err
}

func SaveExecutionNode(ctx context.Context, tx pgx.Tx, node execution.NodeRecord, definition contract.Node) error {
	if node.InputsOmitted {
		return errors.New("cannot save a node with omitted admitted inputs")
	}
	inputs, err := json.Marshal(node.Inputs)
	if err != nil {
		return err
	}
	outputs, err := json.Marshal(node.Outputs)
	if err != nil {
		return err
	}
	failure, err := json.Marshal(node.Failure)
	if err != nil {
		return err
	}
	request, err := json.Marshal(node.Request)
	if err != nil {
		return err
	}
	tag, err := db.New(tx).SaveExecutionNode(ctx, db.SaveExecutionNodeParams{
		RunID:                 node.RunID,
		ID:                    node.ID,
		GraphID:               node.GraphID,
		NodeID:                node.NodeID,
		Address:               node.Address,
		State:                 node.State,
		Inputs:                inputs,
		Outputs:               outputs,
		Failure:               failure,
		ExecutionRequest:      request,
		Deadline:              optionalTime(node.Deadline),
		AttemptNumber:         node.AttemptNumber,
		Revision:              node.Revision,
		StartedAt:             node.StartedAt,
		FinishedAt:            node.FinishedAt,
		Reason:                node.Reason,
		NodeKind:              definition.Type,
		CurrentRequestID:      execution.NodeRequestID(node),
		DependencyNodeIds:     append([]string{}, definition.Dependencies...),
		RemainingDependencies: len(definition.Dependencies),
	})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err == nil && protocol.Terminal(node.State) {
		// The guarded node transition releases each reverse edge once. Readiness
		// metadata changes with the result, without rewriting dependent values.
		// ponytail: fan-out updates together; page edges if it exhausts the five-second scheduling step.
		_, err = db.New(tx).ReleaseNodeDependencies(ctx, db.ReleaseNodeDependenciesParams{RunID: node.RunID, GraphID: node.GraphID, NodeID: node.NodeID})
	}
	return err
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func InsertExecutionAttempt(ctx context.Context, tx pgx.Tx, attempt execution.AttemptRecord) error {
	if err := attempt.Validate(); err != nil {
		return err
	}
	if attempt.State != "ready" || attempt.DispatchGeneration < 1 || attempt.Ownership != nil || attempt.Result != nil {
		return errors.New("new attempt must be unclaimed and ready")
	}
	_, err := db.New(tx).InsertExecutionAttempt(ctx, db.InsertExecutionAttemptParams{
		RunID:              attempt.RunID,
		InstanceID:         attempt.InstanceID,
		Number:             attempt.Number,
		DispatchGeneration: attempt.DispatchGeneration,
		DispatchPending:    attempt.DispatchPending,
	})
	return err
}

func ReadExecutionControls(ctx context.Context, tx pgx.Tx, runID, graphID string, instanceIDs []string) ([]execution.ControlRecord, error) {
	documents, err := db.New(tx).ReadExecutionControls(ctx, db.ReadExecutionControlsParams{RunID: runID, GraphID: graphID, InstanceIds: instanceIDs})
	return decodeRecords[execution.ControlRecord](documents, err)
}

func ReadChildExecutionGraphs(ctx context.Context, tx pgx.Tx, runID, graphID string, instanceIDs []string) ([]execution.GraphRecord, error) {
	// Loop/pipeline reads select their current child. Foreach reads only the first
	// failed child or a page of at most 64 immutable results awaiting validation.
	documents, err := db.New(tx).ReadChildExecutionGraphs(ctx, db.ReadChildExecutionGraphsParams{RunID: runID, GraphID: graphID, InstanceIds: instanceIDs})
	return decodeRecords[execution.GraphRecord](documents, err)
}

func SaveExecutionControl(ctx context.Context, tx pgx.Tx, control execution.ControlRecord) error {
	if control.Revision < 1 {
		return errors.New("control transition requires a positive revision")
	}
	var elements []byte
	var err error
	if control.Revision == 1 {
		elements, err = json.Marshal(control.Elements)
		if err != nil {
			return err
		}
	}
	carry, err := json.Marshal(control.Carry)
	if err != nil {
		return err
	}
	tag, err := db.New(tx).SaveExecutionControl(ctx, db.SaveExecutionControlParams{
		RunID: control.RunID, InstanceID: control.InstanceID, Kind: control.Kind,
		NextPosition: control.NextPosition, Iteration: control.Iteration, Elements: elements,
		CollectionPosition: control.CollectionPosition,
		Concurrency:        control.Concurrency,
		Carry:              carry, ChildGraphID: control.ChildGraphID, Revision: control.Revision,
	})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}

// ReadForeachCollectedOutputs assembles complete exports once, after every child
// page is validated. Child rows are immutable; no growing result array is saved
// or reloaded on intermediate coordination steps.
func ReadForeachCollectedOutputs(ctx context.Context, tx pgx.Tx, control execution.ControlRecord, body contract.Graph) (contract.Values, error) {
	if control.Kind != "foreach" || control.NextPosition != control.ElementCount || control.CollectionPosition != control.ElementCount || control.ActiveChildren != 0 || control.FailedChildren != 0 || control.SucceededChildren != control.ElementCount {
		return nil, errors.New("foreach collection is not complete")
	}
	var jsonPorts, artifactPorts []string
	for name, port := range body.Outputs {
		if port.Artifact == nil {
			jsonPorts = append(jsonPorts, name)
		} else {
			artifactPorts = append(artifactPorts, name)
		}
	}
	document, err := db.New(tx).ReadForeachCollectedOutputs(ctx, db.ReadForeachCollectedOutputsParams{RunID: control.RunID, InstanceID: new(control.InstanceID), JsonPorts: jsonPorts, ArtifactPorts: artifactPorts})
	var outputs contract.Values
	if err == nil {
		err = json.Unmarshal(document, &outputs)
	}
	return outputs, err
}

func InsertExecutionScope(ctx context.Context, tx pgx.Tx, scope execution.ScopeRecord) error {
	limits, err := json.Marshal(scope.Limits)
	if err != nil {
		return err
	}
	_, err = db.New(tx).InsertExecutionScope(ctx, db.InsertExecutionScopeParams{RunID: scope.RunID, ID: scope.ID, Limits: limits})
	return err
}

// ReadSchedulableNodes bounds full record loading; readiness metadata is selected
// before PostgreSQL serializes any inputs, results or admitted requests.
func ReadSchedulableNodes(ctx context.Context, tx pgx.Tx, run execution.RunState, graph execution.GraphRecord, focusID string, bound int) ([]execution.NodeRecord, error) {
	documents, err := db.New(tx).ReadSchedulableNodes(ctx, db.ReadSchedulableNodesParams{
		RunID: run.RunID, GraphID: graph.ID, FocusInstanceID: focusID, NodeLimit: bound,
		Now: run.Now, Paused: run.Paused,
		Stopping: run.Cancelled || run.StopCause != nil || !run.Deadline.After(run.Now) || !graph.Deadline.After(run.Now),
	})
	return decodeRecords[execution.NodeRecord](documents, err)
}

// ReadExecutionDependencyValues loads states and complete outputs only, never
// the sibling's old inputs, request, control or attempt history.
func ReadExecutionDependencyValues(ctx context.Context, tx pgx.Tx, runID, graphID string, nodeIDs []string) ([]execution.NodeRecord, error) {
	documents, err := db.New(tx).ReadExecutionDependencyValues(ctx, db.ReadExecutionDependencyValuesParams{RunID: runID, GraphID: graphID, NodeIds: nodeIDs})
	return decodeRecords[execution.NodeRecord](documents, err)
}

// ReadExecutionGraphMetadata leaves immutable inputs out of coordination reads.
func ReadExecutionGraphMetadata(ctx context.Context, tx pgx.Tx, runID, id string) (execution.GraphRecord, error) {
	var record execution.GraphRecord
	b, err := db.New(tx).ReadExecutionGraphMetadata(ctx, db.ReadExecutionGraphMetadataParams{RunID: runID, ID: id})
	if err == nil {
		err = json.Unmarshal(b, &record)
	}
	return record, classify(err)
}

// A nil port selection reads all inputs for initial contract validation. An
// empty selection reads none; named ports retain their complete values.
func ReadExecutionGraphInputValues(ctx context.Context, tx pgx.Tx, runID, id string, names []string) (contract.Values, error) {
	var values contract.Values
	b, err := db.New(tx).ReadExecutionGraphInputValues(ctx, db.ReadExecutionGraphInputValuesParams{RunID: runID, ID: id, PortNames: names})
	if err == nil {
		err = json.Unmarshal(b, &values)
	}
	return values, classify(err)
}

func ReadExecutionNodeInputValues(ctx context.Context, tx pgx.Tx, runID, id string, names []string) (contract.Values, error) {
	var values contract.Values
	b, err := db.New(tx).ReadExecutionNodeInputValues(ctx, db.ReadExecutionNodeInputValuesParams{RunID: runID, ID: id, PortNames: names})
	if err == nil {
		err = json.Unmarshal(b, &values)
	}
	return values, classify(err)
}

// ReadExecutionNode restores the original admitted inputs/request before a
// selected foreach changes node state or its public projection.
func ReadExecutionNode(ctx context.Context, tx pgx.Tx, runID, id string) (execution.NodeRecord, error) {
	var record execution.NodeRecord
	b, err := db.New(tx).ReadExecutionNode(ctx, db.ReadExecutionNodeParams{RunID: runID, ID: id})
	if err == nil {
		err = json.Unmarshal(b, &record)
	}
	return record, classify(err)
}
