package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/michael-bill/knotra/internal/contract"
)

// ExecutionEvent is diagnostic progress, not a durable execution decision.
// Step numbers in Data are one-based; OperationID correlates a model/tool call.
type ExecutionEvent struct {
	Type        string
	OperationID string
	Data        map[string]any
}

// ExecutionObserver is optional on Hooks. Losing observations must never repeat
// external effects or turn an otherwise completed operation into a failure.
// Implementations must honor the supplied context deadline.
type ExecutionObserver interface {
	Observe(context.Context, ExecutionEvent) error
}

const maxObservationBytes = 48 << 10
const maxObservationString = 16 << 10
const observationQueueSize = 64
const observationWriteTimeout = 250 * time.Millisecond
const observationDrainTimeout = 250 * time.Millisecond

type executionObservation struct {
	hook       ExecutionObserver
	redactions []string
	incomplete atomic.Bool
	queue      chan ExecutionEvent
	done       chan struct{}
	cancel     context.CancelFunc
	finishOnce sync.Once
}

func (r *Runner) newObservation(ctx context.Context, req Request) *executionObservation {
	hook, ok := r.Hooks.(ExecutionObserver)
	if !ok {
		return nil
	}
	o := &executionObservation{hook: hook}
	profile := req.Plan.Profile
	for name := range profile.Spec.Secrets {
		if value, err := r.secret(profile, name); err == nil {
			o.addRedaction(value)
		}
	}
	credentials := func(values map[string]contract.Credential) {
		for _, credential := range values {
			if value, err := r.credential(profile, credential); err == nil {
				o.addRedaction(value)
			}
		}
	}
	for _, model := range profile.Spec.Models {
		credentials(model.Auth)
	}
	for _, server := range profile.Spec.MCP {
		credentials(server.Headers)
		credentials(server.Env)
	}
	o.addRedaction(r.WorkDir)
	o.addRedaction(r.HelperPath)
	// Diagnostic writes have their own bounded lifetime. Provider cancellation
	// must not prevent an already queued failure/completion from being recorded.
	writeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o.queue, o.done, o.cancel = make(chan ExecutionEvent, observationQueueSize), make(chan struct{}), cancel
	go o.deliver(writeCtx)
	return o
}

func (o *executionObservation) deliver(ctx context.Context) {
	defer close(o.done)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-o.queue:
			if !ok || ctx.Err() != nil {
				return
			}
			if o.incomplete.Load() {
				event.Data["observationIncomplete"] = true
			}
			writeCtx, cancel := context.WithTimeout(ctx, observationWriteTimeout)
			err := o.hook.Observe(writeCtx, event)
			cancel()
			if err != nil {
				o.incomplete.Store(true)
			}
		}
	}
}

// finish is called only after execution and its durable operation journal have
// finished. Drain briefly for fast stores, but never change the execution result
// or wait indefinitely for diagnostics. The execution goroutine owns emission.
func (o *executionObservation) finish(ctx context.Context) {
	if o == nil {
		return
	}
	o.finishOnce.Do(func() {
		close(o.queue)
		drain := observationDrainTimeout
		interrupted := ctx.Done()
		if ctx.Err() != nil {
			// An already cancelled execution still gets a small opportunity to
			// persist its final diagnostic without delaying cancellation much.
			drain, interrupted = 50*time.Millisecond, nil
		}
		timer := time.NewTimer(drain)
		defer timer.Stop()
		select {
		case <-o.done:
		case <-timer.C:
		case <-interrupted:
		}
		o.cancel()
	})
}

func (o *executionObservation) addRedaction(value string) {
	if o == nil || value == "" {
		return
	}
	for _, existing := range o.redactions {
		if existing == value {
			return
		}
	}
	o.redactions = append(o.redactions, value)
	encoded, _ := json.Marshal(value)
	if escaped := string(encoded[1 : len(encoded)-1]); escaped != value {
		o.redactions = append(o.redactions, escaped)
	}
	// Replace longest values first so overlapping credentials cannot leave tails.
	sort.Slice(o.redactions, func(i, j int) bool { return len(o.redactions[i]) > len(o.redactions[j]) })
}

