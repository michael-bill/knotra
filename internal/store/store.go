// Package store persists command receipts, public projections and authoritative
// execution records. Queue delivery is composed by the application bridge.
package store

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store/db"
)

//go:embed schema.sql
var schemaSQL string

var ErrNotFound = errors.New("not found")

var ErrConflict = errors.New("operation conflicts with existing state")

type Store struct {
	Pool     *pgxpool.Pool
	EngineID string
}

func Open(ctx context.Context, dsn string) (_ *Store, err error) {
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = db.New(tx).LockStoreSchema(ctx); err != nil {
		return nil, err
	}
	initialized, err := db.New(tx).SchemaInitialized(ctx)
	if err != nil {
		return nil, err
	}
	if !initialized {
		if _, err = tx.Exec(ctx, schemaSQL); err != nil {
			return nil, fmt.Errorf("initialize database: %w", err)
		}
	}
	if _, err = db.New(tx).InsertEngineID(ctx, uuid.NewString()); err != nil {
		return nil, err
	}
	s := &Store{Pool: p}
	s.EngineID, err = db.New(tx).ReadEngineID(ctx)
	if err != nil {
		return nil, err
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.Pool.Close() }

func raw(v any) ([]byte, error) { return json.Marshal(v) }

func classify(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Command atomically binds an operation ID to content, effect and receipt.
// The action must only mutate this transaction; external delivery uses the outbox.
func (s *Store) Command(ctx context.Context, principal, key, route string, payload []byte, action func(pgx.Tx) (int, any, error)) (int, []byte, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = db.New(tx).LockTransactionKey(ctx, principal+":"+key)
	if err != nil {
		return 0, nil, err
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])

	var status int
	var response []byte
	storedCommandReceipt, queryErr2 := db.New(tx).ReadCommandReceipt(ctx, db.ReadCommandReceiptParams{Principal: principal, ID: key})
	err = queryErr2
	if err == nil {
		status = storedCommandReceipt.Status
		response = storedCommandReceipt.Response
	}
	if err == nil {
		if storedCommandReceipt.Route != route || storedCommandReceipt.Digest != digest {
			return 0, nil, ErrConflict
		}
		return status, response, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, err
	}
	status, value, err := action(tx)
	if err != nil {
		return 0, nil, err
	}
	response, err = raw(value)
	if err != nil {
		return 0, nil, err
	}
	_, err = db.New(tx).InsertCommandReceipt(ctx, db.InsertCommandReceiptParams{
		Principal: principal,
		ID:        key,
		Route:     route,
		Digest:    digest,
		Status:    status,
		Response:  response,
	})
	if err != nil {
		return 0, nil, err
	}
	return status, response, tx.Commit(ctx)
}

func PutDefinition(ctx context.Context, tx pgx.Tx, d protocol.Definition) (protocol.Definition, error) {
	b, e := raw(d)
	if e != nil {
		return d, e
	}
	var saved []byte
	saved, e = db.New(tx).PutDefinition(ctx, db.PutDefinitionParams{ID: d.ID, Digest: d.PackageDigest, Document: b})
	if e == nil {
		e = json.Unmarshal(saved, &d)
	}
	return d, e
}

// Querier is implemented by a pool and a transaction. Command handlers must use
// their transaction for reads too, so concurrent admissions cannot exhaust a pool
// while holding every connection and waiting for an additional one.
type Querier = db.DBTX

func ReadDefinition(ctx context.Context, q Querier, id string) (protocol.Definition, error) {
	var d protocol.Definition
	var b []byte
	b, err := db.New(q).ReadDefinition(ctx, id)
	if err == nil {
		err = json.Unmarshal(b, &d)
	}
	return d, classify(err)
}

func (s *Store) Definition(ctx context.Context, id string) (protocol.Definition, error) {
	return ReadDefinition(ctx, s.Pool, id)
}

func (s *Store) Definitions(ctx context.Context, cursor string) ([]protocol.Definition, error) {
	documents, err := db.New(s.Pool).ListDefinitions(ctx, db.ListDefinitionsParams{Cursor: cursor, MaxBytes: MaxListPageBytes})
	return decodePage[protocol.Definition](documents, err)
}

func (s *Store) Run(ctx context.Context, id string) (protocol.Run, error) {
	var v protocol.Run
	var b []byte
	b, e := db.New(s.Pool).ReadRunProjection(ctx, id)
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	if e == nil {
		e = s.hydrateRun(ctx, &v)
	}
	return v, classify(e)
}

func (s *Store) Plan(ctx context.Context, id string) (contract.Plan, error) {
	return ReadPlan(ctx, s.Pool, id)
}

func ReadPlan(ctx context.Context, q Querier, id string) (contract.Plan, error) {
	var v contract.Plan
	var b []byte
	b, e := db.New(q).ReadPlan(ctx, id)
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	return v, classify(e)
}

func (s *Store) Runs(ctx context.Context, cursor string) ([]protocol.Run, error) {
	documents, err := db.New(s.Pool).ListRuns(ctx, cursor)
	runs, err := decodeRecords[protocol.Run](documents, err)
	if err != nil {
		return nil, err
	}
	size := 0

	for i := range runs {
		if i > 0 && size >= MaxListPageBytes {
			return runs[:i+1], nil
		}
		if err = s.hydrateRun(ctx, &runs[i]); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(runs[i])
		if err != nil {
			return nil, err
		}
		size += len(encoded)
		if i > 0 && size > MaxListPageBytes {
			return runs[:i+1], nil
		}
	}
	return runs, nil
}

func decodeRecords[T any](documents [][]byte, err error) ([]T, error) {
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(documents))
	for _, document := range documents {
		var record T
		if err := json.Unmarshal(document, &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

func PutRun(ctx context.Context, tx pgx.Tx, run protocol.Run, plan contract.Plan, inputs contract.Values) error {
	if err := insertRun(ctx, tx, run, plan, inputs); err != nil {
		return err
	}
	return Enqueue(ctx, tx, run.ID, "start", execution.RunAdmission{RunID: run.ID, AcceptedAt: run.CreatedAt, Inputs: inputs})
}

func insertRun(ctx context.Context, tx pgx.Tx, run protocol.Run, plan contract.Plan, inputs contract.Values) error {
	state := run
	state.Package = contract.Package{}
	state.Inputs = nil
	state.InputArtifacts = nil
	state.Instances = []protocol.Instance{}
	b, e := raw(state)
	if e != nil {
		return e
	}
	p, e := raw(plan)
	if e != nil {
		return e
	}
	i, e := raw(inputs)
	if e != nil {
		return e
	}
	_, e = db.New(tx).InsertRun(ctx, db.InsertRunParams{ID: run.ID, DefinitionID: run.DefinitionID, Document: b, Plan: p, Inputs: i})
	if e != nil {
		return e
	}
	return nil
}

func Enqueue(ctx context.Context, tx pgx.Tx, runID, kind string, payload any) error {
	b, e := raw(payload)
	if e != nil {
		return e
	}
	_, e = db.New(tx).Enqueue(ctx, db.EnqueueParams{RunID: runID, Kind: kind, Payload: b})
	return e
}

func (s *Store) Deliver(ctx context.Context, send func(context.Context, Querier, string, string, []byte) error) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(ctx) }()

	storedNextOutbox, queryErr7 := db.New(tx).LockNextOutbox(ctx)
	e = queryErr7

	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = send(ctx, tx, storedNextOutbox.RunID, storedNextOutbox.Kind, storedNextOutbox.Payload); e != nil {
		return e
	}
	_, e = db.New(tx).MarkOutboxSent(ctx, storedNextOutbox.ID)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (s *Store) Events(ctx context.Context, runID, after string) ([]protocol.Event, error) {
	return s.History(ctx, runID, after, "")
}

// History pages the same committed order used by SSE; a filter only selects
// records, it never changes the meaning or validation of the run's cursor.
func (s *Store) History(ctx context.Context, runID, after, instanceID string) ([]protocol.Event, error) {
	var n int64
	var err error
	if after != "" {
		n, err = strconv.ParseInt(after, 10, 64)
		if err != nil || n < 1 {
			return nil, ErrConflict
		}
		var exists bool
		exists, err = db.New(s.Pool).EventIDExists(ctx, db.EventIDExistsParams{RunID: runID, ID: n})
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrNotFound
		}
	}
	rows, err := db.New(s.Pool).ListEvents(ctx, db.ListEventsParams{RunID: runID, ID: n, InstanceID: instanceID})
	if err != nil {
		return nil, err
	}

	out := []protocol.Event{}

	for _, record := range rows {
		var id int64
		var b []byte
		var ev protocol.Event
		id = record.ID
		b = record.Document
		if err = json.Unmarshal(b, &ev); err != nil {
			return nil, err
		}
		ev.ID = strconv.FormatInt(id, 10)
		out = append(out, ev)
	}
	return out, nil
}

func now() time.Time { return time.Now().UTC() }

// hydrateRun joins immutable inputs/package and instance projections on reads.
// Activities update only the small mutable document, never rewrite a 64MiB package.
func (s *Store) hydrateRun(ctx context.Context, run *protocol.Run) error {
	definition, err := s.Definition(ctx, run.DefinitionID)
	if err != nil {
		return err
	}
	run.Package = definition.Package
	var b []byte
	storedRunInputs, queryErr9 := db.New(s.Pool).ReadRunInputs(ctx, run.ID)
	err = queryErr9
	if err != nil {
		return err
	}

	b = storedRunInputs

	var inputs contract.Values
	if err = json.Unmarshal(b, &inputs); err != nil {
		return err
	}
	run.Inputs = map[string]json.RawMessage{}
	run.InputArtifacts = map[string]any{}

	for key, v := range inputs {
		switch {
		case v.JSON != nil:
			run.Inputs[key] = v.JSON
		case v.Collection:
			ids := []string{}

			for _, a := range v.Artifacts {
				ids = append(ids, a.ID)
			}

			run.InputArtifacts[key] = ids
		case len(v.Artifacts) == 1:
			run.InputArtifacts[key] = v.Artifacts[0].ID

		}
	}

	documents, err := db.New(s.Pool).ListInstances(ctx, run.ID)
	run.Instances, err = decodeRecords[protocol.Instance](documents, err)
	return err
}

func ReadRunStatus(ctx context.Context, q Querier, id string) (string, error) {
	var status string
	status, err := db.New(q).ReadRunStatus(ctx, id)
	return status, classify(err)
}

func ReadRunBackend(ctx context.Context, q Querier, id string) (string, error) {
	var backend string
	backend, err := db.New(q).ReadRunBackend(ctx, id)
	return backend, classify(err)
}

// MaxListPageBytes bounds materialized list responses. A single larger item is
// returned alone; callers receive a cursor instead of silent truncation.
const MaxListPageBytes = 32 << 20

func decodePage[T any](documents [][]byte, err error) ([]T, error) {
	if err != nil {
		return nil, err
	}
	out := []T{}
	size := 0

	for _, b := range documents {
		var value T
		if err = json.Unmarshal(b, &value); err != nil {
			return nil, err
		}
		out = append(out, value)
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		size += len(encoded)
		if len(out) > 1 && size > MaxListPageBytes {
			break
		}
	}
	return out, nil
}

func ReadRunName(ctx context.Context, q Querier, id string) (string, error) {
	var name string
	name, err := db.New(q).ReadRunName(ctx, id)
	return name, classify(err)
}
