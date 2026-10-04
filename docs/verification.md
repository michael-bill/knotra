# Verification

The implementation is checked at three levels: deterministic contract and workflow tests,
integration tests against PostgreSQL and Docker, and complete executions through the public CLI/API
with real services.

## Desktop consistency re-review on 4 October 2026

A second review compared desktop behavior and documentation with the implemented Go API. It found
stale Settings text and functional gaps that the initial acceptance scenarios did not cover.

The corrections cover Pipeline entrypoint selection when profiles or schemas appear first in an
import, duplicate ZIP paths, byte-preserving UTF-8 BOM handling, typed file references, foreach
schema compatibility, and visual connection type compatibility. Artifact upload now requires a
base64 string; omitted/null content and JSON byte arrays cannot silently create artifacts. An
explicit empty string still represents an empty artifact.

Browser and native clients validate read responses before replacing cached data and reject unsafe
integers before persisting SSE events or advancing the replay cursor. The native contract check
covers structure, required fields, types, references, enums and bounds; it does not implement a
complete JSON Schema validator or validate date formats and content encodings. Pagination preserves
the server's structured errors. Session changes invalidate late responses, overlapping refreshes
retain a follow-up read, and accepted commands return their durable receipts without waiting for
background reads. Rejected starts can be edited, human-resolution errors remain visible, and cached
artifact actions require a connected engine.

The live browser acceptance now creates a workflow from Library, runs it through actual Ollama,
downloads its verified result, and reopens its history in a fresh client. The second scenario checks
binary input, human validation and an imported child pipeline. The Go real-service suite again
passed with the race detector: store 4.561 s, API 7.279 s, adapters 183.798 s, sandbox helper 3.331
s, and integration 192.546 s. `make check` passed against PostgreSQL; Python formatting and all 39
parse/structural fixtures passed.

| Final check                                    | Result                                                                                         |
| ---------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Frontend unit tests and production build       | 80 tests passed; TypeScript/Vite build passed                                                  |
| Complete Playwright suite with a real endpoint | 49 passed, including both real engine scenarios; no skips                                      |
| Native unit and HTTP fixture tests             | 25 passed; the opt-in real engine test is ignored in the default suite                         |
| Native opt-in acceptance                       | Passed after the final cache, precision and artifact-download corrections                      |
| Repository formatting and documentation links  | Prettier, gofmt, rustfmt and Black passed; 23 Markdown files had no missing local link targets |

Documentation distinguishes implemented run lists and timelines from the future real execution
graph, agent message view and token streaming. Ollama remains the only implemented model provider.
No SQL migrations or dependency lockfiles changed in this re-review. The reproduction commands below
apply to both reviews.

## Initial readability review on 4 October 2026

The repository readability review and desktop integration were checked on macOS/arm64 with Go
1.27.1, Bun 1.3.10, Rust 1.99.0, Colima/Docker, the bundled PostgreSQL/Temporal development
infrastructure, and local Ollama `qwen3.5:9b`.

| Check                                          | Result                                                                                                        |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `make check` with `KNOTRA_TEST_DATABASE_URL`   | gofmt, vet and race tests passed; persistence/API tests used PostgreSQL                                       |
| Full real-service Go suite below               | Passed: store 2.835 s, API 5.325 s, adapters 262.141 s, helper 2.770 s, integration 235.417 s                 |
| Final API regression after title fixes         | Passed with PostgreSQL and the race detector                                                                  |
| Frontend unit tests and production build       | 50 tests passed; TypeScript/Vite build passed                                                                 |
| Complete Playwright suite with a real endpoint | 41 tests passed: 39 authoring/demo/fixture tests and two real engine scenarios                                |
| Native unit and HTTP fixture tests             | 19 passed; one real engine test is ignored by default                                                         |
| Native opt-in acceptance                       | Passed against the same Go engine with SQLite receipt recovery, human review, SSE replay and binary downloads |
| Python fixtures and formatting                 | Black check passed for all three files; 39 fixtures matched parse/structural expectations                     |

