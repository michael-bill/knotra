# Persistence

`Store` owns PostgreSQL transactions, durable command receipts, outbox delivery, external operation
journals, budgets, requests and read projections. `Artifacts` keeps immutable bytes in the engine
data directory and metadata in PostgreSQL. A runtime artifact becomes visible when its producing
node commits validated outputs; uploaded inputs are visible immediately.

Temporal owns workflow history. PostgreSQL projections support client reads and may lag behind
execution; clients do not schedule from these projections. The engine's advisory lock permits one
owning process per engine ID.

## Initial schema

`schema.sql` is the complete application schema for a new database. `Store.Open` creates it once
under a transaction advisory lock; concurrent opens share the same engine identity, and reopening
preserves existing data. There is no application migration history or schema-upgrade runner. The
project has not launched, so change this schema directly. River initializes and manages its own
queue tables through its library.

## Queries

`queries/*.sql` contains named sqlc queries. `sqlc.yaml` reads `schema.sql` and generates the pgx/v5
methods in `db/`. Store and scheduler code pass their existing pool, connection or transaction to
`db.New`; transaction ownership, savepoints and locking remain in the calling Go code. Only
executing schema DDL stays outside generated methods. River maintains its own schema through its
migration library.

Run `make generate-sql` after editing queries or the initial schema. The Makefile pins sqlc
**1.31.1**; generated Go files are checked in, so ordinary builds need no generator.
`make check-sql` regenerates and checks for a diff, and CI runs it before Go checks. An installed
copy of the pinned version can be used with `make generate-sql SQLC=sqlc`.

River's resource ledger records ownership before sandbox creation or HTTP MCP initialization. MCP
cleanup identities contain only a frozen connection name, session ID and protocol version; resolved
credentials stay out of the ledger. Late initialization evidence must match the original owner and
cannot replace an existing identity. Live run-scoped resources remain protected after their creating
attempt ends. The executor first syncs immutable initialization evidence in the outcome directory,
then updates PostgreSQL; recovery can import a missing database identity from the file after
checking the entire owner. Include these files in database/outcome backups. A lost response before
evidence publication leaves an unresolved resource intent, not a record of successful remote
cleanup.

For owned HTTP MCP execution, the same directory also keeps one atomically replaced current-call
record per resource. It contains the original session/owner and JSON-RPC request ID, without call
arguments or credentials, and is synced before physical admission/dispatch. A confirmed reply marks
it complete. After process loss, cleanup notifies cancellation of an unconfirmed call before
deleting the recorded session. Failed notifications/deletions remain retryable. Keep current-call
records with initialization evidence in the backup; remote cancellation requires server cooperation.

Artifacts and outcome files can use `serve --shared-data-dir DIR`; local sandbox staging stays in
`--data-dir`. Artifact content addresses are immutable hard links: identical writers reuse checked
bytes, while damaged content, symlinks or directories cause a failure instead of being overwritten.
The same confined root initialization syncs newly created parent entries for both stores. Shared
filesystem deployments need hard links, atomic rename, durable sync and cross-client read
consistency. Independent local writer processes and sequential engine replacement are tested;
network filesystems and concurrent worker hosts still require acceptance.

## Checks and recovery

Set `KNOTRA_TEST_DATABASE_URL` to enable database tests. Each test creates and removes only its own
schema:

```sh
KNOTRA_TEST_DATABASE_URL='postgres://localhost/knotra_dev?sslmode=disable' \
  go test -mod=readonly -race -count=1 ./internal/store ./internal/api
```

River recovery requires PostgreSQL plus complete immutable artifact/outcome storage, including
uncommitted envelopes and MCP evidence. Temporal runs additionally require their Temporal history
and offloaded payload directory. SQLite in the desktop app is an independent client store. See the
[River backup procedure](../../docs/running.md#river-backup-and-restore) and
[verification](../../docs/verification.md) for live recovery checks.
