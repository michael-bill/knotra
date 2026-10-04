// Package store persists projections and command receipts. Temporal remains the scheduler.
package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/engine"
	"github.com/michael-bill/knotra/internal/protocol"
)

//go:embed migrations/*.sql
var migrations embed.FS

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
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(712865723)"); err != nil {
		return nil, err
	}
	if err = migrate(ctx, tx); err != nil {
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO knotra_settings(key,value) VALUES ('engine_id',$1) ON CONFLICT DO NOTHING", uuid.NewString()); err != nil {
		return nil, err
	}
	s := &Store{Pool: p}
	if err = tx.QueryRow(ctx, "SELECT value FROM knotra_settings WHERE key='engine_id'").Scan(&s.EngineID); err != nil {
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
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", principal+":"+key)
	if err != nil {
		return 0, nil, err
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	var oldRoute, oldDigest string
	var status int
	var response []byte
	err = tx.QueryRow(ctx, "SELECT route,digest,status,response FROM knotra_commands WHERE principal=$1 AND id=$2", principal, key).Scan(
		&oldRoute,
		&oldDigest,
		&status,
		&response,
	)
	if err == nil {
		if oldRoute != route || oldDigest != digest {
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
	_, err = tx.Exec(
		ctx,
		"INSERT INTO knotra_commands(principal,id,route,digest,status,response) VALUES($1,$2,$3,$4,$5,$6)",
		principal,
		key,
		route,
		digest,
		status,
		response,
	)
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
	e = tx.QueryRow(
		ctx,
		"INSERT INTO knotra_definitions(id,digest,document) VALUES($1,$2,$3) ON CONFLICT(digest) DO UPDATE SET digest=EXCLUDED.digest RETURNING document",
		d.ID,
		d.PackageDigest,
		b,
	).Scan(&saved)
	if e == nil {
		e = json.Unmarshal(saved, &d)
	}
	return d, e
}

// Querier is implemented by a pool and a transaction. Command handlers must use
// their transaction for reads too, so concurrent admissions cannot exhaust a pool
// while holding every connection and waiting for an additional one.
type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func ReadDefinition(ctx context.Context, q Querier, id string) (protocol.Definition, error) {
	var d protocol.Definition
	var b []byte
	err := q.QueryRow(ctx, "SELECT document FROM knotra_definitions WHERE id=$1", id).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &d)
	}
	return d, classify(err)
}

func (s *Store) Definition(ctx context.Context, id string) (protocol.Definition, error) {
	return ReadDefinition(ctx, s.Pool, id)
}

func (s *Store) Definitions(ctx context.Context, cursor string) ([]protocol.Definition, error) {
	return listPage[protocol.Definition](
		ctx,
		s.Pool,
		"SELECT document FROM knotra_definitions WHERE ($1='' OR id<$1) ORDER BY id DESC LIMIT 101",
		cursor,
	)
}

func (s *Store) Run(ctx context.Context, id string) (protocol.Run, error) {
	var v protocol.Run
	var b []byte
	e := s.Pool.QueryRow(ctx, "SELECT document FROM knotra_runs WHERE id=$1", id).Scan(&b)
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	if e == nil {
		e = s.hydrateRun(ctx, &v)
	}
	return v, classify(e)
}

func (s *Store) Plan(ctx context.Context, id string) (contract.Plan, error) {
	var v contract.Plan
	var b []byte
	e := s.Pool.QueryRow(ctx, "SELECT plan FROM knotra_runs WHERE id=$1", id).Scan(&b)
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	return v, classify(e)
}

func (s *Store) Runs(ctx context.Context, cursor string) ([]protocol.Run, error) {
	runs, err := list[protocol.Run](
		ctx,
		s.Pool,
		"SELECT document FROM knotra_runs WHERE ($1='' OR id<$1) ORDER BY id DESC LIMIT 101",
		cursor,
	)
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

func list[T any](ctx context.Context, p *pgxpool.Pool, query string, args ...any) ([]T, error) {
	rows, err := p.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}

	for rows.Next() {
		var b []byte
		var v T
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}

	return out, rows.Err()
}

func PutRun(ctx context.Context, tx pgx.Tx, run protocol.Run, plan contract.Plan, inputs contract.Values) error {
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
	_, e = tx.Exec(
		ctx,
		"INSERT INTO knotra_runs(id,definition_id,document,plan,inputs) VALUES($1,$2,$3,$4,$5)",
		run.ID,
		run.DefinitionID,
		b,
		p,
		i,
	)
	if e != nil {
		return e
	}
	return Enqueue(ctx, tx, run.ID, "start", engine.RunInput{RunID: run.ID, AcceptedAt: run.CreatedAt, Inputs: inputs})
}

func Enqueue(ctx context.Context, tx pgx.Tx, runID, kind string, payload any) error {
	b, e := raw(payload)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, "INSERT INTO knotra_outbox(run_id,kind,payload) VALUES($1,$2,$3)", runID, kind, b)
	return e
}

