package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/engine"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func TestStrictBodiesAndBearerAuthentication(t *testing.T) {
	srv := Server{Token: "secret", Store: &store.Store{EngineID: "test"}}

	for _, header := range []string{"", "secret", "Basic secret", "Bearer wrong"} {
		r := httptest.NewRequest("GET", "/v1/info", nil)
		r.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal(header, w.Code)
		}
	}

	for _, body := range []string{`null`, `[]`, `{"a":1,"a":2}`, `{} {}`, `{"unexpected":true}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		var v struct{}
		if _, err := readBody(w, r, &v); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	// A package/upload body is allowed to exceed the 16MiB data-port limit.
	b := []byte(`{"source":"` + strings.Repeat("x", 17<<20) + `"}`)
	var v struct {
		Source string `json:"source"`
	}
	if _, err := readBody(httptest.NewRecorder(), httptest.NewRequest("POST", "/", bytes.NewReader(b)), &v); err != nil {
		t.Fatal(err)
	}
}

func apiStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("KNOTRA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL for API persistence tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "knotra_api_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close(ctx)
	})
	return db
}

func TestDurableAPICommandsAndHumanCancellation(t *testing.T) {
	db := apiStore(t)
	profile, ds := contract.ParseProfile([]byte(`apiVersion: knotra/v1
kind: EngineProfile
metadata: {name: test}
spec:
  limits: {timeout: 10m, maxConcurrentNodes: 4, maxNodeInstances: 100, maxModelCalls: 10, maxToolCalls: 20}
`))
	if contract.HasErrors(ds) {
		t.Fatal(ds)
	}
	srv := Server{
		Store:     db,
		Artifacts: store.Artifacts{Root: t.TempDir(), Store: db},
		Profiles:  map[string]contract.Profile{"test": profile},
	}
	handler := srv.Handler()
	call := func(route, key string, payload any) (int, []byte) {
		t.Helper()
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/v1"+route, bytes.NewReader(b))
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code, w.Body.Bytes()
	}
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Files: []contract.File{}, Source: `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: approve}
spec:
  nodes:
    approval:
      type: human
      human: {prompt: {text: Approve}}
      outputs:
        ok: {schema: {type: boolean}}
  outputs:
    ok:
      schema: {type: boolean}
      bind: {from: nodes.approval.outputs.ok}
`}
	pkg.Files = []contract.File{{Path: pkg.Entrypoint, Content: []byte(pkg.Source)}}
	code, b := call("/definitions", "def", map[string]any{"package": pkg})
	if code != 201 {
		t.Fatalf("define %d %s", code, b)
	}
	var definition struct {
		Definition protocol.Definition `json:"definition"`
	}
	if err := json.Unmarshal(b, &definition); err != nil {
		t.Fatal(err)
	}
	if definition.Definition.Title == "" || definition.Definition.Title != definition.Definition.Name {
		t.Fatal("a definition without a title must use its pipeline name")
	}
	payload := map[string]any{
		"definitionId": definition.Definition.ID,
		"profile":      "test",
		"inputs":       map[string]any{},
		"artifacts":    map[string]any{},
	}
	code, b = call("/runs", "run", payload)
	if code != 201 {
		t.Fatalf("start %d %s", code, b)
	}
	var result struct {
		Run protocol.Run `json:"run"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	code, again := call("/runs", "run", payload)
	if code != 201 || !bytes.Equal(b, again) {
		t.Fatal("run receipt changed")
	}
	payload["profile"] = "other"
	code, _ = call("/runs", "run", payload)
	if code != 409 {
		t.Fatal("changed command reused key", code)
	}
	id := result.Run.ID
	values := map[string]contract.Port{"ok": {Schema: json.RawMessage(`{"type":"boolean"}`)}}
	req := engine.Request{
		RunID:      id,
		ID:         "human1",
		InstanceID: "n1",
		Kind:       "human",
		Status:     "open",
		Outputs:    values,
		Deadline:   time.Now().Add(time.Minute),
	}
	if err := db.SaveRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	code, b = call("/requests/human1/response", "bad-answer", map[string]any{"outputs": map[string]any{"ok": "bad"}})
	if code != 422 {
		t.Fatalf("invalid answer %d %s", code, b)
	}
	code, b = call("/requests/human1/response", "answer", map[string]any{"outputs": map[string]any{"ok": true}})
	if code != 200 {
		t.Fatalf("answer %d %s", code, b)
	}
	code, again = call("/requests/human1/response", "answer", map[string]any{"outputs": map[string]any{"ok": true}})
	if code != 200 || !bytes.Equal(b, again) {
		t.Fatal("answer receipt changed")
	}
	req.ID = "human2"
	if err := db.SaveRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	code, b = call("/runs/"+id+"/cancel", "cancel", map[string]any{})
	if code != 202 {
		t.Fatalf("cancel %d %s", code, b)
	}
	code, b = call("/requests/human2/response", "too-late", map[string]any{"outputs": map[string]any{"ok": true}})
	if code != 409 {
		t.Fatalf("postcancel answer %d %s", code, b)
	}
	signal, err := db.Answer(context.Background(), engine.AnswerRequest{RunID: id, RequestID: "human1", CloseIfAbsent: "cancelled"})
	if err != nil || signal == nil {
		t.Fatalf("accepted answer lost on cancel: %v %v", signal, err)
	}
	code, b = call(
		"/artifacts",
		"artifact",
		map[string]any{"name": "file.txt", "mediaType": "text/plain", "content": []byte("durable file")},
	)
	if code != 201 {
		t.Fatalf("upload %d %s", code, b)
	}
	var artifact struct {
		Artifact contract.Artifact `json:"artifact"`
	}
	if err = json.Unmarshal(b, &artifact); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/artifacts/"+artifact.Artifact.ID+"/content", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "durable file" {
		t.Fatal(w.Code, w.Body.String())
	}
	// A cursor from another run must never silently skip this run's events.
	if err = db.Project(
		context.Background(),
		engine.Projection{RunID: id, Sequence: 1, Time: time.Now(), Kind: "run", Status: "running"},
	); err != nil {
		t.Fatal(err)
	}
	events, err := db.Events(context.Background(), id, "")
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	r = httptest.NewRequest("GET", "/v1/runs/"+id+"/events", nil)
	r.Header.Set("Last-Event-ID", "999999")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal(fmt.Sprint(w.Code, w.Body.String()))
	}
}

