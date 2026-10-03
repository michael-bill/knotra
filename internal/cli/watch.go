package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/michael-bill/knotra/internal/protocol"
)

var errWatchComplete = errors.New("event stream reached terminal run status")

type eventJournal struct {
	EngineID    string           `json:"engineId"`
	PrincipalID string           `json:"principalId"`
	RunID       string           `json:"runId"`
	Cursor      string           `json:"cursor"`
	Events      []protocol.Event `json:"events"`
}

func (s *commandState) watchRun(ctx context.Context, runID, cursor string, fromStart bool) error {
	engine := s.client()
	var info struct {
		EngineID    string `json:"engineId"`
		PrincipalID string `json:"principalId"`
		Protocol    string `json:"protocol"`
	}
	if err := engine.Get(ctx, "/info", &info); err != nil {
		return err
	}
	if info.Protocol != protocol.Version || info.EngineID == "" || info.PrincipalID == "" {
		return fmt.Errorf("engine identity or protocol is incompatible")
	}
	digest := sha256.Sum256([]byte(strings.TrimRight(s.endpoint, "/") + "\x00" + info.EngineID + "\x00" + info.PrincipalID + "\x00" + runID))
	dir := filepath.Join(s.stateDir, "events")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, hex.EncodeToString(digest[:])+".json")
	journal := eventJournal{
		EngineID:    info.EngineID,
		PrincipalID: info.PrincipalID,
		RunID:       runID,
		Events:      []protocol.Event{},
	}
	if cursor == "" && !fromStart {
		data, err := os.ReadFile(path)
		if err == nil {
			if err = json.Unmarshal(data, &journal); err != nil {
				return fmt.Errorf("invalid local event journal: %w", err)
			}
			if journal.EngineID != info.EngineID || journal.PrincipalID != info.PrincipalID || journal.RunID != runID {
				return fmt.Errorf("local event journal identity mismatch")
			}
			cursor = journal.Cursor
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	var lastEvent atomic.Int64
	lastEvent.Store(time.Now().UnixNano())
	go func() {
		done <- engine.Watch(watchCtx, runID, cursor, func(event protocol.Event) error {
			if journal.Cursor == event.ID {
				return nil
			}
			journal.Cursor = event.ID
			journal.Events = append(journal.Events, event)
			if len(journal.Events) > 1000 {
				journal.Events = journal.Events[len(journal.Events)-1000:]
			}
			data, err := json.Marshal(journal)
			if err != nil {
				return err
			}

			for len(data) > 8<<20 && len(journal.Events) > 1 {
				journal.Events = journal.Events[len(journal.Events)/2:]
				data, err = json.Marshal(journal)
				if err != nil {
					return err
				}
			}

			if err = atomicFile(path, data, true); err != nil {
				return err
			}
			lastEvent.Store(time.Now().UnixNano())
			if s.json {
				err = s.printJSON(event)
			} else {
				_, err = fmt.Fprintf(
					s.options.Out,
					"%s  %-20s %-20s %s\n",
					event.At.Format(time.RFC3339),
					event.Type,
					event.InstanceID,
					safeText(event.Message),
				)
			}
			if err != nil {
				return err
			}
			if event.InstanceID == "" && protocol.Terminal(event.Message) {
				return errWatchComplete
			}
			return nil
		})
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			if errors.Is(err, errWatchComplete) {
				return nil
			}
			return err
		case <-ctx.Done():
			cancel()
			<-done
			return ctx.Err()
		case <-ticker.C:
			if time.Since(time.Unix(0, lastEvent.Load())) < 2*time.Second {
				continue
			}
			run, err := s.getRun(watchCtx, runID)
			if err != nil {
				cancel()
				<-done
				return err
			}
			if protocol.Terminal(run.Status) {
				cancel()
				err = <-done
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errWatchComplete) {
					return err
				}
				return nil
			}
		}
	}
}
