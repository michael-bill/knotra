# Verification

The implementation is checked at three levels: deterministic contract and
workflow tests, integration tests against PostgreSQL and Docker, and complete
executions through the public CLI/API with real services.

## Observed results

Verified on 2026-10-03 with Go 1.27.1 on macOS/arm64, Colima/Docker and a
Linux/arm64 sandbox helper, local PostgreSQL and Temporal, and Ollama running
`qwen3.5:9b`. The final real-service command below passed with the race detector:

| Test | Result | Elapsed |
| --- | --- | --- |
| `TestRealConcurrentPipelines` | Three successful independent runs; 15 static / 18 dynamic nodes per run; peak 18 active leaf nodes across all three runs; three verified reports; no remaining run containers | 201.05 s |
| `TestRealProcessRecovery` | SIGKILL/restart, preserved identity/file/events, no repeated completed code, CLI answer/cancel, and late answer rejected with 409 | 8.50 s |

The complete real-service package run took 211.305 seconds. Test data and
temporary schemas were cleaned after success. An earlier run exposed a premature
supervisor exit whose precise cause was lost with its container logs. A separate
regression reproduced termination on a transient lease-file read failure; the
supervisor now retains the last validated expiry while enforcing its hard deadline.
Stopped containers retain bounded diagnostics until cleanup. The table records
the run after that correction.

A separate Temporal stress check completed 1,004 nodes in 106.98 seconds with
three Continue-As-New transitions. Four retained histories replayed successfully
after the final CEL ordering change (0.92 seconds). Their serialized event totals
were 714,417, 822,458, 885,295 and 306,021 bytes. This check uses controlled fake
external activity I/O to isolate workflow history behavior; the model and sandbox
evidence comes from the real-service tests above. See [Temporal execution](temporal.md)
for its payload and history measurements.

## Reproduce the checks

The normal suite includes all 39 notation fixtures, strict parsing, package
confinement, schema validation, CEL evaluation and limits, orchestration,
Ollama protocol handling, and CLI/client behavior:

```sh
go test -mod=readonly -race ./...
```

Set `KNOTRA_TEST_DATABASE_URL` to a disposable PostgreSQL database to enable the
store and API integration tests. Each test uses its own schema and drops only
that schema:

```sh
export KNOTRA_TEST_DATABASE_URL='postgres://localhost/knotra_dev?sslmode=disable'
go test -mod=readonly -race ./internal/store ./internal/api
```

For complete execution, start Temporal, Docker/Colima and Ollama with
`qwen3.5:9b`, then build the Linux sandbox helper and firewall image as described
in [Running Knotra](running.md). The [integration package](../examples/integration/README.md)
lists optional service addresses and work-directory settings.

```sh
export KNOTRA_TEST_REAL=1
export KNOTRA_TEST_OLLAMA='http://127.0.0.1:11434'
go test -mod=readonly -race -count=1 -timeout 30m ./internal/integration \
  -run '^TestReal(ConcurrentPipelines|ProcessRecovery)$' -v
```

The recovery test can run without Ollama or MCP:

```sh
KNOTRA_TEST_RECOVERY=1 go test -mod=readonly -race -count=1 -timeout 10m \
  ./internal/integration -run '^TestRealProcessRecovery$' -v
```

With Temporal available, the history stress test runs independently of Ollama:

```sh
KNOTRA_TEST_TEMPORAL_STRESS=1 go test -mod=readonly ./internal/engine \
  -run '^TestTemporalLargePlanAndHistoryContinuation$' -count=1 -v
```

## What the acceptance tests establish

`TestRealConcurrentPipelines` submits three independent runs with different
markers. One submission uses `cli.Execute` to load and publish the YAML package;
the other two use the same HTTP client as the CLI. Each run executes all nine
node types in 15 static declarations and creates 18 dynamic node instances.

The model and agent use real Ollama inference. The agent reads its own CSV,
calls an actual MCP service, writes a Markdown file, and invokes Python. The
MCP service runs in another operating-system process; it computes statistics,
maintains separate run sessions and writes real idempotent archive records.
Docker runs every code/agent sandbox. Temporal drives execution, and PostgreSQL
stores commands, requests, projections and resource metadata. These acceptance
tests do not replace any of those components with mocks.

Durable operation journals confirm 8, 8 and 9 model turns for the three agents.
For each agent, the test correlates actual model-requested calls with completed
`files.read`, `files.write`, Python `process.exec` and MCP `dataset.stats`
operations. It checks the run marker in file contents, process stdout and MCP
results, a successful process exit, and the expected MCP statistics. Every agent
finishes with a solitary valid finish call and one committed report artifact.

Assertions cover rejected invalid human responses, valid approval, a skipped
switch branch, ordered concurrent foreach outputs, a two-iteration loop,
subpipeline permissions, archive idempotency, separate MCP sessions, overlapping
work across runs, downloaded artifact bytes and checksums, absence of other
runs' markers, and container cleanup.

`TestRealProcessRecovery` starts the actual engine composition in a separate
process, publishes an artifact containing a random nonce, then sends `SIGKILL`
while a human request is open. It restarts against the same database, data
directory and task queue. The engine identity, request identity and deadline,
artifact bytes, and SSE history must remain unchanged. The CLI answers the
request; the completed code must not run again. A second run is cancelled
through the CLI, its late answer must receive HTTP 409, and its downstream code
must never start.

## Scope of the evidence

These are functional and recovery checks, not throughput benchmarks. The report
profile explicitly caps each run at six concurrent nodes, 128 node instances,
40 model calls, 100 tool calls, and a 30-minute deadline. Ollama may serialize
inference even while the engine executes independent work concurrently.

The engine currently implements the Ollama model adapter, and the live model
acceptance uses `qwen3.5:9b`. OpenAI and Anthropic adapters are not implemented;
provider examples in a contract do not establish runtime support. The functional
MCP service exercises HTTP sessions and idempotency; separate adapter tests
exercise stdio transport and sandbox/network restrictions. A passing scenario
is evidence for these paths, not a guarantee for every external service or every
possible pipeline.
