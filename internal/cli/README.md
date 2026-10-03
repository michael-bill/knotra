# CLI

`cmd/knotra` provides signal handling and exit codes. `internal/cli` owns parsing and presentation;
network behavior and mutation reconciliation belong to `internal/client`. CLI commands never run
pipeline workloads locally.

The client shares the [versioned HTTP/SSE contract](../../docs/api/desktop-v1.md) with desktop.
[Running](../../docs/running.md) covers infrastructure and engine configuration;
[verification](../../docs/verification.md) lists real acceptance checks. The CLI remains usable
without installing the desktop toolchain.

Global options:

- `--endpoint` / `KNOTRA_ENDPOINT`, default `http://127.0.0.1:8787`.
- `KNOTRA_TOKEN` for engine bearer authentication. Tokens are not command flags or persisted
  receipts.
- `--state-dir` / `KNOTRA_STATE_DIR` for durable mutation receipts and event cursors; defaults to
  the OS configuration directory's `knotra` subdirectory.
- `--json` for JSON objects; event streams emit one JSON event per line.

```sh
knotra validate pipeline.yaml
knotra validate pipeline.yaml --profile engine-profile.yaml
knotra validate pipeline.yaml --remote --profile local --inputs inputs.json
knotra run pipeline.yaml --profile local --input 'topic="AI"' --wait
knotra runs list --all
knotra runs get RUN_ID --json
knotra runs watch RUN_ID
knotra runs cancel RUN_ID
knotra requests list --run RUN_ID --all --json
knotra requests respond REQUEST_ID --output 'approved=true'
knotra artifacts upload input.txt --media-type text/plain
knotra artifacts download ARTIFACT_ID --output report.txt
knotra operations list
knotra operations retry OPERATION_ID
```

`run` publishes a package version, then admits a run. The two commands have distinct stable
idempotency identities derived from `--idempotency-key` when provided. `--definition ID` starts an
existing immutable definition instead of loading a file. JSON inputs use `--inputs FILE` or repeated
`--input NAME=JSON`; artifact references use `--artifacts FILE` or repeated `--artifact NAME=ID` /
`--artifact 'NAME=["ID"]'`. The literal `-` reads a JSON object or uploaded bytes from stdin.

`--wait` prints final state. `--watch` prints events and final state; with `--json` this is a JSON
Lines stream followed by a final `{run,operationId}` object. Interrupting either stops observation
only. `runs wait --timeout 5m` similarly does not alter engine state. Exit codes: 0 success, 1
command/transport error, 2 invalid pipeline, 3 observed failed/cancelled run, 130 interruption.

Event journals are scoped by endpoint, engine, principal and run identity. Each event and cursor is
atomically persisted before display. Up to 1000 recent events (bounded to 8 MiB) are cached; the
engine retains authoritative history. Use `runs watch --from-start` or `--cursor ID` to select a
different replay position.

`runs resolve RUN_ID INSTANCE_ID` requires `--outcome succeeded|not_started|failed` and
`--evidence TEXT`; verified JSON outputs use `--outputs FILE` or `--output`. The engine decides
whether the evidence permits continuation. `resume` is a separate request and cannot bypass an
uncertain external operation.

Artifact downloads verify both size and SHA-256 before publishing any bytes. Files are created
atomically and are never overwritten unless `--force` is explicit. `--output -` emits exact binary
bytes and cannot be combined with `--json`.

Run `knotra serve --help` for infrastructure options. The service requires at least one trusted
`--profile FILE` and a PostgreSQL URL through `KNOTRA_DATABASE_URL` or `--database-url`; Temporal
defaults to `127.0.0.1:7233`. Engine logs go to stderr.
