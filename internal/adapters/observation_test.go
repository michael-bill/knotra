package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

type observationHooks struct {
	*memoryHooks
	observations chan ExecutionEvent
	mu           sync.Mutex
	events       []ExecutionEvent
	failFirst    bool
}

func newObservationHooks() *observationHooks {
	return &observationHooks{memoryHooks: newHooks(), observations: make(chan ExecutionEvent, 256)}
}

func (h *observationHooks) Observe(_ context.Context, event ExecutionEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
	h.observations <- event
	if h.failFirst && len(h.events) == 1 {
		return errors.New("observation storage unavailable")
	}
	return nil
}

func (h *observationHooks) snapshot() []ExecutionEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ExecutionEvent(nil), h.events...)
}

func waitObservation(t *testing.T, hooks *observationHooks, typ string) ExecutionEvent {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-hooks.observations:
			if event.Type == typ {
				return event
			}
		case <-deadline.C:
			t.Fatalf("no %s observation", typ)
		}
	}
}

func TestLLMStreamsBeforeCompletionAndReplaysDurableResponse(t *testing.T) {
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","thinking":"hidden provider reasoning"},"done":false}`)
		_, _ = fmt.Fprintln(w, `{"message":{"content":"{\"answer\":"},"done":false}`)
		_, _ = fmt.Fprintln(w, `{"message":{"content":"4"},"done":false}`)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		_, _ = fmt.Fprintln(w, `{"message":{"content":"2}"},"done":false}`)
		_, _ = fmt.Fprintln(w, `{"done":true,"done_reason":"stop","prompt_eval_count":11,"eval_count":4}`)
	}))
	defer server.Close()
	hooks := newObservationHooks()
	runner := &Runner{Hooks: hooks}
	req := testRequest(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		values, err := runner.Execute(ctx, req)
		if err == nil && string(values["answer"].JSON) != "42" {
			err = fmt.Errorf("wrong output: %v", values)
		}
		finished <- err
	}()
	delta := waitObservation(t, hooks, "model.delta")
	if delta.Data["text"] != `{"answer":` {
		t.Fatalf("unexpected first delta: %#v", delta)
	}
	// The next small fragment must flush on the timer even while the provider
	// remains blocked, rather than waiting for another token or completion.
	if next := waitObservation(t, hooks, "model.delta"); next.Data["text"] != "4" {
		t.Fatalf("timer did not flush pending text: %#v", next)
	}
	select {
	case err := <-finished:
		t.Fatalf("returned before provider completion: %v", err)
	default:
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	completed := waitObservation(t, hooks, "model.completed")
	if completed.OperationID != delta.OperationID || completed.Data["content"] != `{"answer":42}` || completed.Data["inputTokens"] != json.Number("11") {
		t.Fatalf("incomplete model completion: %#v", completed)
	}
	if _, err := runner.Execute(ctx, req); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range hooks.snapshot() {
		if event.Type == "model.completed" {
			count++
		}
		data, _ := json.Marshal(event)
		if strings.Contains(string(data), "hidden provider reasoning") || strings.Contains(string(data), "thinking") {
			t.Fatalf("hidden reasoning leaked in %s", data)
		}
	}
	if count != 1 || requests.Load() != 1 || hooks.calls["model"] != 1 {
		t.Fatalf("replay duplicated execution: completions=%d requests=%d", count, requests.Load())
	}
}

