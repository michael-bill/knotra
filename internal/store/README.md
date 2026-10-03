# Persistence

`Store` owns PostgreSQL transactions, durable command receipts, outbox delivery, external operation
journals, budgets, requests and read projections. `Artifacts` keeps immutable bytes in the engine
data directory and metadata in PostgreSQL. A runtime artifact becomes visible when its producing
node commits validated outputs; uploaded inputs are visible immediately.

Temporal owns workflow history. PostgreSQL projections support client reads and may lag behind
execution; clients do not schedule from these projections. The engine's advisory lock permits one
owning process per engine ID.

## Migrations

`migrations/*.sql` is embedded into the binary and applied in filename order under a transaction
lock. Applied filenames and SHA-256 checksums are recorded in `knotra_migrations`. **Existing
migrations are byte-immutable**, including whitespace: reformatting an applied file would prevent
the engine from opening an existing database. Add a new ordered migration for a schema change.
Unknown newer migrations are rejected by an older binary.

The readability review deliberately preserves the existing SQL migration files. New SQL should use
separate lines for columns, constraints and logical clauses. Do not reset a contributor's database
to work around migration checks.

## Checks and recovery

Set `KNOTRA_TEST_DATABASE_URL` to enable database tests. Each test creates and removes only its own
schema:

```sh
KNOTRA_TEST_DATABASE_URL='postgres://localhost/knotra_dev?sslmode=disable' \
  go test -mod=readonly -race -count=1 ./internal/store ./internal/api
```

Recovery requires PostgreSQL, Temporal history and the engine data directory with artifacts and
payload files. SQLite in the desktop app is a client store; it is independent of these three engine
stores. See [running](../../docs/running.md) and [verification](../../docs/verification.md) for live
recovery checks.
