package scheduler

import (
	"encoding/json"
	"strings"

	"github.com/michael-bill/knotra/internal/execution"
)

func responsePriority(node execution.NodeRecord, records map[string]execution.RequestRecord) int {
	r := records[execution.NodeRequestID(node)]
	if r.Status == "answered" || r.Status == "resolved" {
		return 0
	}
	return 1
}

func requestDecision(node execution.NodeRecord, records map[string]execution.RequestRecord) (*execution.ExecuteResult, *execution.Request) {
	r, found := records[execution.NodeRequestID(node)]
	if !found || node.Request == nil || r.RunID != node.RunID || r.InstanceID != node.ID || r.ResponseID == "" || r.AcceptedAt.IsZero() || !r.AcceptedAt.Before(node.Deadline) {
		return nil, nil
	}
	closed := r.Request
	var result execution.ExecuteResult
	switch {
	case node.State == "waiting_human" && r.Kind == "human" && r.Status == "answered":
		if err := json.Unmarshal(r.Answer, &result.Outputs); err != nil {
			result.Failure = failure("RESPONSE_INVALID", "persisted human response is invalid")
		}
		closed.Status = "accepted"
	case node.State == "waiting_resolution" && r.Kind == "resolution" && r.Status == "resolved":
		var signal execution.ResolutionSignal
		if err := json.Unmarshal(r.Answer, &signal); err != nil || node.Failure == nil || signal.InstanceID != node.ID || signal.OperationID != node.Failure.OperationID || strings.TrimSpace(signal.Evidence) == "" {
			result.Failure = failure("RESOLUTION_INVALID", "persisted resolution does not match its admitted operation")
		} else {
			closed.Evidence = signal.Evidence
			switch signal.Decision {
			case "completed":
				result.Outputs = signal.Outputs
			case "not_executed":
				result.Failure = &execution.Failure{Code: "NOT_EXECUTED", Message: "operator confirmed operation did not execute", Retryable: node.Failure.CanRetryIfNotExecuted}
			case "failed":
				result.Failure = failure("EXTERNAL_FAILED", "operator confirmed external operation failed")
			default:
				result.Failure = failure("RESOLUTION_INVALID", "invalid persisted resolution decision")
			}
			if signal.Decision != "completed" && len(signal.Outputs) != 0 {
				result.Failure = failure("RESOLUTION_INVALID", "outputs require a completed outcome")
			}
		}
		closed.Status = "resolved"
	default:
		return nil, nil
	}
	closed.Response = execution.WithoutArtifactPaths(result.Outputs)
	return &result, &closed
}
