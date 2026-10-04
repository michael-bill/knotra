package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

func cloudRequest(provider, endpoint string) Request {
	req := testRequest(endpoint)
	key := "fixture-key"
	req.Plan.Profile.Spec.Models["local"] = contract.ModelConnection{
		Provider: provider, Model: "fixture-model", BaseURL: endpoint,
		Auth: map[string]contract.Credential{"key": {Value: &key}},
	}
	return req
}

func decodeRequest(t *testing.T, r *http.Request) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Error(err)
	}
	return body
}

func checkCloudAuth(t *testing.T, provider string, req *http.Request) {
	t.Helper()
	if provider == "openai" {
		if req.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("missing bearer credential")
		}
	} else if req.Header.Get("x-api-key") != "fixture-key" || req.Header.Get("anthropic-version") != "2023-06-01" {
		t.Error("missing Anthropic credential or protocol version")
	}
}

// Fixtures are literal provider wire responses, independent of adapter structs.
func cloudOutput(provider, name, arguments string) string {
	args, _ := json.Marshal(arguments)
	if provider == "openai" {
		return fmt.Sprintf(`{"status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":%q,"arguments":%s}],"usage":{"input_tokens":17,"output_tokens":9}}`, name, args)
	}
	return fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_1","name":%q,"input":%s}],"stop_reason":"tool_use","usage":{"input_tokens":17,"output_tokens":9}}`, name, arguments)
}

func writeCloudOutput(w http.ResponseWriter, provider, name, args string, streaming bool) {
	response := cloudOutput(provider, name, args)
	if !streaming {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if provider == "openai" {
		fmt.Fprint(w, ": keep-alive\r\n\r\n")
		fmt.Fprintf(w, "event: response.output_item.added\r\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":%q,\"arguments\":\"\"}}\r\n\r\n", name)
		for _, fragment := range []string{args[:len(args)/2], args[len(args)/2:]} {
			encoded, _ := json.Marshal(fragment)
			fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":%s}\n\n", encoded)
		}
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
		return
	}
	fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":17,\"output_tokens\":1}}}\n\n")
	fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":%q,\"input\":{}}}\n\n", name)
	for _, fragment := range []string{args[:len(args)/2], args[len(args)/2:]} {
		encoded, _ := json.Marshal(fragment)
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}}\n\n", encoded)
	}
	fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
}

func TestCloudStructuredOutputAndReplay(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", provider, streaming), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					calls.Add(1)
					checkCloudAuth(t, provider, req)
					body := decodeRequest(t, req)
					if req.Method != "POST" || string(body["stream"]) != "true" || string(body["model"]) != `"fixture-model"` {
						t.Error("wrong generation request")
					}
					var tools []map[string]json.RawMessage
					if json.Unmarshal(body["tools"], &tools) != nil || len(tools) != 1 {
						t.Error("missing output tool")
						return
					}
					if string(tools[0]["name"]) != `"knotra_output"` {
						t.Error("wrong output tool")
					}
					var choice map[string]any
					_ = json.Unmarshal(body["tool_choice"], &choice)
					if choice["name"] != "knotra_output" {
						t.Error("output tool is not forced")
					}
					if provider == "openai" {
						if req.URL.Path != "/responses" || string(body["store"]) != "false" || string(tools[0]["strict"]) != "false" {
							t.Error("wrong Responses contract")
						}
					} else if req.URL.Path != "/messages" || string(body["max_tokens"]) != "4096" {
						t.Error("wrong Messages contract")
					}
					writeCloudOutput(w, provider, "knotra_output", `{"answer":42}`, streaming)
				}))
				defer server.Close()
				hooks := newHooks()
				runner := &Runner{Hooks: hooks}
				req := cloudRequest(provider, server.URL)
				for range 2 {
					values, err := runner.Execute(context.Background(), req)
					if err != nil || string(values["answer"].JSON) != "42" {
						t.Fatalf("values=%v err=%v", values, err)
					}
				}
				if calls.Load() != 1 || hooks.calls["model"] != 1 {
					t.Fatal("replay generated or charged another model call")
				}
			})
		}
	}
}

func TestCloudAdmissionResolvesRemoteIDAndRejectsDrift(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			var drift atomic.Bool
			var generations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				checkCloudAuth(t, provider, req)
				if req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/models/") {
					id := "resolved-model"
					if drift.Load() {
						id = "changed-model"
					}
					fmt.Fprintf(w, `{"id":%q}`, id)
					return
				}
				generations.Add(1)
				writeCloudOutput(w, provider, "knotra_output", `{"answer":42}`, false)
			}))
			defer server.Close()
			req := cloudRequest(provider, server.URL)
			req.Plan.Pipelines[req.Pipeline].Spec.Sandboxes = nil
			runner := &Runner{Hooks: newHooks()}
			if err := runner.Prepare(context.Background(), req.Plan); err != nil {
				t.Fatal(err)
			}
			if pipeline(req).Spec.Models["model"].Model != "resolved-model" || req.Plan.ModelDigests["local/resolved-model"] != "remote-id:resolved-model" {
				t.Fatal("model alias was not fixed at admission")
			}
			drift.Store(true)
			_, err := runner.Execute(context.Background(), req)
			var failed *Failure
			if !errors.As(err, &failed) || failed.Code != "RESOURCE_CHANGED" || generations.Load() != 0 {
				t.Fatalf("drift generated a response: %v", err)
			}
		})
	}
}