func TestLLMRejectsBrokenStreamsWithoutPublishingCompletion(t *testing.T) {
	for name, body := range map[string]string{
		"missing terminal chunk": `{"message":{"content":"{\"answer\":42}"},"done":false}`,
		"malformed chunk":        "{bad json",
		"wrong field type":       `{"message":42,"done":true}`,
		"provider error":         `{"error":"provider failed"}`,
		"length stop":            `{"message":{"content":"{\"answer\":42}"},"done":true,"done_reason":"length"}`,
		"trailing data":          `{"done":true} {"message":{"content":"extra"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
			defer server.Close()
			hooks := newObservationHooks()
			runner := &Runner{Hooks: hooks}
			req := testRequest(server.URL)
			_, err := runner.Execute(context.Background(), req)
			var failed *Failure
			if !errors.As(err, &failed) || failed.Code != "OUTPUT_INVALID" || failed.Retryable || failed.Unknown {
				t.Fatalf("invalid stream has wrong failure: %v", err)
			}
			_, replayErr := runner.Execute(context.Background(), req)
			if !errors.As(replayErr, &failed) || failed.Code != "OUTPUT_INVALID" || hooks.calls["model"] != 1 {
				t.Fatalf("invalid response replay changed execution: %v", replayErr)
			}
			failures := 0
			for _, event := range hooks.snapshot() {
				if event.Type == "model.completed" {
					t.Fatal("invalid stream published completion")
				}
				if event.Type == "model.failed" {
					failures++
				}
			}
			if failures != 1 {
				t.Fatalf("model did not publish one failure: %d", failures)
			}
		})
	}
}

func TestLLMStreamingCancellationInterruptsReader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = fmt.Fprintln(w, `{"message":{"content":"partial"}}`)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		<-req.Context().Done()
	}))
	defer server.Close()
	hooks := newObservationHooks()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := (&Runner{Hooks: hooks}).Execute(ctx, testRequest(server.URL)); finished <- err }()
	waitObservation(t, hooks, "model.delta")
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled stream succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled stream reader remained blocked")
	}
}

func TestObservationsRedactSplitCredentialsAndRecoverFromObserverFailure(t *testing.T) {
	secret := "credential-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, `{"message":{"content":"{\"answer\":\"visible credential-"}}`)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprintln(w, `{"message":{"content":"value\"}"},"done":true}`)
	}))
	defer server.Close()
	req := testRequest(server.URL)
	connection := req.Plan.Profile.Spec.Models["local"]
	connection.Auth = map[string]contract.Credential{"key": {Value: &secret}}
	req.Plan.Profile.Spec.Models["local"] = connection
	req.Node.Outputs["answer"] = contract.Port{Schema: json.RawMessage(`{"type":"string"}`)}
	req.Node.LLM.Prompt.Text = "Credential: " + secret + " /host/private/work"
	hooks := newObservationHooks()
	hooks.failFirst = true
	runner := &Runner{Hooks: hooks, WorkDir: "/host/private/work"}
	values, err := runner.Execute(context.Background(), req)
	if err != nil || string(values["answer"].JSON) != `"visible credential-value"` {
		t.Fatalf("observation affected actual result: %v %v", values, err)
	}
	text := ""
	gap := false
	for _, event := range hooks.snapshot() {
		if event.Type == "model.delta" {
			delta, ok := event.Data["text"].(string)
			if !ok {
				t.Fatalf("invalid text delta: %+v", event)
			}
			text += delta
		}
		gap = gap || event.Data["observationIncomplete"] == true
		data, _ := json.Marshal(event)
		if strings.Contains(string(data), secret) || strings.Contains(string(data), "/host/private/work") {
			t.Fatalf("private data leaked: %s", data)
		}
	}
	if text != `{"answer":"visible [redacted]"}` || !gap {
		t.Fatalf("split redaction or gap marker failed: %q gap=%v", text, gap)
	}
}

func TestObservationPreviewIsBoundedAndRemovesNestedReasoning(t *testing.T) {
	hooks := newObservationHooks()
	o := (&Runner{Hooks: hooks}).newObservation(context.Background(), testRequest(""))
	o.emit(context.Background(), "tool.completed", "operation", map[string]any{
		"step": 1, "result": map[string]any{
			"thinking": "private", "answer": strings.Repeat("\x00", 100000),
			strings.Repeat("key", 30000): "value", "password": "password value",
		},
	})
	o.finish(context.Background())
	event := hooks.snapshot()[0]
	data, _ := json.Marshal(event.Data)
	if len(data) > 64<<10 || event.Data["truncated"] != true || strings.Contains(string(data), "private") || strings.Contains(string(data), "password value") {
		t.Fatalf("unsafe preview (%d bytes): %.200s", len(data), data)
	}
}

func TestStreamingToolCallsPreserveAgentFollowupContext(t *testing.T) {
	body := strings.Join([]string{
		`{"message":{"role":"assistant","thinking":"private internal context"}}`,
		`{"message":{"content":"Using a tool.","tool_calls":[{"function":{"name":"knotra_files_read","arguments":{"path":"source.txt"}}}]}}`,
		`{"message":{"tool_calls":[{"function":{"name":"knotra_files_write","arguments":{"path":"result.txt","content":"done"}}}]},"done":true}`,
	}, "\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
	defer server.Close()
	hooks := newObservationHooks()
	runner := &Runner{Hooks: hooks}
	req := testRequest(server.URL)
	req.observation = runner.newObservation(context.Background(), req)
	response, err := runner.chat(context.Background(), req, "model", 0, []message{{Role: "user", Content: "Do task"}}, nil, nil)
	if err != nil || len(response.ToolCalls) != 2 || response.ToolCalls[1].Function.Name != "knotra_files_write" || response.Thinking != "private internal context" {
		t.Fatalf("streamed conversation was not assembled: %#v %v", response, err)
	}
	req.observation.finish(context.Background())
	// Provider-private context is necessary in the next provider request, but
	// never appears in the user-facing cycle events.
	data, _ := json.Marshal(hooks.snapshot())
	if strings.Contains(string(data), "private internal context") {
		t.Fatalf("provider-private context exposed: %s", data)
	}
}

func TestObservationRedactionPreservesOverlappingAndEscapedSecrets(t *testing.T) {
	o := &executionObservation{}
	o.addRedaction("abcY")
	o.addRedaction("YZ")
	o.addRedaction("secret\nvalue")
	for _, value := range []string{"abcY abcY", "secret\\nval"} {
		cut := o.safePrefix(value)
		prefix := o.text(value[:cut])
		if strings.Contains(prefix, "abc") || strings.Contains(prefix, "secret") {
			t.Fatalf("partial credential exposed at boundary: %q / %q", prefix, value[cut:])
		}
	}
}

func TestChatStreamRejectsOversizedResponse(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`{"message":{"content":"` + strings.Repeat("x", maxResponseBytes) + `"},"done":true}`))
	_, _, err := readChatStream(context.Background(), body, time.Now(), nil, "operation", 1)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize response was accepted: %v", err)
	}
}

func TestObservationsPreserveUnsupportedNumbersAsExplicitText(t *testing.T) {
	hooks := newObservationHooks()
	o := (&Runner{Hooks: hooks}).newObservation(context.Background(), testRequest(""))
	floating := -1e30
	integer := int64(-9007199254740993)
	o.emit(context.Background(), "tool.completed", "operation", map[string]any{
		"step":    1,
		"result":  json.RawMessage(`{"positive":9007199254740993,"negative":-9007199254740993,"exponent":1e1000,"small":1e-1000,"safe":9007199254740991,"fraction":1.25}`),
		"pointer": &integer, "floating": &floating,
	})
	o.finish(context.Background())
	event := hooks.snapshot()[0]
	if event.Data["truncated"] != true || event.Data["step"] != json.Number("1") {
		t.Fatalf("missing numeric preview marker: %#v", event)
	}
	result, ok := event.Data["result"].(map[string]any)
	if !ok {
		t.Fatalf("invalid result preview: %+v", event)
	}
	for key, spelling := range map[string]string{
		"positive": "9007199254740993", "negative": "-9007199254740993", "exponent": "1e1000", "small": "1e-1000",
	} {
		if result[key] != "[unsupported number: "+spelling+"]" {
			t.Fatalf("number %s was rounded or not marked: %#v", key, result[key])
		}
	}
	if result["safe"] != json.Number("9007199254740991") || result["fraction"] != json.Number("1.25") || event.Data["pointer"] != "[unsupported number: -9007199254740993]" || event.Data["floating"] != "[unsupported number: -1e+30]" {
		t.Fatalf("scalar or pointer normalization failed: %#v", event.Data)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("observation poisoned JSON decoding: %v", err)
	}
}

func TestProviderTokenCountsCannotPoisonObservationHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"message":{"content":"{\"answer\":42}"},"done":true,"prompt_eval_count":9007199254740993,"eval_count":-9007199254740993}`)
	}))
	defer server.Close()
	hooks := newObservationHooks()
	values, err := (&Runner{Hooks: hooks}).Execute(context.Background(), testRequest(server.URL))
	if err != nil || string(values["answer"].JSON) != "42" {
		t.Fatalf("provider diagnostics changed outputs: %v %v", values, err)
	}
	completed := waitObservation(t, hooks, "model.completed")
	if completed.Data["inputTokens"] != "[unsupported number: 9007199254740993]" || completed.Data["outputTokens"] != "[unsupported number: -9007199254740993]" || completed.Data["truncated"] != true {
		t.Fatalf("provider counts were exposed as unsafe numbers: %#v", completed.Data)
	}
}

