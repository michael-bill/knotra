package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, e := s.Store.Run(r.Context(), id); e != nil {
		s.err(w, e)
		return
	}
	cursor := r.Header.Get("Last-Event-ID")
	batch, e := s.Store.Events(r.Context(), id, cursor)
	if e != nil {
		s.err(w, e)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, 500, "UNAVAILABLE", "streaming is unavailable", nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		for _, ev := range batch {
			b, e := json.Marshal(ev)
			if e != nil {
				return
			}
			if _, e = fmt.Fprintf(w, "id: %s\ndata: %s\n\n", ev.ID, b); e != nil {
				return
			}
			cursor = ev.ID
		}

		flusher.Flush()

		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, e = io.WriteString(w, ": heartbeat\n\n"); e != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
		}

		batch, e = s.Store.Events(r.Context(), id, cursor)
		if e != nil {
			return
		}
	}
}