The real Go suite exercised three concurrent pipelines with all nine node types, actual Ollama
inference, agent tools, Docker, MCP, Temporal and PostgreSQL, plus SIGKILL/restart recovery. Run
from the repository root after preparing services:

```sh
export KNOTRA_TEST_DATABASE_URL='postgres://knotra:knotra-development@127.0.0.1:25432/knotra?sslmode=disable'
export KNOTRA_TEST_TEMPORAL=127.0.0.1:27233
export KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper"
export KNOTRA_TEST_WORKDIR="$PWD/.knotra/test-work"
export KNOTRA_TEST_FIREWALL_IMAGE=knotra-firewall:dev
export KNOTRA_TEST_OLLAMA=http://127.0.0.1:11434
export KNOTRA_TEST_REAL=1
go test -mod=readonly -race -count=1 -timeout=30m \
  ./internal/store ./internal/api ./internal/adapters/... ./internal/integration
```

For desktop acceptance, start the engine with the bundled `local` profile and
`--cors-origin http://127.0.0.1:1420`. This review used a separate engine on port `18787` with its
own data directory; the default endpoint is `8787`. From `app/`:

```sh
KNOTRA_E2E_ENDPOINT=http://127.0.0.1:8787 bun run test:e2e
KNOTRA_E2E_ENDPOINT=http://127.0.0.1:8787 \
  cargo test --locked --manifest-path src-tauri/Cargo.toml real_engine -- --ignored
```

The browser tests submit packages through the UI. One checks Ollama/code output and replay of a
completed run on a fresh client. The other uploads binary input, checks a human schema rejection,
answers correctly, executes an imported child pipeline with its transitive script, and exports
identical bytes. Native acceptance uses the Rust command boundary and reopens SQLite to recover an
exact start receipt. It checks server-side 422 rejection, valid review, durable SSE cursor/replay
deduplication and download integrity. These tests retain their engine definitions/runs/artifacts;
use a development engine.

Mechanical edits were also compared with the previous source: 70 Go files have unchanged AST apart
from positions/comments/import order; the three deliberate Go changes are API titles and their
regression tests. Python AST and parsed JSON/YAML content are unchanged by formatting. TypeScript
emission differences were checked: formatting only splits adjacent JSX text in three components;
functional edits concern the documented integration fixes. Applied SQL migration bytes, generated
assets and dependency locks are not hand formatted.

Vite reports the existing large-bundle warning. Signing/notarization and Windows or Linux native
builds were not verified in this macOS review. The first GitHub run passed Go checks and
integration, native tests, Python fixtures, and frontend formatting, unit tests and build. Its Linux
browser job failed because screenshots used macOS temporary paths and two polling callbacks read
data before the browser's debounced persistence completed.

The CI follow-up uses Playwright per-test output directories and waits for persisted state without
throwing on fields that have not been saved yet. The complete offline browser suite passed locally
with `CI=1`: 39 tests passed; the two opt-in real-engine scenarios were skipped. Application
behavior was unchanged by this follow-up.

## Historical verification on 3 October 2026

Verified on 2026-10-03 with Go 1.27.1 on macOS/arm64, Colima/Docker and a Linux/arm64 sandbox
helper, local PostgreSQL and Temporal, and Ollama running `qwen3.5:9b`. The final real-service
command below passed with the race detector:

| Test                          | Result                                                                                                                                                                        | Elapsed  |
| ----------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------- |
| `TestRealConcurrentPipelines` | Three successful independent runs; 15 static / 18 dynamic nodes per run; peak 18 active leaf nodes across all three runs; three verified reports; no remaining run containers | 201.05 s |
| `TestRealProcessRecovery`     | SIGKILL/restart, preserved identity/file/events, no repeated completed code, CLI answer/cancel, and late answer rejected with 409                                             | 8.50 s   |

The complete real-service package run took 211.305 seconds. Test data and temporary schemas were
cleaned after success. An earlier run exposed a premature supervisor exit whose precise cause was
lost with its container logs. A separate regression reproduced termination on a transient lease-file
read failure; the supervisor now retains the last validated expiry while enforcing its hard
deadline. Stopped containers retain bounded diagnostics until cleanup. The table records the run
after that correction.