type heldObservationHooks struct {
	*observationHooks
	entered, release, returned, durable chan struct{}
	first, journal                      sync.Once
}

func newHeldObservationHooks() *heldObservationHooks {
	return &heldObservationHooks{observationHooks: newObservationHooks(), entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{}), durable: make(chan struct{})}
}

func (h *heldObservationHooks) Observe(ctx context.Context, event ExecutionEvent) error {
	first := false
	h.first.Do(func() { first = true })
	if first {
		close(h.entered)
		defer close(h.returned)
		select {
		case <-h.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h.observationHooks.Observe(ctx, event)
}

func (h *heldObservationHooks) CompleteOperation(ctx context.Context, id string, data json.RawMessage) error {
	err := h.memoryHooks.CompleteOperation(ctx, id, data)
	if err == nil {
		h.journal.Do(func() { close(h.durable) })
	}
	return err
}

func TestStalledObservationCannotBlockProviderOrDurableCompletion(t *testing.T) {
	hooks := newHeldObservationHooks()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Ensure the diagnostic writer is already stalled while the model responds.
		select {
		case <-hooks.entered:
		case <-req.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, `{"message":{"content":"{\"answer\":42}"},"done":true}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		values, err := (&Runner{Hooks: hooks}).Execute(ctx, testRequest(server.URL))
		if err == nil && string(values["answer"].JSON) != "42" {
			err = fmt.Errorf("wrong durable value: %v", values)
		}
		finished <- err
	}()
	select {
	case <-hooks.durable:
	case <-ctx.Done():
		t.Fatal("diagnostic storage blocked durable model completion")
	}
	select {
	case <-hooks.returned:
		t.Fatal("model completion waited for the stalled observation")
	default:
	}
	close(hooks.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if len(hooks.snapshot()) == 0 {
		t.Fatal("healthy final drain lost every observation")
	}
}

func TestObservationQueueBoundsBackpressureAndMarksOrderedGaps(t *testing.T) {
	hooks := newHeldObservationHooks()
	o := (&Runner{Hooks: hooks}).newObservation(context.Background(), testRequest(""))
	o.emit(context.Background(), "model.delta", "first", map[string]any{"text": "first"})
	select {
	case <-hooks.entered:
	case <-time.After(time.Second):
		t.Fatal("writer never started")
	}
	for index := 0; index < observationQueueSize+10; index++ {
		o.emit(context.Background(), "model.delta", fmt.Sprint(index), map[string]any{"text": "next"})
	}
	if len(o.queue) != observationQueueSize {
		t.Fatalf("queue did not remain bounded: %d", len(o.queue))
	}
	close(hooks.release)
	o.finish(context.Background())
	events := hooks.snapshot()
	if len(events) != observationQueueSize+1 {
		t.Fatalf("unexpected retained events: %d", len(events))
	}
	for index, event := range events[1:] {
		if event.OperationID != fmt.Sprint(index) || event.Data["observationIncomplete"] != true {
			t.Fatalf("lost accepted ordering or gap marker: %d %#v", index, event)
		}
	}
}

func TestObservationDrainStopsItsWorkerAndKeepsCancellationDiagnostics(t *testing.T) {
	hooks := newHeldObservationHooks()
	o := (&Runner{Hooks: hooks}).newObservation(context.Background(), testRequest(""))
	o.emit(context.Background(), "model.started", "call", map[string]any{"step": 1})
	select {
	case <-hooks.entered:
	case <-time.After(time.Second):
		t.Fatal("writer never started")
	}
	o.finish(context.Background())
	select {
	case <-o.done:
	case <-time.After(time.Second):
		t.Fatal("diagnostic worker outlived bounded drain")
	}

	fast := newObservationHooks()
	ctx, cancel := context.WithCancel(context.Background())
	last := (&Runner{Hooks: fast}).newObservation(ctx, testRequest(""))
	cancel()
	last.emit(ctx, "model.failed", "call", map[string]any{"error": "cancelled"})
	last.finish(ctx)
	if events := fast.snapshot(); len(events) != 1 || events[0].Type != "model.failed" {
		t.Fatalf("cancellation dropped an immediately writable final event: %#v", events)
	}
}
