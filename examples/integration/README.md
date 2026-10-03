# Real integration pipeline

This package exercises all nine Knotra node types in **15 static node declarations** (12 in the root
graph, one foreach body, one loop body, and one child pipeline). Each successful run creates 18
dynamic node instances.

The process builds a CSV file, calculates statistics through MCP, asks a local model for a summary,
and runs an agent that reads the CSV, calls MCP, writes a Markdown report and checks it with Python.
A switch selects human approval; the unused fallback is skipped. Concurrent foreach iterations
double the input numbers, a bounded loop counts two iterations, and a child pipeline assembles the
approved report. A second MCP tool archives the summary with an idempotency key. The last code node
verifies the published file.

The package is used by `internal/integration/TestRealConcurrentPipelines`. That test starts the real
engine and a separate functional MCP service, then submits three independent runs with different
markers: one through the CLI, two through the HTTP client. Ollama, Docker, Temporal, PostgreSQL and
the model are real services; their execution is not replaced by test doubles. The generated
EngineProfile grants only the resources needed by this package.

## Run the acceptance test

Start PostgreSQL, Temporal, Ollama with `qwen3.5:9b`, and Docker/Colima. Build the Linux sandbox
helper as described in the repository README and pull `python:3.13-alpine`. Then run from the
repository root:

```sh
export KNOTRA_TEST_REAL=1
export KNOTRA_TEST_DATABASE_URL='postgres://localhost/knotra_dev?sslmode=disable'
export KNOTRA_TEST_OLLAMA='http://127.0.0.1:11434'
go test -mod=readonly -race -count=1 -timeout 30m ./internal/integration \
  -run '^TestReal(ConcurrentPipelines|ProcessRecovery)$' -v
```

Use the PostgreSQL credentials appropriate for your local installation. With the bundled Compose
setup, PostgreSQL uses port `25432` and Temporal uses `27233`; export
`KNOTRA_TEST_TEMPORAL=127.0.0.1:27233`. The [running guide](../../docs/running.md) contains the
development connection string and helper/firewall setup. The test creates and drops its own schema;
it does not reset existing tables.

Optional environment variables:

| Variable                     | Default                                           |
| ---------------------------- | ------------------------------------------------- |
| `KNOTRA_TEST_TEMPORAL`       | `127.0.0.1:7233`                                  |
| `KNOTRA_TEST_NAMESPACE`      | `default`                                         |
| `KNOTRA_TEST_HELPER`         | `.knotra/bin/sandbox-helper` under the repository |
| `KNOTRA_TEST_WORKDIR`        | `.knotra/integration-work` under the repository   |
| `KNOTRA_TEST_FIREWALL_IMAGE` | `knotra-firewall:dev`                             |
| `DOCKER_HOST`                | Current Docker context                            |

On macOS, keep the work directory under a directory shared with Colima. The test uses a separate
Temporal task queue and listens only on loopback addresses. Its MCP service computes statistics and
saves actual archive records to disk. The sandbox secret is a harmless generated test value.

## Assertions

- All three runs finish successfully through the public CLI/client/API.
- Invalid human answers are rejected before valid answers are accepted.
- Concurrent foreach results retain input order; the loop stops after two bodies.
- The unselected branch is skipped and the declared child permissions are checked.
- Each agent uses its own input file, tools and workspace.
- Each final report contains its run marker and excludes markers from other runs.
- Artifact byte count and SHA-256 match the downloaded content.
- MCP run sessions are separate, and archival writes happen once per run.
- Execution events show overlapping leaf work across multiple runs.
- No containers belonging to the completed runs remain.

Ordinary `go test ./...` checks that this package compiles offline but skips the real service test
unless `KNOTRA_TEST_REAL=1` is set. Failed acceptance runs keep their local evidence directory for
diagnosis; successful runs clean it up.

## Recovery without a model

`recovery.yaml` publishes a JSON artifact containing a random nonce, asks for human approval, and
checks the file in a second sandbox. The recovery test starts the actual engine in a separate
process and kills it while the approval is open. It restarts the engine using the same database,
data directory and task queue, then answers through the CLI. The test checks that the engine and
request IDs, deadline, artifact bytes and SSE event history survive, and that completed code does
not execute again. A second run is cancelled through the CLI; a later human answer must return HTTP
409 and downstream code must remain unexecuted.

This test needs Docker, Temporal and PostgreSQL, but no model or MCP service:

```sh
export KNOTRA_TEST_RECOVERY=1
export KNOTRA_TEST_DATABASE_URL='postgres://localhost/knotra_dev?sslmode=disable'
go test -mod=readonly -race -count=1 -timeout 10m ./internal/integration \
  -run '^TestRealProcessRecovery$' -v
```

The same helper, work directory, Docker and Temporal settings apply. Setting `KNOTRA_TEST_REAL=1`
also enables this recovery test.