func (o *executionObservation) text(value string) string {
	for _, secret := range o.redactions {
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	return value
}

func (o *executionObservation) emit(ctx context.Context, typ, operationID string, data map[string]any) {
	if o == nil {
		return
	}
	// JSON normalization also handles RawMessage and typed tool result structs.
	raw, err := json.Marshal(data)
	if err != nil {
		o.incomplete.Store(true)
		return
	}
	var normalized map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&normalized) != nil {
		o.incomplete.Store(true)
		return
	}
	remaining := maxObservationBytes
	truncated := false
	clean := o.clean(normalized, 0, &remaining, &truncated).(map[string]any)
	// Large prompts/tool results must not consume the correlation metadata.
	for _, key := range []string{"step", "maxSteps", "model", "name", "providerName", "valid", "isError", "durationMs", "inputTokens", "outputTokens", "firstTokenMs"} {
		if value, ok := normalized[key]; ok {
			budget := 512
			clean[key] = o.clean(value, 0, &budget, &truncated)
		}
	}
	if truncated {
		clean["truncated"] = true
	}
	if o.incomplete.Load() {
		clean["observationIncomplete"] = true
	}
	// Escaping control characters can make JSON larger than its text preview.
	if encoded, _ := json.Marshal(clean); len(encoded) > maxObservationBytes {
		clean = map[string]any{"truncated": true, "originalBytes": len(encoded)}
		for _, key := range []string{"step", "model", "name", "valid", "durationMs", "inputTokens", "outputTokens", "firstTokenMs"} {
			if value, ok := normalized[key]; ok {
				budget := 1024
				clean[key] = o.clean(value, 0, &budget, &truncated)
			}
		}
		if o.incomplete.Load() {
			clean["observationIncomplete"] = true
		}
	}
	select {
	case o.queue <- ExecutionEvent{Type: typ, OperationID: operationID, Data: clean}:
	default:
		// Bounded loss is preferable to applying backpressure to a provider or
		// delaying durable effect completion. Later delivered events expose it.
		o.incomplete.Store(true)
	}
}

func (o *executionObservation) clean(value any, depth int, remaining *int, truncated *bool) any {
	if depth > 12 || *remaining <= 0 {
		*truncated = true
		return "[truncated]"
	}
	switch value := value.(type) {
	case json.Number:
		parsed, err := value.Float64()
		significand := strings.Split(strings.ToLower(value.String()), "e")[0]
		underflow := parsed == 0 && strings.ContainsAny(significand, "123456789")
		if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) || underflow || (math.Trunc(parsed) == parsed && math.Abs(parsed) > 9007199254740991) {
			// Diagnostic payloads may precede schema validation. Never let an
			// untrusted tool/provider number poison the strict SSE/JSON clients,
			// and retain its exact spelling instead of silently rounding it.
			*truncated = true
			return o.clean("[unsupported number: "+value.String()+"]", depth, remaining, truncated)
		}
		*remaining -= len(value.String())
		return value
	case string:
		value = o.text(value)
		limit := min(maxObservationString, *remaining)
		if len(value) > limit {
			limit = utf8Prefix(value, limit)
			value = value[:limit] + "…"
			*truncated = true
		}
		*remaining -= len(value) + 8
		return value
	case map[string]any:
		out := map[string]any{}
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i >= 128 || *remaining <= 0 {
				*truncated = true
				break
			}
			cleanKey := o.text(key)
			if len(cleanKey) > 128 {
				cleanKey = cleanKey[:utf8Prefix(cleanKey, 128)] + "…"
				*truncated = true
			}
			switch strings.ToLower(strings.ReplaceAll(key, "-", "_")) {
			case "thinking", "reasoning", "reasoning_content":
				continue
			case "authorization", "api_key", "apikey", "password", "secret", "access_token", "refresh_token":
				out[cleanKey] = "[redacted]"
				continue
			}
			*remaining -= len(cleanKey) + 8
			out[cleanKey] = o.clean(value[key], depth+1, remaining, truncated)
		}
		return out
	case []any:
		out := make([]any, 0, min(len(value), 64))
		for i, item := range value {
			if i >= 64 || *remaining <= 0 {
				*truncated = true
				break
			}
			out = append(out, o.clean(item, depth+1, remaining, truncated))
		}
		return out
	default:
		*remaining -= 16
		return value
	}
}

func utf8Prefix(value string, limit int) int {
	for limit > 0 && limit < len(value) && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return limit
}

// safePrefix retains every suffix that could be the beginning of a credential.
// Redaction must work even when the provider splits a secret between chunks.
func (o *executionObservation) safePrefix(value string) int {
	cut := len(value)
	if o == nil {
		return cut
	}
	for _, secret := range o.redactions {
		for length := min(len(secret)-1, len(value)); length > 0; length-- {
			if strings.HasSuffix(value, secret[:length]) {
				cut = min(cut, len(value)-length)
				break
			}
		}
	}
	for {
		previous := cut
		for _, secret := range o.redactions {
			window := value[:min(len(value), cut+len(secret)-1)]
			start := strings.LastIndex(window, secret)
			if start >= 0 && start < cut && start+len(secret) > cut {
				cut = start
			}
		}
		if cut == previous {
			break
		}
	}
	return utf8Prefix(value, cut)
}
