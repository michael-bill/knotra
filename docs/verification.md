# Verification

Knotra uses deterministic contract/workflow tests, opt-in service integration tests and complete
CLI/browser executions. Passing examples establish the paths below, not throughput or universal
model quality. Default checks need no paid API credentials.

## Current examples: 5 October 2026

### Game generation and correction

The game uses actual local `qwen3.5:9b`, Docker/Node, Temporal and PostgreSQL. Repair run
`a4cbb411-34dd-44f9-8563-c754b283e97b` completed in two loop iterations with no diagnostics:

1. The supplied, deliberately incorrect opponent lost against a legal sequence of moves.
2. The model received that exact failure and source; its correction passed all 3159 checks across
   929 positions and 236 completed games.

A separate sandbox verified the final source before producing the playable HTML and test report.
Source SHA-256: `f46794bbb02cd5d1248ac671eab2aa6504cdf20a3b9c91d1679cbb71029ffcfb`. The downloaded
game ran in a browser with networking denied; a human move, computer reply and restart worked
without page errors. Generation from scratch also passed in one iteration, run
`fe7f4980-70b1-41ea-bf7c-6fb09b9cb380`.

An earlier repair run reached the four-iteration limit after repeated model syntax errors. It
published no playable game. The prompt now distinguishes the opponent helper from local variables. A
default-input smoke run also exposed validation of an empty input before a skipped branch; the seed
input now permits empty text, and the successful default run above verifies that correction.
Failures remain in execution history.

Reproduce [generation](../examples/starter/tic-tac-toe/pipeline.yaml) and the
[seeded repair exercise](../examples/showcase/game-repair/README.md). Fixed tests cover all winning
lines, terminal states, move legality, immutability, repeatable choices and every legal opponent
continuation against the tested computer implementation as both X and O.

### Service fleet release gate

Run `920bc2ea-76a1-4c72-8d02-7f90809edeed` completed all 30 nodes with no diagnostics or browser
errors. Six executable sample services passed 36 regression tests and 18 release gates. Three agents
read scoped evidence and the release policy; code verified their citations. A human response with a
change ticket authorized the evidence packet.

The exported ZIP was checked against the actual reviewed bytes and input snapshot:

| File                | SHA-256                                                            |
| ------------------- | ------------------------------------------------------------------ |
| Repository snapshot | `bdd4fe2f79ac52d61005d21241b363f4008e9a125a15386502272f9f48e55333` |
| Executed checks     | `b7ac344c6adf6221e30a2d592c34690b761793156c7490903f1a76d489b9b257` |
| Reviewed dossier    | `64ce4cac59609f74241382e1f8860328b5670fda1f7f9e297bd3b0d6f6edf31e` |

A separate snapshot widened an existing API response enum. Run
`fd7f0b74-5b63-40c0-bc45-30f3c890b816` failed at the deterministic gate: no agent started, no human
request appeared and no authorized packet was published.

The [package](../examples/showcase/release-gate/README.md) includes 11 deterministic tests. They
execute the sample regressions and exercise incompatible/unsupported API schemas, unsafe or
ambiguous configuration, archive confinement, incomplete service coverage, mixed snapshot hashes,
invented citations and byte-preserving publication.

The [screenshots](../README.md#workflow-in-pictures) show these real executions in light theme. The
graph was captured during work; the agent inspector shows two model calls and actual evidence reads.
Inbox's prompt, deadline, files and details now share the header/form padding. Browser measurements
confirmed aligned left edges; four affected review/artifact checks passed, including long IDs at
1024/1440 px and preservation of a response draft after failed export.

## Providers and recovery

Ollama has passed real structured-output, tool and complete-workflow checks. OpenAI Responses and
Anthropic Messages use literal HTTP/SSE fixtures for structured arguments, tool IDs, private
conversation context, malformed/incomplete replies, credentials, HTTP failures, cancellation,
budgets and durable replay. **Paid APIs were not called:** keys were unavailable. Live cloud checks
require a key and an explicit model ID.

Both fixture providers also passed with real Docker tools and real Temporal/PostgreSQL recovery. The
engine was killed while waiting for a human and restarted; request identity remained unchanged and
each provider received exactly one generation request. Reviewed research runs additionally checked
SIGKILL recovery, SSE replay, valid/late human responses, cancellation and byte-identical
publication. These paths are covered by the reproducible integration tests below.

The broader real-service suite exercised three concurrent pipelines with all nine node types, actual
Ollama, Docker, MCP, Temporal and PostgreSQL. A separate Temporal stress test completed 1,004 nodes
through three Continue-As-New transitions; retained histories replayed. Its external activities were
controlled fixtures, so this is history evidence rather than a model throughput benchmark.
[Temporal details](temporal.md) record the measurements.

The v0.1.1 release passed Go checks, desktop/docs checks and four CLI archive builds. Archives
contain helpers for both Docker architectures. A standalone quickstart produced its greeting and
retained the same run on restart. A release-test scheduling assumption was corrected without
changing production scheduling; 200 focused race repetitions and `make check` passed.

## Reproduce the checks

From the repository root:

```sh
make check
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s examples/showcase/release-gate/tests -v
```

Frontend and native commands are in [CONTRIBUTING](../CONTRIBUTING.md). Optional browser acceptance
uses `KNOTRA_E2E_ENDPOINT` and submits packages through the same API as desktop.

For real services, use a disposable database and the addresses/helper/work directory described in
[Running Knotra](running.md) and [integration setup](../examples/integration/README.md). Then enable
only the checks needed:

```sh
KNOTRA_TEST_REAL=1 go test -mod=readonly -race -count=1 -timeout 30m \
  ./internal/integration -run '^TestReal(ConcurrentPipelines|ProcessRecovery)$' -v
KNOTRA_TEST_RECOVERY=1 go test -mod=readonly -race -count=1 -timeout 10m \
  ./internal/integration -run '^Test(CloudProtocolsWithRealEngineRecovery|RealProcessRecovery)$' -v
KNOTRA_TEST_TEMPORAL_STRESS=1 go test -mod=readonly ./internal/engine \
  -run '^TestTemporalLargePlanAndHistoryContinuation$' -count=1 -v
```

## Boundaries

The release sample runs real code, but its services and digest strings are demonstration data. Its
API adapter covers a conservative subset, and deployment policy inspects supplied configuration; it
does not query a cluster, scan an image or establish release safety. Agent recommendations remain
advice. Approval records typed reviewer/ticket fields rather than authenticated identities or
digital signatures, and publication does not deploy services.

Quickstart runs on one controlled host. Shared storage, multi-user access, retention and workflow
upgrade policy remain separate work. Recovery preserves workflow state and completed results; it
does not promise restoration of a live agent workspace or rollback of external effects.
[Execution semantics](notation/execution.md) define retries and unknown outcomes.