func (s *Store) Deliver(ctx context.Context, send func(context.Context, Querier, string, string, []byte) error) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var id int64
	var runID, kind string
	var payload []byte
	e = tx.QueryRow(
		ctx,
		"SELECT o.id,o.run_id,o.kind,o.payload FROM knotra_outbox o WHERE o.sent_at IS NULL AND NOT EXISTS (SELECT 1 FROM knotra_outbox earlier WHERE earlier.run_id=o.run_id AND earlier.id<o.id AND earlier.sent_at IS NULL) ORDER BY o.id FOR UPDATE OF o SKIP LOCKED LIMIT 1",
	).Scan(
		&id,
		&runID,
		&kind,
		&payload,
	)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = send(ctx, tx, runID, kind, payload); e != nil {
		return e
	}
	_, e = tx.Exec(ctx, "UPDATE knotra_outbox SET sent_at=now() WHERE id=$1", id)
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
		err = s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM knotra_events WHERE run_id=$1 AND id=$2)", runID, n).Scan(&exists)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrNotFound
		}
	}
	rows, err := s.Pool.Query(ctx, "SELECT id,document FROM knotra_events WHERE run_id=$1 AND id>$2 AND ($3='' OR document->>'instanceId'=$3) ORDER BY id LIMIT 100", runID, n, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.Event{}

	for rows.Next() {
		var id int64
		var b []byte
		var ev protocol.Event
		if err = rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &ev); err != nil {
			return nil, err
		}
		ev.ID = strconv.FormatInt(id, 10)
		out = append(out, ev)
	}

	return out, rows.Err()
}

func now() time.Time { return time.Now().UTC() }

// migrate applies immutable, ordered migrations under Open's transaction lock.
func migrate(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(
		ctx,
		`CREATE TABLE IF NOT EXISTS knotra_migrations (name text PRIMARY KEY, sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`,
	); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	known := map[string]bool{}

	for _, name := range names {
		known[name] = true
	}

	rows, err := tx.Query(ctx, "SELECT name FROM knotra_migrations")
	if err != nil {
		return err
	}

	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if !known[name] {
			rows.Close()
			return fmt.Errorf("database has newer migration %s; upgrade engine", name)
		}
	}

	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}

	for _, name := range names {
		b, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		digest := hex.EncodeToString(sum[:])
		var existing string
		err = tx.QueryRow(ctx, "SELECT sha256 FROM knotra_migrations WHERE name=$1", name).Scan(&existing)
		if err == nil {
			if digest != existing {
				return fmt.Errorf("applied migration %s checksum changed", name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err = tx.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO knotra_migrations(name,sha256) VALUES($1,$2)", name, digest); err != nil {
			return err
		}
	}

	return nil
}

// hydrateRun joins immutable inputs/package and instance projections on reads.
// Activities update only the small mutable document, never rewrite a 64MiB package.
func (s *Store) hydrateRun(ctx context.Context, run *protocol.Run) error {
	definition, err := s.Definition(ctx, run.DefinitionID)
	if err != nil {
		return err
	}
	run.Package = definition.Package
	var b []byte
	if err = s.Pool.QueryRow(ctx, "SELECT inputs FROM knotra_runs WHERE id=$1", run.ID).Scan(&b); err != nil {
		return err
	}
	var inputs contract.Values
	if err = json.Unmarshal(b, &inputs); err != nil {
		return err
	}
	run.Inputs = map[string]json.RawMessage{}
	run.InputArtifacts = map[string]any{}

	for key, v := range inputs {
		if v.JSON != nil {
			run.Inputs[key] = v.JSON
		} else if v.Collection {
			ids := []string{}

			for _, a := range v.Artifacts {
				ids = append(ids, a.ID)
			}

			run.InputArtifacts[key] = ids
		} else if len(v.Artifacts) == 1 {
			run.InputArtifacts[key] = v.Artifacts[0].ID
		}
	}

	run.Instances, err = list[protocol.Instance](ctx, s.Pool, "SELECT document FROM knotra_instances WHERE run_id=$1 ORDER BY id", run.ID)
	return err
}

func ReadRunStatus(ctx context.Context, q Querier, id string) (string, error) {
	var status string
	err := q.QueryRow(ctx, "SELECT document->>'status' FROM knotra_runs WHERE id=$1", id).Scan(&status)
	return status, classify(err)
}

// MaxListPageBytes bounds materialized list responses. A single larger item is
// returned alone; callers receive a cursor instead of silent truncation.
const MaxListPageBytes = 32 << 20

func listPage[T any](ctx context.Context, p *pgxpool.Pool, query string, args ...any) ([]T, error) {
	rows, err := p.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	size := 0

	for rows.Next() {
		var b []byte
		var value T
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
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

	return out, rows.Err()
}

func ReadRunName(ctx context.Context, q Querier, id string) (string, error) {
	var name string
	err := q.QueryRow(
		ctx,
		`SELECT d.document->>'name' FROM knotra_runs r JOIN knotra_definitions d ON d.id=r.definition_id WHERE r.id=$1`,
		id,
	).Scan(&name)
	return name, classify(err)
}
