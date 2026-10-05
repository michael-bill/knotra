# Verification

The implementation is checked at three levels: deterministic contract and workflow tests,
integration tests against PostgreSQL and Docker, and complete executions through the public CLI/API
with real services.

## Providers, onboarding and reviewed research on 5 October 2026

`make check` passes Go formatting, vet and the full race suite. Default cloud tests use literal
OpenAI Responses and Anthropic Messages HTTP/SSE fixtures. They cover fragmented structured
arguments, tool IDs and private conversation context, malformed/incomplete replies, credentials,
HTTP failures, cancellation, budgets and durable replay. Real paid APIs were **not called**: no API
keys were available. Opt-in tests require both a key and an explicit model ID.

Both fixture providers also passed with real Docker agent tools and real Temporal/PostgreSQL process
recovery. The engine was killed at a human request and restarted; the request ID remained unchanged
and each provider received exactly one generation request. The existing real recovery test also
passed its artifact, SSE replay, CLI response/cancellation and late-answer checks.

Actual local `qwen3.5:9b` passed structured-output/replay and agent-tool tests after the shared
adapter refactor. A complete reviewed research run analyzed the supplied fictional offers, verified
evidence in code, paused for review, survived SIGKILL and accepted its response through the browser.
Run `8339dfde-b8e5-4059-b25e-cf67cc4bafd0` completed with no diagnostics. Published
`approved-dossier.md` remained byte-identical to the reviewed draft, SHA-256
`955dee8f62a9ff9a4ba7d6c723f5bf45fb434f999576463c10921f305fb65aae`. The ZIP's dossier and comparison
matched the source hashes in `review.json` and the original review context. A second real run,
`cd93b2a8-8494-46a3-a576-1e7454b30538`, records the corrected Inbox and recovery flow at 2880×1620.
Its saved request and both review file hashes were compared before and after SIGKILL. The video uses
lossless source screenshots and one H.264 encode with visible English captions.
[Recorded demonstration](media/review-recovery.mp4).

The first archive smoke check found that Colima could not mount a helper from an unshared temporary
extraction directory. Quickstart now copies the verified helper into its persistent directory under
its content digest and makes it executable for sandbox UID 65532. The failed run remains in history;
the corrected run above verifies publication after this fix. A fresh standalone quickstart produced
`greeting.txt`; repeating it retained run `f5b3b014-c14c-4fca-b0cd-f57c97638a87` and the same file.
All four macOS/Linux CLI archives include both Linux helpers and `SHA256SUMS`.

Frontend checks passed 126 unit tests and the production build. The browser suite passed 70 default
scenarios before the Inbox correction; the affected review/translation checks then passed all eleven
cases. These include long-ID layout checks at 1024 and 1440 px, missing run metadata, and rejection
of corrupted file bytes without losing the human response draft. The actual recorded browser run
reported no page errors. Existing paid/cloud and other opt-in browser scenarios remain separate.

The five-developer pilot is prepared in [pilot](pilot.md); no user interviews or usefulness metrics
are claimed by these automated checks.

Reproduce cloud fixture recovery using the database and Temporal variables from the integration
setup, plus `KNOTRA_TEST_RECOVERY=1`:

```sh
go test -mod=readonly -race -count=1 ./internal/integration \
  -run '^Test(CloudProtocolsWithRealEngineRecovery|RealProcessRecovery)$' -v
```

For real Ollama/Docker adapter checks:

```sh
KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
KNOTRA_TEST_WORKDIR="$PWD/.knotra/test-work" \
KNOTRA_TEST_OLLAMA=http://127.0.0.1:11434 \
go test -mod=readonly -race -count=1 ./internal/adapters \
  -run '^Test(CloudDockerAgentToolsValidationAndReplay|CloudStructuredStreamingObservations|OllamaRealStructuredAndAgent|OllamaRealStructuredWithoutDocker)$' -v
```

## Live execution inspection on 4 October 2026

Real engine runs now open a live graph with per-instance states, durations, nested iteration scopes,
waiting reasons and selectable connections. The node inspector shows streamed Ollama output, prompt
context, agent iterations, tool arguments/results, validation and artifact previews. It separates
retries, marks incomplete/truncated observations and preserves the longer streamed answer when a
bounded completion preview arrives. Recorded state can be rewound without executing effects; run
comparison matches nested instances by structural ancestry rather than runtime IDs.

Adapter tests cover incremental NDJSON, cancellation, malformed/incomplete streams, operation
journal replay, telemetry failures and credential redaction across chunk boundaries. Workflow tests
cover legacy version gates and checkpoint continuations. PostgreSQL tests cover ordered observation
commit/cursors, scoped pagination, bounded port previews and numbers outside the desktop safe range.
The Go vet/race suite passed with local PostgreSQL, as did the native real-engine acceptance test.

Browser acceptance uses actual local `qwen3.5:9b` for both an LLM pipeline and a multi-step agent
that writes/reads a file and finishes through its structured output tool. It verifies live deltas
before completion, durable node history, token timing, agent/tool cards and produced artifacts.
Contract fixtures additionally test duplicate delivery, tool errors, waiting-resolution actions,
historical rewind and comparisons. These checks use an isolated development database and engine on
port 8887.

The frontend unit suite passed all 105 tests, and all 60 Playwright scenarios passed with the real
endpoint enabled. Native tests passed (26 default tests plus the opt-in real-engine test).
TypeScript/Vite, Prettier, gofmt, rustfmt and the macOS application bundle build passed. The local
Rust toolchain warned that debug-symbol stripping could not load `libLLVM.dylib`; bundling still
completed successfully. Vite retains its existing large-bundle warning.

## Interface localization on 4 October 2026

The frontend now uses separate English and Russian JSON catalogs with stable semantic keys. The
selected language is saved with the workspace, and older workspaces/backups retain their content
while falling back to the browser or webview language. Authoring data and engine messages are
preserved; application-authored frontend validation messages are translated for display. Unknown
library/parser error details stay verbatim.

All 88 frontend unit tests passed, including catalog key coverage, matching named parameters,
verbatim interpolation and legacy backup compatibility. All 51 browser scenarios passed with the
real engine endpoint enabled, including both Ollama/human execution scenarios. Two language
scenarios check live switching, reload persistence, accessible names, editor search, theme keyboard
focus, malformed engine URLs and empty resource tabs. They also compare saved authoring content,
demo history and engine cache bytes before and after switching. A pipeline named
`navigation.settings` remains that literal name in both languages.

The TypeScript/Vite production build, repository Prettier check and macOS application bundle build
passed. A manual run through the Russian interface completed with the unchanged model output
`Hello, Knotra!` and a 15-byte artifact (run `19e413be-0555-4c5a-b560-309c8e249b7e`). The initially
stopped development engine was restarted before the final acceptance run. No backend code, SQL
migrations, pipeline fixtures or dependency lockfiles changed for localization.

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

At that review, the real execution graph, agent message view and token streaming were still future
work; the live execution inspection work above implements them. At that review, Ollama was the only
model provider; the provider work above adds OpenAI and Anthropic. No SQL migrations or dependency
lockfiles changed in this re-review. The reproduction commands below apply to both reviews.

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

The engine implements Ollama, OpenAI Responses and Anthropic Messages. Live model acceptance uses
local `qwen3.5:9b`; OpenAI and Anthropic are checked with protocol fixtures and real engine
recovery, without paid API calls. The functional MCP service exercises HTTP sessions and
idempotency; separate adapter tests exercise stdio transport and sandbox/network restrictions. A
passing scenario is evidence for these paths, not a guarantee for every external service or every
possible pipeline.
