# Temporal in Knotra

The implementation review and verification were completed on October 3, 2026. The Go SDK
`go.temporal.io/sdk v1.49.0` is used; the behavior source for specific APIs is the pinned SDK
version in `go.mod`. Engine startup instructions are located in [running.md](running.md).

On October 4, a re-run of Go race/integration checks was performed after the readability review. The
AST workflow code was preserved; results of new desktop checks and reproduction commands are in
[verification.md](verification.md). Numerical history metrics below refer to the original run on
October 3.

## Workflow and activities boundary

One Knotra run — one Workflow ID and a chain of Temporal Run IDs with Continue-As-New. The workflow
`knotra.run` executes a deterministic graph: dependencies, CEL, branching, loops, limits, deadlines,
and request state. Nested `pipeline` creates a graph scope, not a separate Temporal child workflow.

The workflow does not access PostgreSQL, models, MCP, Docker, or the file system. External work
resides in activities. Collections affecting commands are handled in stable order; `workflow.Now`,
`workflow.Go`, selectors, and durable timers are used. Results of external actions during replay are
taken from history. Determinism requirements are described in
[Temporal Workflow Definition](https://docs.temporal.io/workflow-definition).

| Activity                             | Responsibility                                                 |
| ------------------------------------ | -------------------------------------------------------------- |
| `knotra.plan`                        | Load the immutable admitted plan by Run ID                     |
| `knotra.execute`                     | Execute one attempt of `llm`, `agent`, `code`, or `tool`       |
| `knotra.project`                     | Idempotently save state and event                              |
| `knotra.request`                     | Create or close a human/resolution request                     |
| `knotra.answer`, `knotra.resolution` | Resolve a deadline/cancellation race with an accepted response |

One agent attempt executes within one activity. Model moves and tool calls have separate log entries
in PostgreSQL but do not create a separate Temporal activity for each move. If an unfinished
workspace is lost, the runtime does not declare recovery from a missing checkpoint: an unknown
outcome requires a contract-provided stop or operator confirmation.

## What is passed and where it is stored

The initial production `RunInput` contains Run ID, admission time, and inputs. The full plan is
loaded via `knotra.plan`. With Continue-As-New, the same identifiers, inputs, and checkpoint are
passed; the package and plan are not re-included.

`ExecuteRequest` contains the current normalized node, its verified inputs, tool arguments, budget
chain, permissions, and deadline. The full plan and package are absent from the activity request.
The activity host retrieves them from the immutable run record. The prompt is included in the
specific node description and may be passed as a large payload.

The Payload codec moves serialized payloads of **8 KiB** or more to the persistent catalog
`payloads` inside `--data-dir`. In Temporal, only the encoding marker and SHA-256 key remain. The
entire original protobuf Payload, including metadata, is preserved. Identical result `knotra.plan`
yields one content-addressed blob across all run continuations. Saving is atomic; the file is
synchronized before publishing the link; reading is limited by the size of an open regular file and
checks SHA-256.

The codec operates at the SDK boundary, outside the deterministic workflow. This is local persistent
storage, not a cache. Replay requires Temporal history and these files; continuing external work
also requires PostgreSQL and artifact storage. Payload files must not be deleted while corresponding
history is needed. Automatic collection of unused payloads after history deletion is not yet
available.

There is no separate hidden 256 MiB limit on the aggregated checkpoint: multiple admissible node
outputs can collectively exceed such a size. The codec does not make state free: serialization and
replay still require RAM, CPU, and disk space proportional to live state. Significant byte volumes
should be passed as artifacts. Package, JSON port, and artifact limits continue to be checked at
their boundaries.

## History and command size limits

Per [official Temporal limits](https://docs.temporal.io/evaluate/cloud/limits), the history limit is
51,200 events or 50 MB; warnings start at 10,240 events or 10 MB. The single payload limit is 2 MB;
history transactions and gRPC messages are 4 MB. For self-hosted installations, account for the
actual service configuration; do not raise limits instead of managing history growth.

Knotra requests Continue-As-New at 8,000 events, 16 MiB history, or earlier if the SDK reports
`GetContinueAsNewSuggested()`. After this, new actions are not admitted. Already started attempts,
state records, and responses complete; transition occurs without active leaf attempts, human waits,
or undefined operations. Processed outputs, skips, counters, original deadlines, loop positions, and
foreach order are preserved. Late signals are consumed before transition. This approach follows
[Go Continue-As-New](https://docs.temporal.io/develop/go/workflows/continue-as-new).

The implementation limits to 64 concurrently executing leaf attempts, 64 metadata activities, and 64
pending human requests. Additionally, history space is reserved for permitted retries of already
admitted attempts. This is backpressure, not rejection of a graph with many nodes; narrower profile
limits are preserved.

The node timer is created after saving `ready`/`running`, so the state write queue limits and new
timer commands flow. The deadline is calculated before this write: the queue does not give the node
additional time. An already expired node does not start external work. A small inline payload
threshold leaves room for several dozen simultaneous arguments, results, timer commands, and service
metadata in a single transaction. This is also checked against real command-event batches; assessing
only the size of a single argument would be insufficient.

## Why timers and waits are used

`workflow.Sleep` in Temporal — durable timer, non-blocking Go goroutine sleep. The timer is recorded
in history and survives worker stoppage. Knotra uses an equivalent
`NewTimerWithOptions(...).Get(...)` to give the timer a clear label in the UI. Purposes: start
deadline, node/sub-pipeline deadline, and permitted retry backoff. There is no polling-loop with
`time.Sleep` inside the workflow. Waiting for a dependency or user response uses
`workflow.Await`/signals. See
[Go durable timers](https://docs.temporal.io/develop/go/workflows/timers).

The start of the root run deadline — durable admission time, so the outbox queue, worker wait, and
plan load do not extend the timeout. In a race with a user response, the workflow checks the already
recorded in DB response: outbox delivery delay does not cancel a timely decision.

## Retries and publishing

Automatic Temporal retry (`MaximumAttempts: 1`) is disabled for `knotra.execute`. Notational retry
policy is executed by the workflow after adapter result classification. An unsafe external call
cannot be invisibly retried due to activity timeout or worker loss. The log records intent before
call and result before confirmation; an error fixing a successful external result means unknown.

State-storage activities have a different policy: `StartToCloseTimeout: 30s`, retries with limited
backoff and without common `ScheduleToCloseTimeout`. Temporary PostgreSQL unavailability does not
turn an already selected result into a lost publish after five minutes. The write uses disconnected
context; external work deadlines are preserved meanwhile. An unavailable storage must be restored by
the operator — retry does not fix a lost DB or damaged disk.

A Knotra business error is published as failed run and returned to `RunResult`. Therefore Temporal
`COMPLETED` means completed orchestration; pipeline success is checked via Knotra run/result, not
only Temporal status.

## Display and updates

Workflow ID remains a stable run UUID. `StaticSummary` shows `Knotra · <metadata.name>`,
`StaticDetails` — name and Run ID. Activity summaries show node type, Node ID, attempt number, or
saved state. Timers have labels like `Node deadline: review` and `Retry backoff: enrich`. Prompts,
inputs, and secrets do not fit into these labels.

The plan fixes compiler/CEL/adapter versions. An incompatible version is rejected before external
work with diagnostics. This is not a replacement for Temporal Worker Versioning: changing workflow
command order requires replay-check and separate compatibility/deployment strategy. Automatic
seamless migration of running runs between arbitrary workflow versions to the current release is not
included.

## Execution cost and checks

Production activity currently reads and decodes full `db.Plan(runID)` on every leaf call. Thus,
PostgreSQL traffic and decoding cost grow as plan size × number of leaf attempts, although arguments
and Temporal history remain small. No plan cache exists. Before adding it, measurements are needed;
a safe cache must have memory limits and preserve data immutability between runs.

`TestTemporalLargePlanAndHistoryContinuation` works with real local Temporal: 1 000 switch nodes,
four leaf nodes with 900 000 byte prompts and a package with 3 600 000 byte supporting files.
External work of leaf activities in this test is replaced by a controlled response. It checks
orchestration, no re-execution, continuation, external payload, plan deduplication, and UI
summaries; this is not a PostgreSQL, Docker, or model benchmark. Time and history volumes are output
to the test log.

The last verified run finished in 106.98 seconds. Four histories were obtained, i.e., three
Continue-As-New; all 1 004 nodes published success exactly once, four leaf activities executed once
each. The history stores 14 references to external payload, full plan in all continuations uses one
blob.

| History | Events | Event Size, bytes | Largest command events group, bytes |
| ------- | -----: | ----------------: | ----------------------------------: |
| 1       |  4 099 |           714 417 |                                 522 |
| 2       |  4 414 |           822 458 |                              33 360 |
| 3       |  4 513 |           885 295 |                              33 216 |
| 4       |  1 661 |           306 021 |                              33 216 |

Group size is sum of protobuf sizes `ActivityTaskScheduled`, `TimerStarted` and `TimerCanceled` with
one `WorkflowTaskCompletedEventId`; this is observation of history commands, not measurement of full
network RPC. Numbers depend on batching and Temporal configuration. Test requires less than 2 MiB
for such group and less than 12 000 events/16 MiB for each history. Replay of all four completed
histories by same workflow passed without executing activities in 0.98 seconds.

```sh
KNOTRA_TEST_TEMPORAL_STRESS=1 go test ./internal/engine \
  -run TestTemporalLargePlanAndHistoryContinuation -count=1 -v
KNOTRA_TEST_LARGE_PAYLOADS=1 go test ./internal/engine \
  -run TestPayloadCodecLargeAggregateHasNoHidden256MiBCap -count=1 -v
go test -race ./internal/engine
```

Default endpoint `127.0.0.1:7233`, namespace `default`; overrides — `KNOTRA_TEST_TEMPORAL` and
`KNOTRA_TEST_NAMESPACE`. Stress test stores payload in `.knotra/temporal-history/<workflow-id>` so
recorded history remains readable after test completion. Separate codec test checks round-trip **269
484 032 bytes (257 MiB)**. Unit regression with virtual time checks publish after six-minute DB
unavailability without repeating external action.

For replay, save the payload directory and test run history in Temporal:

```sh
KNOTRA_TEST_REPLAY_WORKFLOW=knotra-history-13d1678d-5281-403e-af98-1dba2672b7af \
KNOTRA_TEST_REPLAY_RUN_ID=01a10163-4fa0-7a6b-8a95-66611cc0471f \
go test ./internal/engine -run TestTemporalReplayRetainedHistory -count=1 -v
```

These are verified identifiers of the specified run; after a new stress test, substitute its
Workflow ID and **first** Temporal Run ID. Replay reads saved histories of the entire chain, does
not create a new run, and does not invoke activities.
