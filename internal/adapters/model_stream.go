package adapters

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type modelEvent struct {
	data []byte
	err  error
}

type modelEventHandler func([]byte) (string, *chatResponse, error)

func invalidModelResponse() error {
	return failure("OUTPUT_INVALID", fmt.Errorf("invalid or incomplete model response"))
}

func readModelJSON(body io.Reader, result any) error {
	return readBoundedModelJSON(body, maxResponseBytes, result)
}

func readBoundedModelJSON(body io.Reader, limit int64, result any) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit || json.Unmarshal(data, result) != nil {
		return invalidModelResponse()
	}
	return nil
}

func isEventStream(res *http.Response) bool {
	return strings.HasPrefix(strings.ToLower(res.Header.Get("Content-Type")), "text/event-stream")
}

// SSE frames may span lines and network reads. Parsing has a bounded lifetime and
// total byte budget; only a provider's terminal event can complete the operation.
func readModelEvents(ctx context.Context, body io.ReadCloser, started time.Time, observation *executionObservation, id string, step int, handle modelEventHandler) (chatResponse, time.Duration, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = body.Close() }()
	events := make(chan modelEvent)
	go func() {
		reader := &io.LimitedReader{R: body, N: maxResponseBytes + 1}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), maxResponseBytes)
		var data strings.Builder
		send := func(event modelEvent) bool {
			select {
			case events <- event:
				return true
			case <-streamCtx.Done():
				return false
			}
		}
		for scanner.Scan() {
			if reader.N <= 0 {
				send(modelEvent{err: invalidModelResponse()})
				return
			}
			line := scanner.Text()
			if line == "" {
				if data.Len() > 0 {
					if !send(modelEvent{data: []byte(strings.TrimSuffix(data.String(), "\n"))}) {
						return
					}
					data.Reset()
				}
			} else if strings.HasPrefix(line, "data:") {
				_, _ = data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				_ = data.WriteByte('\n')
			}
		}
		if scanner.Err() != nil {
			err := scanner.Err()
			if errors.Is(err, bufio.ErrTooLong) || reader.N <= 0 {
				err = invalidModelResponse()
			}
			send(modelEvent{err: err})
			return
		}
		if data.Len() > 0 && !send(modelEvent{data: []byte(strings.TrimSuffix(data.String(), "\n"))}) {
			return
		}
		send(modelEvent{err: io.EOF})
	}()
	var firstToken time.Duration
	var pending strings.Builder
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	flush := func(final bool) {
		if observation == nil {
			pending.Reset()
			return
		}
		text := pending.String()
		cut := len(text)
		if !final {
			cut = observation.safePrefix(text)
		}
		if cut == 0 {
			return
		}
		observation.emit(ctx, "model.delta", id, map[string]any{"step": step, "text": text[:cut]})
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
		case event := <-events:
			if event.err != nil {
				if errors.Is(event.err, io.EOF) {
					return chatResponse{}, firstToken, invalidModelResponse()
				}
				return chatResponse{}, firstToken, event.err
			}
			text, result, err := handle(event.data)
			if err != nil {
				return chatResponse{}, firstToken, err
			}
			if text != "" {
				first := firstToken == 0
				if first {
					firstToken = time.Since(started)
				}
				_, _ = pending.WriteString(text)
				if first || pending.Len() >= 1024 {
					flush(false)
				}
			}
			if result != nil {
				flush(true)
				return *result, firstToken, nil
			}
		}
	}
}