func TestCloudHTTPFailuresDoNotRetryOrLeakBodies(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 503} {
			t.Run(fmt.Sprintf("%s/%d", provider, status), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					calls.Add(1)
					http.Error(w, "fixture-key private prompt", status)
				}))
				defer server.Close()
				hooks := newHooks()
				runner := &Runner{Hooks: hooks}
				_, err := runner.Execute(context.Background(), cloudRequest(provider, server.URL))
				var failed *Failure
				if !errors.As(err, &failed) || failed.Retryable != (status == 408 || status == 429 || status >= 500) || failed.Unknown {
					t.Fatalf("wrong failure: %v", err)
				}
				if strings.Contains(err.Error(), "fixture-key") || strings.Contains(err.Error(), "private prompt") {
					t.Fatal("provider body leaked")
				}
				if calls.Load() != 1 || hooks.calls["model"] != 1 {
					t.Fatal("hidden retry or incorrect budget")
				}
			})
		}
	}
}

func TestCloudInvalidOutputsRemainDurable(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		for _, output := range []string{`{}`, `{"answer":"42"}`, `{"answer":42,"extra":1}`} {
			t.Run(provider+output, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					calls.Add(1)
					writeCloudOutput(w, provider, "knotra_output", output, true)
				}))
				defer server.Close()
				runner := &Runner{Hooks: newHooks()}
				req := cloudRequest(provider, server.URL)
				for range 2 {
					if _, err := runner.Execute(context.Background(), req); err == nil {
						t.Fatal("invalid output accepted")
					}
				}
				if calls.Load() != 1 {
					t.Fatal("invalid output was regenerated on replay")
				}
			})
		}
	}
}

func TestCloudStreamRejectsMissingCompletionAndMalformedData(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		for _, data := range []string{"data: nope\n\n", "data: {}\n\n", "", "data: {\"type\":\"error\",\"error\":{\"message\":\"fixture-key\"}}\n\n"} {
			t.Run(provider+data, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, data)
				}))
				defer server.Close()
				runner := &Runner{Hooks: newHooks()}
				req := cloudRequest(provider, server.URL)
				for range 2 {
					_, err := runner.Execute(context.Background(), req)
					var failed *Failure
					if !errors.As(err, &failed) || failed.Code != "OUTPUT_INVALID" || failed.Retryable || failed.Unknown {
						t.Fatalf("invalid stream: %v", err)
					}
				}
				if calls.Load() != 1 {
					t.Fatal("invalid stream was replayed externally")
				}
			})
		}
	}
}

func TestCloudStreamCancellation(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			entered := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, ": waiting\n\n")
				w.(http.Flusher).Flush()
				close(entered)
				<-req.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := (&Runner{Hooks: newHooks()}).Execute(ctx, cloudRequest(provider, server.URL))
				finished <- err
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("generation never started")
			}
			cancel()
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("cancelled request succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled stream did not close")
			}
		})
	}
}

func TestCloudConfigurationRejectsInvalidParameters(t *testing.T) {
	cases := []struct {
		provider, name string
		value          any
	}{
		{"openai", "num_predict", 256}, {"openai", "max_output_tokens", 1.5}, {"openai", "temperature", 3}, {"openai", "top_p", "0.5"}, {"openai", "reasoning_effort", "unknown"},
		{"anthropic", "max_tokens", 0}, {"anthropic", "temperature", 1.1}, {"anthropic", "stop_sequences", []string{""}}, {"anthropic", "top_k", -1}, {"anthropic", "store", false},
	}
	for _, c := range cases {
		t.Run(c.provider+"/"+c.name, func(t *testing.T) {
			req := cloudRequest(c.provider, "")
			connection := req.Plan.Profile.Spec.Models["local"]
			connection.Parameters = map[string]any{c.name: c.value}
			req.Plan.Profile.Spec.Models["local"] = connection
			if _, err := modelConfig(req, "model"); err == nil {
				t.Fatal("invalid parameter accepted")
			}
		})
	}
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider+"/missing-key", func(t *testing.T) {
			req := cloudRequest(provider, "")
			c := req.Plan.Profile.Spec.Models["local"]
			c.Auth = nil
			req.Plan.Profile.Spec.Models["local"] = c
			if _, err := modelConfig(req, "model"); err == nil {
				t.Fatal("missing authentication accepted")
			}
		})
		t.Run(provider+"/unsupported-capability", func(t *testing.T) {
			req := cloudRequest(provider, "")
			resource := pipeline(req).Spec.Models["model"]
			resource.Requires = []string{"imageInput"}
			pipeline(req).Spec.Models["model"] = resource
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, `{"id":"fixture-model"}`) }))
			defer server.Close()
			c := req.Plan.Profile.Spec.Models["local"]
			c.BaseURL = server.URL
			req.Plan.Profile.Spec.Models["local"] = c
			if err := (&Runner{}).prepareModels(context.Background(), req); err == nil {
				t.Fatal("unsupported capability accepted")
			}
		})
	}
}

func TestOpenAIStreamSeparatesTextAndStructuredArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"Ready."}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","name":"knotra_output"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"answer\":42}"}`+"\n\n")
		final := strings.Replace(cloudOutput("openai", "knotra_output", `{"answer":42}`), `"output":[`, `"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Ready."}]},`, 1)
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", final)
	}))
	defer server.Close()
	values, err := (&Runner{Hooks: newHooks()}).Execute(context.Background(), cloudRequest("openai", server.URL))
	if err != nil || string(values["answer"].JSON) != "42" {
		t.Fatalf("values=%v err=%v", values, err)
	}
}