A separate Temporal stress check completed 1,004 nodes in 106.98 seconds with three Continue-As-New
transitions. Four retained histories replayed successfully after the final CEL ordering change (0.92
seconds). Their serialized event totals were 714,417, 822,458, 885,295 and 306,021 bytes. This check
uses controlled fake external activity I/O to isolate workflow history behavior; the model and
sandbox evidence comes from the real-service tests above. See [Temporal execution](temporal.md) for
its payload and history measurements.

## Reproduce the checks

The normal suite includes all 39 notation fixtures, strict parsing, package confinement, schema
validation, CEL evaluation and limits, orchestration, Ollama protocol handling, and CLI/client
behavior:

```sh
go test -mod=readonly -race ./...
```

Set `KNOTRA_TEST_DATABASE_URL` to a disposable PostgreSQL database to enable the store and API
integration tests. Each test uses its own schema and drops only that schema:

```sh
export KNOTRA_TEST_DATABASE_URL='postgres://localhost/knotra_dev?sslmode=disable'
go test -mod=readonly -race ./internal/store ./internal/api
```

For complete execution, start Temporal, Docker/Colima and Ollama with `qwen3.5:9b`, then build the
Linux sandbox helper and firewall image as described in [Running Knotra](running.md). The
[integration package](../examples/integration/README.md) lists optional service addresses and
work-directory settings.

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

`TestRealConcurrentPipelines` submits three independent runs with different markers. One submission
uses `cli.Execute` to load and publish the YAML package; the other two use the same HTTP client as
the CLI. Each run executes all nine node types in 15 static declarations and creates 18 dynamic node
instances.

The model and agent use real Ollama inference. The agent reads its own CSV, calls an actual MCP
service, writes a Markdown file, and invokes Python. The MCP service runs in another
operating-system process; it computes statistics, maintains separate run sessions and writes real
idempotent archive records. Docker runs every code/agent sandbox. Temporal drives execution, and
PostgreSQL stores commands, requests, projections and resource metadata. These acceptance tests do
not replace any of those components with mocks.

Durable operation journals confirm 8, 8 and 9 model turns for the three agents. For each agent, the
test correlates actual model-requested calls with completed `files.read`, `files.write`, Python
`process.exec` and MCP `dataset.stats` operations. It checks the run marker in file contents,
process stdout and MCP results, a successful process exit, and the expected MCP statistics. Every
agent finishes with a solitary valid finish call and one committed report artifact.

Assertions cover rejected invalid human responses, valid approval, a skipped switch branch, ordered
concurrent foreach outputs, a two-iteration loop, subpipeline permissions, archive idempotency,
separate MCP sessions, overlapping work across runs, downloaded artifact bytes and checksums,
absence of other runs' markers, and container cleanup.

`TestRealProcessRecovery` starts the actual engine composition in a separate process, publishes an
artifact containing a random nonce, then sends `SIGKILL` while a human request is open. It restarts
against the same database, data directory and task queue. The engine identity, request identity and
deadline, artifact bytes, and SSE history must remain unchanged. The CLI answers the request; the
completed code must not run again. A second run is cancelled through the CLI, its late answer must
receive HTTP 409, and its downstream code must never start.

## Scope of the evidence

These are functional and recovery checks, not throughput benchmarks. The report profile explicitly
caps each run at six concurrent nodes, 128 node instances, 40 model calls, 100 tool calls, and a
30-minute deadline. Ollama may serialize inference even while the engine executes independent work
concurrently.

The engine currently implements the Ollama model adapter, and the live model acceptance uses
`qwen3.5:9b`. OpenAI and Anthropic adapters are not implemented; provider examples in a contract do
not establish runtime support. The functional MCP service exercises HTTP sessions and idempotency;
separate adapter tests exercise stdio transport and sandbox/network restrictions. A passing scenario
is evidence for these paths, not a guarantee for every external service or every possible pipeline.
