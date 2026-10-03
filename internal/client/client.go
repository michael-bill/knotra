// Package client implements the engine wire protocol and durable command receipts.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/michael-bill/knotra/internal/protocol"
)

type Client struct {
	BaseURL, Token, StateDir string
	HTTP                     *http.Client
}
type Command struct {
	ID          string          `json:"id"`
	Endpoint    string          `json:"endpoint"`
	EngineID    string          `json:"engineId"`
	PrincipalID string          `json:"principalId"`
	Route       string          `json:"route"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Response    json.RawMessage `json:"response,omitempty"`
	HTTPStatus  int             `json:"httpStatus,omitempty"`
}
type HTTPError struct {
	Status int
	Body   protocol.Error
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Body.Code, e.Body.Message)
}
func (c *Client) validate() error {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("endpoint must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("endpoint must use HTTP(S)")
	}
	ip := net.ParseIP(u.Hostname())
	local := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if !local && (u.Scheme != "https" || c.Token == "") {
		return errors.New("remote engines require HTTPS and KNOTRA_TOKEN")
	}
	return nil
}
func (c *Client) httpClient() *http.Client {
	client := &http.Client{}
	if c.HTTP != nil {
		*client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}
func (c *Client) request(ctx context.Context, method, route string, body []byte, key string) (*http.Response, error) {
	if e := c.validate(); e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+"/v1"+route, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return c.httpClient().Do(req)
}
func response(res *http.Response, out any) error {
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, (256<<20)+1))
	if e != nil {
		return e
	}
	if len(b) > 256<<20 {
		return errors.New("response exceeds 256 MiB")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var v protocol.Error
		if json.Unmarshal(b, &v) != nil {
			v.Code = "INVALID_RESPONSE"
			v.Message = "server returned a non-JSON error"
		}
		return &HTTPError{Status: res.StatusCode, Body: v}
	}
	if e = json.Unmarshal(b, out); e != nil {
		return fmt.Errorf("invalid engine response: %w", e)
	}
	return nil
}
func (c *Client) Get(ctx context.Context, route string, out any) error {
	res, e := c.request(ctx, http.MethodGet, route, nil, "")
	if e != nil {
		return e
	}
	return response(res, out)
}
func (c *Client) Validate(ctx context.Context, payload any, out any) error {
	b, e := json.Marshal(payload)
	if e != nil {
		return e
	}
	res, e := c.request(ctx, http.MethodPost, "/packages/validate", b, "")
	if e != nil {
		return e
	}
	return response(res, out)
}
func (c *Client) commandDir() string {
	sum := sha256.Sum256([]byte(strings.TrimRight(c.BaseURL, "/")))
	return filepath.Join(c.StateDir, "commands", hex.EncodeToString(sum[:16]))
}
func (c *Client) save(v Command) error {
	dir := c.commandDir()
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".command-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(name, filepath.Join(dir, v.ID+".json")); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func validID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func (c *Client) Command(ctx context.Context, route string, payload any, id string, out any) error {
	if id == "" {
		id = uuid.NewString()
	}
	if !validID(id) {
		return errors.New("invalid operation ID")
	}
	b, e := json.Marshal(payload)
	if e != nil {
		return e
	}
	unlock, e := c.lockCommand(ctx, id)
	if e != nil {
		return e
	}
	defer unlock()
	var info struct {
		EngineID    string `json:"engineId"`
		PrincipalID string `json:"principalId"`
		Protocol    string `json:"protocol"`
	}
	if e = c.Get(ctx, "/info", &info); e != nil {
		return e
	}
	if info.Protocol != protocol.Version || info.EngineID == "" || info.PrincipalID == "" {
		return errors.New("incompatible engine identity or protocol")
	}
	v := Command{ID: id, Endpoint: c.BaseURL, EngineID: info.EngineID, PrincipalID: info.PrincipalID, Route: route, Payload: b, Status: "pending"}
	old, e := c.load(id)
	if e == nil {
		if old.Route != route || !bytes.Equal(old.Payload, b) || old.EngineID != info.EngineID || old.PrincipalID != info.PrincipalID {
			return errors.New("operation ID belongs to different command or engine identity")
		}
		v = old
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if v.Status == "completed" {
		if e := validateReceipt(v.Route, v.Response); e != nil {
			return fmt.Errorf("stored receipt is invalid: %w", e)
		}
		return json.Unmarshal(v.Response, out)
	}
	if v.Status == "rejected" {
		var body protocol.Error
		if e := validateErrorReceipt(v.Response, &body); e != nil {
			return fmt.Errorf("stored rejection is invalid: %w", e)
		}
		return &HTTPError{Status: v.HTTPStatus, Body: body}
	}
	if e = c.save(v); e != nil {
		return e
	}
	res, e := c.request(ctx, http.MethodPost, route, b, id)
	if e != nil {
		return fmt.Errorf("operation %s is pending; reconcile with operations retry: %w", id, e)
	}
	defer res.Body.Close()
	data, e := io.ReadAll(io.LimitReader(res.Body, (256<<20)+1))
	if e != nil || len(data) > 256<<20 {
		return fmt.Errorf("operation %s remains pending after incomplete response", id)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return fmt.Errorf("operation %s remains pending after malformed response", id)
	}
	var er protocol.Error
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		if e = validateReceipt(route, data); e == nil {
			e = json.Unmarshal(data, out)
		}
	} else {
		e = validateErrorReceipt(data, &er)
	}
	if e != nil {
		return fmt.Errorf("operation %s remains pending after invalid receipt: %w", id, e)
	}
	v.HTTPStatus = res.StatusCode
	v.Response = data
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		v.Status = "completed"
	} else if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 408 && res.StatusCode != 429 {
		v.Status = "rejected"
	}
	if e = c.save(v); e != nil {
		return e
	}
	if v.Status == "completed" {
		return nil
	}
	return &HTTPError{Status: res.StatusCode, Body: er}
}

func (c *Client) load(id string) (Command, error) {
	var v Command
	if !validID(id) {
		return v, errors.New("invalid operation ID")
	}
	b, e := os.ReadFile(filepath.Join(c.commandDir(), id+".json"))
	if e != nil {
		return v, e
	}
	e = json.Unmarshal(b, &v)
	if e == nil {
		var compact bytes.Buffer
		e = json.Compact(&compact, v.Payload)
		v.Payload = compact.Bytes()
	}
	return v, e
}
func (c *Client) Retry(ctx context.Context, id string, out any) error {
	v, e := c.load(id)
	if e != nil {
		return e
	}
	return c.Command(ctx, v.Route, v.Payload, id, out)
}
func (c *Client) Commands() ([]Command, error) {
	entries, e := os.ReadDir(c.commandDir())
	if errors.Is(e, os.ErrNotExist) {
		return []Command{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []Command{}
	for _, f := range entries {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		v, e := c.load(strings.TrimSuffix(f.Name(), ".json"))
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func (c *Client) Bytes(ctx context.Context, route string) ([]byte, error) {
	res, e := c.request(ctx, http.MethodGet, route, nil, "")
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("artifact download: HTTP %d", res.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (64<<20)+1))
	if len(b) > 64<<20 {
		return nil, errors.New("artifact exceeds 64 MiB")
	}
	return b, e
}

// Watch reconnects only the read stream. Each callback succeeds before its cursor
// advances; mutations are never automatically resubmitted.
func (c *Client) Watch(ctx context.Context, runID, cursor string, emit func(protocol.Event) error) error {
	for {
		req, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.BaseURL, "/")+"/v1/runs/"+url.PathEscape(runID)+"/events", nil)
		if e != nil {
			return e
		}
		if e = c.validate(); e != nil {
			return e
		}
		if cursor != "" {
			req.Header.Set("Last-Event-ID", cursor)
		}
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		res, e := c.httpClient().Do(req)
		if e == nil {
			if res.StatusCode != 200 {
				res.Body.Close()
				return fmt.Errorf("event stream: HTTP %d", res.StatusCode)
			}
			mediaType, _, mediaErr := mime.ParseMediaType(res.Header.Get("Content-Type"))
			if mediaErr != nil || mediaType != "text/event-stream" {
				res.Body.Close()
				return errors.New("event stream has invalid content type")
			}
			var callbackErr error
			e = scanEvents(res.Body, runID, func(v protocol.Event) error {
				if v.ID == cursor {
					return nil
				}
				if e := emit(v); e != nil {
					callbackErr = e
					return e
				}
				cursor = v.ID
				return nil
			})
			res.Body.Close()
			if callbackErr != nil {
				return callbackErr
			}
			var readErr *eventReadError
			if e != nil && !errors.Is(e, io.EOF) && !errors.As(e, &readErr) {
				return e
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type eventReadError struct{ err error }

func (e *eventReadError) Error() string { return e.err.Error() }
func (e *eventReadError) Unwrap() error { return e.err }

func scanEvents(r io.Reader, runID string, emit func(protocol.Event) error) error {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 256<<10)
	var id string
	var data []string
	size := 0
	for scan.Scan() {
		line := scan.Text()
		size += len(line) + 1
		if size > 256<<10 {
			return errors.New("event frame exceeds 256 KiB")
		}
		if line == "" {
			if len(data) > 0 {
				var ev protocol.Event
				if e := json.Unmarshal([]byte(strings.Join(data, "\n")), &ev); e != nil {
					return e
				}
				if !opaqueID(id) || ev.ID != id || ev.RunID != runID {
					return errors.New("event identity mismatch")
				}
				if e := emit(ev); e != nil {
					return e
				}
			}
			id = ""
			data = nil
			size = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			id = value
		case "data":
			data = append(data, value)
		}
	}
	if e := scan.Err(); e != nil {
		if errors.Is(e, bufio.ErrTooLong) {
			return errors.New("event frame exceeds 256 KiB")
		}
		return &eventReadError{err: e}
	}
	return io.EOF
}