func TestListPageHasCursorAtByteBoundary(t *testing.T) {
	type item struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	}
	entries := []item{{ID: "a", Data: strings.Repeat("a", 17<<20)}, {ID: "b", Data: strings.Repeat("b", 17<<20)}}
	result := page(entries, func(v item) string { return v.ID })
	if len(result.Items) != 1 || result.NextCursor == nil || *result.NextCursor != "a" {
		t.Fatal("oversized list has no continuation cursor")
	}
}

func TestProfilesUseNameWhenTitleOmitted(t *testing.T) {
	srv := Server{Profiles: map[string]contract.Profile{
		"local": {Metadata: contract.Metadata{Name: "Local engine"}},
	}}
	response := httptest.NewRecorder()
	srv.profiles(response, httptest.NewRequest(http.MethodGet, "/v1/profiles", nil))
	var catalog struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Items) != 1 || catalog.Items[0].Title != "Local engine" {
		t.Fatalf("profile without an explicit title must display its name: %s", response.Body.String())
	}
}

func TestArtifactUploadRequiresBase64StringContent(t *testing.T) {
	db := apiStore(t)
	srv := Server{Store: db, Artifacts: store.Artifacts{Root: t.TempDir(), Store: db}}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing", `{"name":"empty.txt","mediaType":"text/plain"}`, 422},
		{"null", `{"name":"empty.txt","mediaType":"text/plain","content":null}`, 422},
		{"array", `{"name":"empty.txt","mediaType":"text/plain","content":[65]}`, 400},
		{"invalid-base64", `{"name":"empty.txt","mediaType":"text/plain","content":"!"}`, 400},
		{"empty", `{"name":"empty.txt","mediaType":"text/plain","content":""}`, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", strings.NewReader(tc.body))
			r.Header.Set("Idempotency-Key", tc.name)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("upload status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == 201 {
				var result struct {
					Artifact contract.Artifact `json:"artifact"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				content, err := srv.Artifacts.Get(context.Background(), result.Artifact.ID)
				if err != nil || len(content) != 0 || result.Artifact.Size != 0 {
					t.Fatalf("explicit empty file did not round-trip: size=%d content=%q error=%v", result.Artifact.Size, content, err)
				}
			} else {
				var result protocol.Error
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Code != "INPUT_INVALID" {
					t.Fatalf("invalid content must produce a structured input error: %s", w.Body.String())
				}
			}
		})
	}
	artifacts, err := db.Artifacts(context.Background(), "")
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("invalid uploads registered artifacts: count=%d error=%v", len(artifacts), err)
	}
}
