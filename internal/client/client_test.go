package client

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

	"github.com/michael-bill/knotra/internal/protocol"
)

func TestCommandJournalConcurrentAndIdentity(t *testing.T) {
	var calls atomic.Int32
	identity := "engine-a"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			fmt.Fprintf(w, `{"protocol":%q,"engineId":%q,"principalId":"operator"}`, protocol.Version, identity)
			return
		}
		calls.Add(1)
		if r.Header.Get("Idempotency-Key") != "one" {
			t.Error("missing stable key")
		}
		fmt.Fprint(w, `{"run":{"id":"run","status":"pending"}}`)
	}))
	defer srv.Close()
	c := Client{BaseURL: srv.URL, StateDir: t.TempDir()}
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out any
			if err := c.Command(context.Background(), "/runs", map[string]any{"value": 1}, "one", &out); err != nil {
				t.Error(err)
			}
		}()
	}

	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("sent %d calls for one local command", calls.Load())
	}
	var out any
	if err := c.Command(context.Background(), "/runs", map[string]any{"value": 2}, "one", &out); err == nil {
		t.Fatal("changed payload reused ID")
	}
	identity = "engine-b"
	if err := c.Retry(context.Background(), "one", &out); err == nil {
		t.Fatal("changed engine reused receipt")
	}
}

func TestAmbiguousResponseKeepsCommandForExplicitRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			fmt.Fprintf(w, `{"protocol":%q,"engineId":"e","principalId":"p"}`, protocol.Version)
			return
		}
		calls++
		if calls == 1 {
			fmt.Fprint(w, `{`)
			return
		}
		fmt.Fprint(w, `{"run":{"id":"run","status":"pending"}}`)
	}))
	defer srv.Close()
	c := Client{BaseURL: srv.URL, StateDir: t.TempDir()}
	var out any
	if err := c.Command(context.Background(), "/runs", map[string]any{}, "retry", &out); err == nil {
		t.Fatal("accepted truncated response")
	}
	entries, err := c.Commands()
	if err != nil || len(entries) != 1 || entries[0].Status != "pending" {
		t.Fatalf("lost pending receipt: %v %v", entries, err)
	}
	if err = c.Retry(context.Background(), "retry", &out); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestRejectedReceiptIsStable(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			fmt.Fprintf(w, `{"protocol":%q,"engineId":"e","principalId":"p"}`, protocol.Version)
			return
		}
		calls.Add(1)
		w.WriteHeader(409)
		fmt.Fprint(w, `{"code":"CLOSED","message":"closed","diagnostics":[]}`)
	}))
	defer srv.Close()
	c := Client{BaseURL: srv.URL, StateDir: t.TempDir()}
	var out any

	for i := 0; i < 2; i++ {
		err := c.Command(context.Background(), "/runs", json.RawMessage(`{}`), "reject", &out)
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != 409 {
			t.Fatal(err)
		}
	}

	if calls.Load() != 1 {
		t.Fatal("reissued a definitive rejection")
	}
}

func TestSSEChecksIdentityAndFrameBounds(t *testing.T) {
	for _, s := range []string{
		"id: a\ndata: {\"id\":\"b\",\"runId\":\"r\"}\n\n",
		"id: a\ndata: {\"id\":\"a\",\"runId\":\"other\"}\n\n",
		"id: \x00\ndata: {\"id\":\"\\u0000\",\"runId\":\"r\"}\n\n",
		strings.Repeat("x", (256<<10)+1),
	} {
		if err := scanEvents(strings.NewReader(s), "r", func(protocol.Event) error { t.Fatal("invalid event emitted"); return nil }); err == nil {
			t.Fatal("invalid stream accepted")
		}
	}
}

func TestCustomHTTPClientStillRejectsRedirects(t *testing.T) {
	var followed atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		followed.Add(1)
		fmt.Fprint(w, `{}`)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, destination.URL, http.StatusFound)
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTP: server.Client()}
	var output any
	if err := client.Get(context.Background(), "/info", &output); err == nil || followed.Load() != 0 {
		t.Fatalf("redirect was followed: err=%v requests=%d", err, followed.Load())
	}
}

