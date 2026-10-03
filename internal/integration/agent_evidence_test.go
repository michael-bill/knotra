package integration

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
)

type journalOperation struct {
	kind      string
	completed bool
	response  json.RawMessage
}

// assertAgentEvidence proves actual model/tool cycles from durable responses,
// rather than inferring execution from the prompt or a finished report. The
// scenario has one root agent, three builtins and one inherited dataset.stats
// grant. Each requested call is correlated with its exact delivery journal ID.
func assertAgentEvidence(t *testing.T, ctx context.Context, dsn string, run protocol.Run) {
	t.Helper()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var rawPlan []byte
	if err = tx.QueryRow(ctx, "SELECT plan FROM knotra_runs WHERE id=$1", run.ID).Scan(&rawPlan); err != nil {
		t.Fatal(err)
	}
	var plan contract.Plan
	if err = json.Unmarshal(rawPlan, &plan); err != nil {
		t.Fatal(err)
	}
	root := plan.Pipelines[plan.Root]
	if root == nil || root.Spec.Nodes["write_report"].Agent == nil {
		t.Fatal("acceptance agent is absent from admitted plan")
	}
	maxSteps := root.Spec.Nodes["write_report"].Agent.MaxSteps
	var agent protocol.Instance
	for _, instance := range run.Instances {
		if instance.NodeID == "write_report" {
			if agent.ID != "" {
				t.Fatal("ambiguous report agent instance")
			}
			agent = instance
		}
	}
	if agent.ID == "" || agent.Status != "succeeded" {
		t.Fatalf("report agent did not succeed: %+v", agent)
	}
	attempt, err := strconv.Atoi(strings.TrimPrefix(agent.AttemptID, agent.ID+".a"))
	if err != nil || attempt < 1 {
		t.Fatalf("agent attempt identity is missing: %s", agent.AttemptID)
	}
	var marker string
	if err = json.Unmarshal(run.Inputs["marker"], &marker); err != nil || marker == "" {
		t.Fatal("run marker missing", err)
	}
	rows, err := tx.Query(ctx, "SELECT id,kind,completed,response FROM knotra_operations WHERE run_id=$1", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	operations := map[string]journalOperation{}
	for rows.Next() {
		var id string
		var operation journalOperation
		if err = rows.Scan(&id, &operation.kind, &operation.completed, &operation.response); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		operations[id] = operation
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	id := func(name string) string {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/attempt/%d", run.ID, agent.ID, name, attempt)))
		return hex.EncodeToString(sum[:])
	}
	completed := map[string]int{}
	turns, additionalRequests, lastFinish := 0, 0, false
	var finishArguments json.RawMessage
	for step := 0; step < maxSteps; step++ {
		operation, exists := operations[id(fmt.Sprintf("model/%d", step))]
		if !exists {
			break
		}
		if operation.kind != "model" || !operation.completed {
			t.Fatal("agent model call lacks a durable completed response")
		}
		var response struct {
			Done    bool   `json:"done"`
			Error   string `json:"error"`
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		}
		if err = json.Unmarshal(operation.response, &response); err != nil || !response.Done || response.Error != "" {
			t.Fatal("invalid real model response in journal", err)
		}
		turns++
		lastFinish = len(response.Message.ToolCalls) == 1 && response.Message.ToolCalls[0].Function.Name == "knotra_finish"
		if lastFinish {
			finishArguments = response.Message.ToolCalls[0].Function.Arguments
		}
		for index, call := range response.Message.ToolCalls {
			name := call.Function.Name
			if name == "knotra_finish" {
				continue
			}
			key := fmt.Sprintf("builtin/%d/%d", step, index)
			if strings.HasPrefix(name, "mcp_") {
				key = fmt.Sprintf("mcp/dataset/stats/%d/%d", step, index)
				name = "dataset.stats"
			}
			tool, exists := operations[id(key)]
			if !exists || tool.kind != "tool" || !tool.completed || !successfulAgentTool(name, call.Function.Arguments, tool.response, marker) {
				// Extra calls, invalid arguments and recoverable errors may
				// prompt another turn. Only matching successful replies count
				// toward the required evidence above.
				additionalRequests++
				continue
			}
			completed[name]++
		}
	}
	if turns < 2 || !lastFinish {
		t.Errorf("agent did not perform multiple turns ending in solitary finish: turns=%d finish=%v", turns, lastFinish)
	}
	for _, name := range []string{"knotra_files_read", "knotra_files_write", "knotra_process_exec", "dataset.stats"} {
		if completed[name] == 0 {
			t.Errorf("agent %s has no successful journal-correlated %s execution", agent.ID, name)
		}
	}
	attemptRecord := operations[id("agent-attempt")]
	var outputs contract.Values
	if !attemptRecord.completed || json.Unmarshal(attemptRecord.response, &outputs) != nil || len(outputs["report"].Artifacts) != 1 {
		t.Fatal("agent final outputs/artifact were not committed")
	}
	var finish struct {
		Summary string `json:"summary"`
	}
	var committedSummary string
	if json.Unmarshal(finishArguments, &finish) != nil || json.Unmarshal(outputs["summary"].JSON, &committedSummary) != nil || finish.Summary == "" || finish.Summary != committedSummary {
		t.Error("solitary finish arguments do not match committed agent result")
	}
	report := outputs["report"].Artifacts[0]
	if report.Origin["runId"] != run.ID || report.Origin["instanceId"] != agent.ID || report.Origin["attemptId"] != agent.AttemptID {
		t.Error("agent artifact origin does not match the verified attempt")
	}
	t.Logf("real agent evidence: run=%s instance=%s modelTurns=%d files.read=%d files.write=%d process.exec=%d MCP dataset.stats=%d additionalToolRequests=%d solitaryFinish=%v committedArtifacts=%d", run.ID, agent.ID, turns, completed["knotra_files_read"], completed["knotra_files_write"], completed["knotra_process_exec"], completed["dataset.stats"], additionalRequests, lastFinish, len(outputs["report"].Artifacts))
}

func successfulAgentTool(name string, arguments, response json.RawMessage, marker string) bool {
	var result struct {
		IsError      bool   `json:"isError"`
		Error        string `json:"error"`
		Content      string `json:"content"`
		Encoding     string `json:"encoding"`
		BytesWritten int    `json:"bytesWritten"`
		ExitCode     *int   `json:"exitCode"`
		Stdout       string `json:"stdout"`
	}
	if name == "dataset.stats" {
		// MCP content is an array, unlike the file-read result's string.
		var mcpResult struct {
			IsError    bool        `json:"isError"`
			Structured statsOutput `json:"structuredContent"`
		}
		var input statsInput
		return json.Unmarshal(response, &mcpResult) == nil && !mcpResult.IsError && mcpResult.Structured.Count == 3 && mcpResult.Structured.Sum == 12 && json.Unmarshal(arguments, &input) == nil && input.Marker == marker
	}
	if json.Unmarshal(response, &result) != nil || result.IsError || result.Error != "" {
		return false
	}
	switch name {
	case "knotra_files_read":
		if result.Encoding == "base64" {
			decoded, err := base64.StdEncoding.Strict().DecodeString(result.Content)
			if err != nil {
				return false
			}
			result.Content = string(decoded)
		}
		return strings.Contains(result.Content, "value,marker") && strings.Contains(result.Content, marker)
	case "knotra_files_write":
		var input struct{ Path, Content string }
		return json.Unmarshal(arguments, &input) == nil && input.Path == "report.md" && strings.Contains(input.Content, marker) && result.BytesWritten > 0
	case "knotra_process_exec":
		var input struct{ Command []string }
		return json.Unmarshal(arguments, &input) == nil && len(input.Command) >= 3 && strings.HasPrefix(input.Command[0], "python") && result.ExitCode != nil && *result.ExitCode == 0 && strings.Contains(result.Stdout, marker)
	default:
		return false
	}
}
