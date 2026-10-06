package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type chatChunk struct {
	response chatResponse
	err      error
}

// readChatStream accepts Ollama NDJSON as well as a single complete JSON object.
// The journal still stores one complete response, so retries/replay never treat
// visible partial text as a completed model operation.
func readChatStream(
	ctx context.Context,
	body io.ReadCloser,
	started time.Time,
	observation *executionObservation,
	operationID string,
	step int,
) (chatResponse, time.Duration, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = body.Close() }()
	chunks := make(chan chatChunk)
	go func() {
		limited := &io.LimitedReader{R: body, N: maxResponseBytes + 1}
		decoder := json.NewDecoder(limited)
		for {
			var chunk chatChunk
			chunk.err = decoder.Decode(&chunk.response)
			if limited.N <= 0 {
				chunk.err = fmt.Errorf("model response exceeds %d bytes", maxResponseBytes)
			}
			select {
			case chunks <- chunk:
			case <-streamCtx.Done():
				return
			}
			if chunk.err != nil {
				return
			}
		}
	}()
	var result chatResponse
	var content, thinking strings.Builder
	var pending strings.Builder
	var firstToken time.Duration
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	flush := func(final bool) {
		text := pending.String()
		cut := len(text)
		if !final {
			cut = observation.safePrefix(text)
		}
		if cut == 0 {
			return
		}
		observation.emit(ctx, "model.delta", operationID, map[string]any{"step": step, "text": text[:cut]})
		rest := strings.Clone(text[cut:])
		pending.Reset()
		_, _ = pending.WriteString(rest)
	}
	for {
		select {
		case <-ctx.Done():
			return chatResponse{}, firstToken, ctx.Err()
		case <-ticker.C:
			flush(false)
		case chunk := <-chunks:
			if chunk.err != nil {
				if !errors.Is(chunk.err, io.EOF) {
					var syntax *json.SyntaxError
					var invalidType *json.UnmarshalTypeError
					if errors.As(chunk.err, &syntax) || errors.As(chunk.err, &invalidType) || errors.Is(chunk.err, io.ErrUnexpectedEOF) {
						return chatResponse{}, firstToken, failure("OUTPUT_INVALID", fmt.Errorf("invalid model response stream"))
					}
					return chatResponse{}, firstToken, chunk.err
				}
				if !result.Done {
					return chatResponse{}, firstToken, failure("OUTPUT_INVALID", fmt.Errorf("model response stream ended before completion"))
				}
				flush(true)
				result.Message.Content = content.String()
				result.Message.Thinking = thinking.String()
				return result, firstToken, nil
			}
			if result.Done {
				return chatResponse{}, firstToken, failure("OUTPUT_INVALID", fmt.Errorf("model returned data after completion"))
			}
			r := chunk.response
			if r.Error != "" {
				return chatResponse{}, firstToken, failure("OUTPUT_INVALID", fmt.Errorf("model returned an error"))
			}
			if r.Message.Role != "" {
				result.Message.Role = r.Message.Role
			}
			_, _ = content.WriteString(r.Message.Content)
			_, _ = thinking.WriteString(r.Message.Thinking)
			result.Message.ToolCalls = append(result.Message.ToolCalls, r.Message.ToolCalls...)
			if r.Message.Content != "" || len(r.Message.ToolCalls) > 0 {
				first := firstToken == 0
				if first {
					firstToken = time.Since(started)
				}
				if observation != nil && r.Message.Content != "" {
					_, _ = pending.WriteString(r.Message.Content)
					if first || pending.Len() >= 1024 {
						flush(false)
					}
				}
			}
			if r.Done {
				result.Done, result.DoneReason = r.Done, r.DoneReason
				result.PromptEvalCount, result.EvalCount = r.PromptEvalCount, r.EvalCount
			}
		}
	}
}
