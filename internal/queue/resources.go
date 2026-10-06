package queue

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
	"github.com/michael-bill/knotra/internal/store/db"
	"github.com/michael-bill/knotra/internal/telemetry"
)

func (r *Runtime) cleanup(ctx context.Context, job *river.Job[CleanupArgs]) (err error) {
	defer func() {
		if err != nil {
			schedulerCount(ctx, "cleanup.failures")
		}
	}()
	args := job.Args
	if args.HostID != r.HostID {
		return river.JobCancel(errors.New("cleanup delivery belongs to another host"))
	}
	var resource *execution.ResourceRecord
	err = pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		var err error
		resource, err = r.Store.ClaimResourceCleanupTx(ctx, tx, args.ResourceID, args.HostID, args.WorkerID, args.OwnershipGeneration, args.DispatchGeneration)
		return err
	})
	if err != nil {
		return err
	}
	if resource != nil {
		trace.SpanFromContext(ctx).SetAttributes(telemetry.OwnershipAttributes(resource.Ownership)...)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("knotra.actor.id", r.WorkerID))
		defer func() {
			telemetry.Event(ctx, telemetry.OwnershipLogger(r.Host.Log, resource.Ownership), "resource.cleanup", err, attribute.String("knotra.resource.id", resource.ID))
		}()
		if resource.Kind == "mcp_http" {
			if resource.MCP == nil {
				resource.MCP, err = r.Outcomes.GetMCPSession(*resource)
				if err != nil {
					return err
				}
				if err := r.Store.RecordOwnedMCPSession(ctx, resource.Ownership, resource.ID, *resource.MCP); err != nil {
					return err
				}
			}
			plan, err := r.Store.Plan(ctx, resource.Ownership.RunID)
			if err != nil {
				return err
			}
			if err := r.Host.Runner.CleanupMCPSession(ctx, *resource, plan.Profile); err != nil {
				return err
			}
		} else {
			if err := r.Host.Runner.CleanupResource(ctx, *resource); err != nil {
				return err
			}
		}
	}
	return pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if resource != nil {
			if _, err := db.New(tx).CloseCleanedResource(ctx, db.CloseCleanedResourceParams{ID: resource.ID, EngineID: resource.EngineID, HostID: resource.HostID, WorkerID: resource.Ownership.WorkerID, OwnershipGeneration: resource.Ownership.Generation, CleanupGeneration: resource.DispatchGeneration}); err != nil {
				return err
			}
		}
		_, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job)
		return err
	})
}

// The cursor bounds each scan even when live resources fill earlier pages.
// Native delivery repair changes only cleanup generation, never resource ownership.
func (r *Runtime) reconcileResources(ctx context.Context) error {
	rows, err := db.New(r.Store.Pool).ReadResourceCleanupCandidates(ctx, db.ReadResourceCleanupCandidatesParams{EngineID: r.Store.EngineID, HostID: r.HostID, Cursor: r.resourceCursor})
	if err != nil {
		return err
	}
	for _, row := range rows {
		err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			resource, err := r.Store.ClaimResourceCleanupTx(ctx, tx, row.ID, r.HostID, row.WorkerID, row.OwnershipGeneration, 0)
			if err != nil || resource == nil {
				return err
			}
			return r.enqueueResourceCleanup(ctx, tx, *resource)
		})
		r.resourceCursor = row.ID
		if err != nil {
			return err
		}
	}
	if len(rows) < 64 {
		r.resourceCursor = ""
	}
	return nil
}

func (r *Runtime) reconcilePhysicalResources(ctx context.Context) error {
	if r.Host.Runner.WorkDir == "" {
		return nil
	}
	resources, cursor, scanErr := r.Host.Runner.ResourcePage(ctx, r.containerCursor)
	r.containerCursor = cursor
	for _, observed := range resources {
		err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			resource, err := r.Store.ClaimObservedResourceCleanupTx(ctx, tx, observed)
			if err != nil || resource == nil {
				return err
			}
			return r.enqueueResourceCleanup(ctx, tx, *resource)
		})
		if err != nil {
			return errors.Join(scanErr, err)
		}
	}
	return scanErr
}

func (r *Runtime) enqueueResourceCleanup(ctx context.Context, tx pgx.Tx, resource execution.ResourceRecord) error {
	args := CleanupArgs{HostID: resource.HostID, WorkerID: resource.Ownership.WorkerID, ResourceID: resource.ID, OwnershipGeneration: resource.Ownership.Generation, DispatchGeneration: resource.DispatchGeneration, RoutingVersion: RoutingVersion}
	now, err := db.New(tx).DatabaseTime(ctx)
	if err != nil {
		return err
	}
	viable, err := r.Client.hasDelivery(ctx, tx, args, now)
	if err != nil || viable {
		return err
	}
	args.DispatchGeneration, err = db.New(tx).BumpResourceCleanupDelivery(ctx, resource.ID)
	if err != nil {
		return err
	}
	return r.Client.InsertCleanup(ctx, tx, args)
}

func (r *Runtime) reconcileSessions(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			schedulerCount(ctx, "cleanup.failures")
		}
	}()
	keys, err := r.Host.Runner.SessionPage(r.sessionCursor)
	if err != nil {
		return err
	}
	for _, key := range keys {
		r.sessionCursor = key
		runID, _, _ := strings.Cut(key, "/")
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, runID)
		if err != nil {
			return err
		}
		if protocol.Terminal(status) {
			if err := r.Host.Runner.ReleaseSession(key); err != nil {
				return err
			}
		}
	}
	if len(keys) < 64 {
		r.sessionCursor = ""
	}
	return nil
}
