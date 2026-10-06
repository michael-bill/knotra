package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

func TestCloudToolConversationPreservesIDsAndPrivateContext(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body := decodeRequest(t, req)
				if calls.Add(1) == 1 {
					if provider == "openai" {
						_, _ = fmt.Fprint(w, `{"status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"private-encrypted-context","summary":[]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"work","arguments":"{\"value\":7}"}],"usage":{"input_tokens":4,"output_tokens":3}}`)
					} else {
						_, _ = fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"private-thinking","signature":"private-signature"},{"type":"tool_use","id":"call_1","name":"work","input":{"value":7}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":3}}`)
					}
					return
				}
				var raw []map[string]json.RawMessage
				if provider == "openai" {
					if json.Unmarshal(body["input"], &raw) != nil || len(raw) != 4 {
						t.Error("reasoning/tool history lost")
						return
					}
					if string(raw[1]["encrypted_content"]) != `"private-encrypted-context"` || string(raw[2]["call_id"]) != `"call_1"` || string(raw[3]["call_id"]) != `"call_1"` || string(raw[3]["type"]) != `"function_call_output"` {
						t.Error("OpenAI context or call identity lost")
					}
				} else {
					if json.Unmarshal(body["messages"], &raw) != nil || len(raw) != 3 {
						t.Error("tool history lost")
						return
					}
					var assistant, user []map[string]json.RawMessage
					_ = json.Unmarshal(raw[1]["content"], &assistant)
					_ = json.Unmarshal(raw[2]["content"], &user)
					if len(assistant) != 2 || len(user) != 1 || string(assistant[0]["signature"]) != `"private-signature"` || string(user[0]["tool_use_id"]) != `"call_1"` || string(user[0]["is_error"]) != "true" {
						t.Error("Anthropic context, ID or error feedback lost")
					}
				}
				writeCloudOutput(w, provider, "knotra_finish", `{"answer":42}`, true)
			}))
			defer server.Close()
			hooks := newObservationHooks()
			runner := &Runner{Hooks: hooks}
			req := cloudRequest(provider, server.URL)
			req.observation = runner.newObservation(context.Background(), req)
			defer req.observation.finish(context.Background())
			messages := []message{{Role: "user", Content: "Use work then finish."}}
			tools := []functionTool{toolDefinition("work", "Work", json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}}}`))}
			response, err := runner.chat(context.Background(), req, "model", 0, messages, tools, nil)
			if err != nil || len(response.ToolCalls) != 1 {
				t.Fatalf("first turn: %v", err)
			}
			messages = append(messages, response, message{Role: "tool", ToolName: "work", ToolCallID: response.ToolCalls[0].ID, Content: "Invalid arguments: value rejected"})
			response, err = runner.chat(context.Background(), req, "model", 1, messages, tools, nil)
			if err != nil || response.ToolCalls[0].Function.Name != "knotra_finish" {
				t.Fatalf("second turn: %v", err)
			}
			req.observation.finish(context.Background())
			for _, event := range hooks.snapshot() {
				data, _ := json.Marshal(event)
				if strings.Contains(string(data), "private-") {
					t.Fatal("provider-private context reached observations")
				}
			}
			if calls.Load() != 2 || hooks.calls["model"] != 2 {
				t.Fatal("conversation budgets incorrect")
			}
		})
	}
}

func TestCloudDockerAgentToolsValidationAndReplay(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			runner := integrationRunner(t)
			defer func() { _ = runner.Close() }()
			var turns atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body := decodeRequest(t, req)
				switch turns.Add(1) {
				case 1:
					writeCloudOutput(w, provider, "knotra_files_write", `{"path":"report.txt","content":"hello"}`, true)
				case 2:
					if !strings.Contains(string(body["input"])+string(body["messages"]), "call_1") {
						t.Error("tool result has no provider ID")
					}
					writeCloudOutput(w, provider, "knotra_finish", `{"answer":"wrong"}`, true)
				case 3:
					if !strings.Contains(string(body["input"])+string(body["messages"]), "Validation error:") {
						t.Error("output validation feedback was not returned")
					}
					writeCloudOutput(w, provider, "knotra_finish", `{"answer":42}`, true)
				default:
					t.Error("unexpected model call")
				}
			}))
			defer server.Close()
			req := cloudRequest(provider, server.URL)
			req.Node.Type, req.Node.Sandbox, req.Node.LLM = "agent", "box", nil
			req.Node.Agent = &contract.AgentNode{Model: "model", Prompt: contract.TextSource{Text: "Write report and finish."}, MaxSteps: 4}
			req.Node.Tools = &contract.ToolGrants{Sandbox: []string{"files.write"}}
			req.Node.Outputs["report"] = contract.Port{Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}, Collect: &contract.Collect{Path: "report.txt", MediaType: "text/plain"}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			for range 2 {
				values, err := runner.Execute(ctx, req)
				if err != nil || string(values["answer"].JSON) != "42" || len(values["report"].Artifacts) != 1 {
					t.Fatalf("agent outputs=%v err=%v", values, err)
				}
				data, err := runner.Hooks.GetArtifact(ctx, values["report"].Artifacts[0].ID)
				if err != nil || string(data) != "hello" {
					t.Fatal("sandbox artifact is incorrect")
				}
			}
			if turns.Load() != 3 {
				t.Fatal("completed agent attempt was reexecuted")
			}
		})
	}
}

func TestCloudStructuredStreamingObservations(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeCloudOutput(w, provider, "knotra_output", `{"answer":42}`, true)
			}))
			defer server.Close()
			hooks := newObservationHooks()
			if _, err := (&Runner{Hooks: hooks}).Execute(context.Background(), cloudRequest(provider, server.URL)); err != nil {
				t.Fatal(err)
			}
			var deltas strings.Builder
			var complete bool
			for _, event := range hooks.snapshot() {
				if event.Type == "model.delta" {
					_, _ = fmt.Fprint(&deltas, event.Data["text"])
				}
				if event.Type == "model.completed" {
					complete = event.Data["content"] == `{"answer":42}` && event.Data["inputTokens"] == json.Number("17") && event.Data["outputTokens"] == json.Number("9")
				}
			}
			if deltas.String() != `{"answer":42}` || !complete {
				t.Fatalf("stream=%q completion=%t", deltas.String(), complete)
			}
		})
	}
}