func TestInvalidReceiptRemainsPendingUntilExplicitReconciliation(t *testing.T) {
	cases := []struct {
		name, route, body, valid string
		status                   int
	}{
		{"empty success", "/runs", `{}`, `{"run":{"id":"run","status":"pending"}}`, 201},
		{
			"missing run identity",
			"/runs",
			`{"run":{"status":"pending"}}`,
			`{"run":{"id":"run","status":"pending"}}`,
			201,
		},
		{
			"unknown run status",
			"/runs",
			`{"run":{"id":"run","status":"invented"}}`,
			`{"run":{"id":"run","status":"pending"}}`,
			201,
		},
		{"empty definition", "/definitions", `{"definition":{}}`, `{"definition":{"id":"definition"}}`, 201},
		{
			"unaccepted action",
			"/runs/run/cancel",
			`{"accepted":false,"runId":"run"}`,
			`{"accepted":true,"runId":"run"}`,
			202,
		},
		{
			"wrong target",
			"/requests/request/response",
			`{"accepted":true,"requestId":"other"}`,
			`{"accepted":true,"requestId":"request"}`,
			200,
		},
		{"empty rejection", "/runs/run/cancel", `{}`, `{"accepted":true,"runId":"run"}`, 409},
		{
			"malformed rejection",
			"/runs/run/cancel",
			`{"code":"CLOSED","message":"closed","diagnostics":null}`,
			`{"accepted":true,"runId":"run"}`,
			409,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/v1/info" {
					fmt.Fprintf(w, `{"protocol":%q,"engineId":"e","principalId":"p"}`, protocol.Version)
					return
				}
				if req.Header.Get("Idempotency-Key") != "stable" {
					t.Error("reconciliation changed operation ID")
				}
				if calls.Add(1) == 1 {
					w.WriteHeader(test.status)
					fmt.Fprint(w, test.body)
				} else {
					fmt.Fprint(w, test.valid)
				}
			}))
			defer server.Close()
			client := &Client{BaseURL: server.URL, StateDir: t.TempDir()}
			var output any
			if err := client.Command(context.Background(), test.route, map[string]any{}, "stable", &output); err == nil {
				t.Fatal("invalid receipt accepted")
			}
			receipt, err := client.load("stable")
			if err != nil || receipt.Status != "pending" {
				t.Fatalf("pending command lost: receipt=%+v err=%v", receipt, err)
			}
			if err := client.Retry(context.Background(), "stable", &output); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("reconciliation calls=%d", calls.Load())
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type brokenReader struct{ err error }

func (r brokenReader) Read([]byte) (int, error) { return 0, r.err }

func TestWatchReconnectsInterruptedBodyFromCommittedCursor(t *testing.T) {
	var calls int
	stop := errors.New("observation complete")
	frame := func(id string) string {
		return fmt.Sprintf("id: %s\ndata: {\"id\":%q,\"runId\":\"run\"}\n\n", id, id)
	}
	client := &Client{BaseURL: "http://localhost", HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		var reader io.Reader
		if calls == 1 {
			if cursor := req.Header.Get("Last-Event-ID"); cursor != "" {
				t.Fatal(cursor)
			}
			reader = io.MultiReader(strings.NewReader(frame("one")+"id: partial\ndata: {"), brokenReader{err: io.ErrUnexpectedEOF})
		} else {
			if cursor := req.Header.Get("Last-Event-ID"); cursor != "one" {
				t.Fatalf("cursor advanced before delivery: %s", cursor)
			}
			reader = strings.NewReader(frame("one") + frame("two"))
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(reader),
		}, nil
	})}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ids []string
	err := client.Watch(ctx, "run", "", func(ev protocol.Event) error {
		ids = append(ids, ev.ID)
		if ev.ID == "two" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || calls != 2 || strings.Join(ids, ",") != "one,two" {
		t.Fatalf("bad reconnect: calls=%d events=%v err=%v", calls, ids, err)
	}
}

func TestWatchDoesNotReconnectProtocolOrCallbackErrors(t *testing.T) {
	for _, test := range []struct {
		name, contentType, body string
		callbackErr             error
	}{
		{"wrong content type", "application/json", `{}`, nil},
		{
			"wrong identity",
			"text/event-stream",
			"id: one\ndata: {\"id\":\"one\",\"runId\":\"other\"}\n\n",
			nil,
		},
		{"oversize frame", "text/event-stream", strings.Repeat("x", 256<<10), nil},
		{
			"callback EOF",
			"text/event-stream",
			"id: one\ndata: {\"id\":\"one\",\"runId\":\"run\"}\n\n",
			io.EOF,
		},
		{
			"callback network error",
			"text/event-stream",
			"id: one\ndata: {\"id\":\"one\",\"runId\":\"run\"}\n\n",
			io.ErrUnexpectedEOF,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &Client{BaseURL: "http://localhost", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": {test.contentType}},
					Body:       io.NopCloser(strings.NewReader(test.body)),
				}, nil
			})}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := client.Watch(ctx, "run", "", func(protocol.Event) error { return test.callbackErr })
			if err == nil || calls != 1 || (test.callbackErr != nil && !errors.Is(err, test.callbackErr)) {
				t.Fatalf("unexpected retry: calls=%d err=%v", calls, err)
			}
		})
	}
}
