package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
)

const maxObservationBytes = 64 << 10
const maxContextBytes = 32 << 10

// Observe stores activity diagnostics separately from the workflow's idempotent
// projection sequence. The shared run lock makes ID order also commit order:
// a reader cannot advance its cursor past an uncommitted earlier event.
// Observation failures must never be interpreted as permission to retry an effect.
func (s *Store) Observe(ctx context.Context, event protocol.Event) error {
	if event.RunID == "" || event.InstanceID == "" || event.Type == "" {
		return fmt.Errorf("observation requires run, instance and event type")
	}
	event.ID, event.At = "", now()
	event.Message = shortEventText(event.Message)
	data, err := json.Marshal(event.Data)
	if err != nil {
		return err
	}
	if len(data) > maxObservationBytes {
		summary := map[string]any{"truncated": true, "originalBytes": len(data)}
		if fields, ok := event.Data.(map[string]any); ok {
			for _, key := range []string{"step", "name", "model", "valid", "isError", "durationMs", "firstTokenMs", "inputTokens", "outputTokens", "observationIncomplete"} {
				if value, ok := fields[key]; ok {
					if encoded, err := json.Marshal(value); err == nil && len(encoded) <= 512 {
						summary[key] = value
					}
				}
			}
		}
		event.Data = summary
	}
	b, err := raw(event)
	if err != nil {
		return err
	}
	if len(b) > 128<<10 {
		return fmt.Errorf("observation metadata exceeds limit")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, _, err = lockRun(ctx, tx, event.RunID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO knotra_events(run_id,sequence,document) VALUES($1,NULL,$2)", event.RunID, b); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Context previews preserve complete port values and omit only ports that would
// exceed the budget. Artifact descriptors intentionally exclude attempt paths.
func observationContext(values contract.Values) (map[string]any, bool) {
	if values == nil {
		return nil, false
	}
	bounded := contract.Values{}
	truncated, size := false, 0
	for _, key := range sortedKeys(values) {
		value := values[key]
		if len(value.JSON) > 0 {
			decoded, err := contract.DecodeJSON(value.JSON)
			if err != nil || !clientPreviewNumbers(decoded) {
				truncated = true
				continue
			}
		}
		value.Artifacts = append([]contract.Artifact(nil), value.Artifacts...)
		for i := range value.Artifacts {
			value.Artifacts[i].Path = ""
		}
		one := contract.ContextEnvelope(contract.Values{key: value})
		encoded, err := json.Marshal(one)
		if err != nil || size+len(encoded) > maxContextBytes {
			truncated = true
			continue
		}
		size += len(encoded)
		bounded[key] = value
	}
	return contract.ContextEnvelope(bounded), truncated
}

// Internal CEL values may use the full int64 range. A preview must not make an
// otherwise readable run fail the desktop client's lossless-number validation.
// Omit the whole port rather than rounding or changing its declared value type.
func clientPreviewNumbers(value any) bool {
	const maximum = 9007199254740991
	switch value := value.(type) {
	case int64:
		return value >= -maximum && value <= maximum
	case float64:
		return !math.IsNaN(value) && !math.IsInf(value, 0) && (math.Trunc(value) != value || math.Abs(value) <= maximum)
	case map[string]any:
		for _, item := range value {
			if !clientPreviewNumbers(item) {
				return false
			}
		}
	case []any:
		for _, item := range value {
			if !clientPreviewNumbers(item) {
				return false
			}
		}
	}
	return true
}
