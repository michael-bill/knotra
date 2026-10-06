package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/michael-bill/knotra/internal/store/db"
)

// SchemaTarget is the final main-line PostgreSQL migration in River v0.48.0.
// Updating the dependency requires reviewing and explicitly updating this target.
const SchemaTarget = 8

// Migrate runs each River migration in its own transaction. The advisory lock
// uses a dedicated connection, so a MaxConns=1 pool still has its connection
// available for the migrator. Never call this inside Store.Open's transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	lock, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect River migration lock: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = lock.Close(cleanup) // closing releases the session advisory lock
	}()
	if schema == "" {
		schema, err = db.New(lock).CurrentSchema(ctx)
		if err != nil {
			return err
		}
	}
	if _, err := db.New(lock).LockRiverMigrations(ctx, "knotra:river:migrate:"+schema); err != nil {
		return err
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: schema})
	if err != nil {
		return err
	}
	existing, err := migrator.ExistingVersions(ctx)
	if err != nil {
		return err
	}
	for _, version := range existing {
		if version.Version > SchemaTarget {
			return fmt.Errorf("river schema %d exceeds supported target %d", version.Version, SchemaTarget)
		}
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{TargetVersion: SchemaTarget}); err != nil {
		return fmt.Errorf("migrate River: %w", err)
	}
	validation, err := migrator.Validate(ctx, &rivermigrate.ValidateOpts{TargetVersion: SchemaTarget})
	if err != nil {
		return err
	}
	if !validation.OK {
		return fmt.Errorf("river schema validation: %v", validation.Messages)
	}
	return nil
}
