> MVP scope updated 2026-10-06: River is now the default. Normal Compose and quickstart require
> PostgreSQL only. Temporal remains an explicit legacy option for baseline tests. Cluster deployment
> and exhaustive release gates below are deferred; their historical status does not block the
> single-process MVP.

# Scheduler migration implementation status

Updated October 6, 2026. This document records the historical full
[scheduler and River migration plan](scheduler-river-plan.md). The current release scope is a
single-process MVP. Default admission and service startup use River without connecting to Temporal.
The runtime executes DAGs and persisted nested controls; cluster deployment and exhaustive
performance acceptance remain future work.

## Implemented and verified

### River adoption prototype

River OSS and its pgx driver are pinned to **v0.48.0**, which uses the repository's pgx v5.11.0. The
PostgreSQL main migration target is **8**. `internal/queue` runs migrations individually under a
dedicated connection's advisory lock. Both interrupted migration resume and competing migration
clients pass with application pools configured for one connection.

Advance, execute, finalization and cleanup arguments contain identities and routing versions.
Separate orchestration, execution and maintenance queues retain independent capacity. Execute jobs
have `MaxAttempts: 1`; orchestration and maintenance deliveries can retry infrastructure failures.
Uniqueness includes arguments and queue, with completed jobs included. New generations remain
insertable while old completed jobs are retained. Fetched arguments are validated before invoking
handlers. All domain handlers are required when constructing a working client.

`internal/execution` provides stable attempt and ownership identities and immutable local outcome
files. Publication synchronizes the file and its directory, never replaces an existing outcome, and
verifies checksums and identity on recovery. Concurrent identical publication is idempotent;
different evidence for the same ownership returns a conflict. Complete values are retained,
including JSON null and large outputs, independently of API observation limits. Outcome evidence
cannot replace external-operation response journaling.

The PostgreSQL prototype verifies:

- Transactional insertion and completion roll back alongside application records.
- Two competing River clients cause one domain claim and one fake external operation.
- Execution timeout discards the delivery at one attempt.
- A finalizer publishes a discarded execution's saved result once, including after queue retention
  removes the original finalization job.
- A worker durably saves its outcome while its real PostgreSQL connections are cut, then is killed
  with SIGKILL. Connectivity remains unavailable for six minutes. A replacement worker waits for
  River to discard the exhausted abandoned delivery and imports the outcome with one external
  operation, one publication event and one next wakeup.
- A 65-second execution finishes under its configured two-minute timeout, beyond River's default
  one-minute timeout.

The outage test uses a local TCP proxy to isolate the worker's database connection failure from
other schemas and the test's administrative connection. The abandoned delivery is aged past the
configured rescue horizon before replacement startup; the test does not wait an hour for rescue. The
successful six-minute run recovered after **6m4.39s**. These are prototype guarantees; the
production scheduler, lease renewal lifecycle and ownership-aware adapter hooks still need their own
process crash matrix.

Client configuration uses a finite execution timeout and a rescue horizon one hour beyond it. Short
advance and maintenance handlers have separate 30-second timeouts. Admission must enforce the
configured maximum attempt duration before this client becomes a production execution backend.
Shutdown drains for the caller's bounded context, then cancels active handlers with a separate grace
period. River's notification listener hijacks a connection from pgx and manages it independently;
`MaxConns: 1` therefore does not mean only one total PostgreSQL connection.

Primary API references: [migrations](https://riverqueue.com/docs/migrations),
[transactional completion](https://riverqueue.com/docs/transactional-job-completion),
[uniqueness](https://riverqueue.com/docs/unique-jobs) and
[maintenance](https://riverqueue.com/docs/maintenance-services).

### Neutral domain and compatibility foundation

Execution messages, classified failures and pure binding, switch, collection, carry, permission,
limit, deadline and stable-identity helpers now live in `internal/execution`. The legacy engine uses
compatible aliases and wrappers. Its checkpoint and workflow control remain in `internal/engine`.
Storage depends on the neutral messages. `internal/executor` hosts adapter execution and durable
hooks; the Temporal activity retains its SDK-specific heartbeat shim. Optional ownership tokens are
omitted from legacy execution-request JSON, and all four retained histories still replay after their
addition.

`internal/execution/executiontest` contains **23 scenarios** with an explicit start time, fixture
leaf results, timed human/resolution/cancellation actions, expected exports and failures, leaf
invocation counts, terminal branch states and inherited scope checks. All nine node types are
represented. The Temporal runner passes all scenarios, including optional absence versus JSON null,
ordered branching, foreach order and empty input, simultaneous loop carry, loop limit modes, human
validation, safe retry, retry deadlines, unknown-outcome evidence and materialization budgets.

The new River scheduler passes the same 23 scenarios at transition bounds 1, 2 and 64. These
controlled-clock cases are complemented by `TestLiteralBackendCompatibilityAndNestedBudgets`, which
runs the same package and real HTTP/MCP providers against both application services. It does not
replace existing semantic, request, provider, sandbox or recovery tests.

The literal suite contains **seven scenarios on each backend**: exact budgets and independently
exhausted root/child model, MCP-tool and code-call budgets. Each package delegates model, MCP and
sandbox permissions into a nested pipeline, then executes LLM, agent, standalone MCP tool and Python
nodes in order. The successful case ends with a real CLI human response and root exports. Its
**175,000-byte input** is checked in every generation request and MCP argument, the Python output,
root export and downloaded artifact. MCP writes append and sync an external fixture file; Python
actually runs in Docker and creates the exported artifact. No execution callbacks substitute for
these effects.

| Scenario                            | Physical model calls | External MCP writes | Code calls | Root and child model/tool budgets |
| ----------------------------------- | -------------------- | ------------------- | ---------- | --------------------------------- |
| Exact limits                        | 3                    | 2                   | 1          | 3 / 3                             |
| Root or child model limit           | 2                    | 1                   | 0          | 2 / 1                             |
| Root or child standalone-tool limit | 3                    | 1                   | 0          | 3 / 1                             |
| Root or child code-call limit       | 3                    | 2                   | 0          | 3 / 2                             |

Observations compare final status, leaf states, physical model calls, external writes, confirmed
operation journals and both persisted budget scopes. Every exhaustion case identifies the rejected
scope, preserves the completed prefix, cancels downstream nodes and publishes no root exports or
artifacts. Child budget rejection rolls back the earlier parent debit; root exhaustion does not
debit the child. Root-limit cases omit child limits so the child inherits the parent ceiling;
explicitly raising a child ceiling is rejected at admission.

All **14 real-service cases passed with the race detector** in **73.74s** (including process
startup/teardown). `make check` passed with zero lint findings, and all four retained Temporal
history segments replayed again with no external activity calls. This closes the literal provider
and budget-observation gap in phase 2; the full process/client/resource acceptance matrix remains
open.

A real Temporal stress baseline completed **1,004 nodes**, with four leaf executions and four
history segments, in **75.19s**. All four retained histories replayed before and after extraction
without external activity calls. The retained workflow is
`knotra-history-85b24c65-53d3-4870-af96-b51eb1aeea5e`, with first Temporal run
`01a10be5-7613-794b-bc6c-140b7e1ee652`. Payloads remain in the ignored `.knotra/temporal-history/`
directory for further compatibility checks.

### Authoritative persistence foundation

The initial `internal/store/schema.sql` includes backend/version metadata, database admission time,
original execution deadline, revision, event sequence, wake generations, admission pause and stop
cause. Individually changing graphs, nodes, attempts, controls, scopes, slot reservations, timers
and worker identities have separate tables, domain uniqueness constraints and relevant partial
indexes. No execution evidence depends on River job retention.

`PutRiverRun` persists a new immutable admission, root graph, root budget scope, deadline timer and
initial transactional queue delivery. It creates no Temporal start command and never converts an
existing run. A savepoint rolls back partial admission on enqueue failure even if the outer caller
commits. `WakeExecution` provides the same protection for generation updates. `ApplyWake` ignores
consumed generations and rejects future generations. `LockExecutionRun` captures database time after
acquiring the run lock; a real lock wait crossing a deadline passes its regression check.

`ProjectTx` composes the existing idempotent public projection inside a transaction.
`ProjectExecution` allocates a scheduler event sequence in that transaction. PostgreSQL tests verify
admission and queue rollback, projection and wakeup rollback, stale deliveries, incompatible
versions and database-clock admission. Graph and node records retain full values, immutable original
deadlines and transition revisions; terminal records cannot be rewritten by later scheduler steps.

### Attempt ownership and physical-call admission

Worker registration uses a fresh process incarnation. Worker heartbeats and attempt leases expire
after 20 seconds; renewal checks database time after locking, and cannot resurrect an expired owner.
Draining workers cannot claim new attempts, while their admitted work can finish. Scheduler and
state format versions are checked before claiming and before new effects.

`ClaimAttempt` locks the run and current attempt, verifies frozen request/scope identities and the
original deadlines, reserves every enclosing scope in lexical order, and persists ownership and the
expected outcome key before returning. The caller still must commit before execution. The running
node state, timestamp, public projection and event commit with the claim. Duplicate delivery and
stale dispatch generations cannot claim work. Capacity misses leave the same business attempt ready
and marked for redispatch. `ReleaseAttemptSlots` releases once for the matching ownership generation
and commits its next wakeup atomically; injected enqueue failure rolls back the release even if an
outer caller commits.

Operation records retain attempt ownership and a physical admission timestamp. A model/tool
operation can be prepared without a budget debit; immediately before the physical call,
`AdmitOperation` checks ownership, cancellation, stop state and deadlines, then commits the
operation intent and every ancestor call budget together. Provider digest checks and MCP preparation
precede this gate. Completed journal responses replay without another call or debit. Agent workspace
and run-session lifecycle intents remain admitted before their creation. Legacy journal/budget
methods cannot bypass ownership on River runs. Node timestamps/reasons and child iteration metadata
remain independent of bounded public projections.

The host requires the matching token for River attempts and guards operation responses and artifact
metadata. Loss of ownership after an external call preserves an unknown outcome; the immutable
envelope does not turn an unjournaled response into success. The River runtime now uses these hooks
with separate worker heartbeats, attempt renewal, bounded shutdown and outcome-file recovery. Opt-in
service wiring is implemented; default cutover and the full process crash acceptance remain pending.

PostgreSQL and literal HTTP fixture checks pass for:

- 36 competing deliveries obtaining two root reservations and one reservation per child scope.
- Claim/projection rollback, stale dispatch, incompatible/draining workers, cancellation, stop
  cause, lease expiry, deadline lock waits, idempotent slot release and redispatch markers.
- 24 competing operation admissions permitting one physical call and one debit per scope.
- Ancestor budget failure rolling back both the prepared operation's admission and earlier debits.
- Completed provider response replay, missing-token rejection, and expiry during provider
  preparation preventing generation and budget consumption.
- Lease loss after a provider response retaining one unconfirmed operation, one budget debit and an
  immutable unknown-outcome envelope, with no repeated call.
- Real Temporal service process restart with literal OpenAI and Anthropic providers, stable human
  requests and exactly one generation per run. Both cases pass in **17.67s** combined.

### Bounded scheduler and outcome publication

`internal/scheduler.Step` makes pure decisions for all nine v1 node types. It bounds
materialization, node and control transitions, counts skipped nodes against materialization limits,
evaluates bindings and readiness from full authoritative values, validates outputs and completes
graphs. The shared compatibility suite passes **all 23 scenarios** at transition bounds **1, 2 and
64**, including empty foreaches, ordered collection, simultaneous loop carry and both loop limit
modes.

Execution records persist control revisions, the graph scan cursor and the need for a fresh pass.
Foreach elements, the next body position, loop carry/iteration and admitted child identity survive
repeated delivery. Creating a child graph, advancing its control position, recording its deadline
and inserting its continuation commit together. Waiting human bodies count against foreach
concurrency while holding no leaf slots. Nested pipelines restrict permissions and limits and
reserve a separate scope; every leaf still reserves capacity and debits calls in all its ancestors.
Child deadlines inherit the parent's original deadline, and child pipeline timeouts start at parent
admission.

The runtime visits at most 64 graphs and makes at most 64 coordination transitions per transaction.
Its persisted scan progress catches newly created addresses behind the cursor without repeatedly
waking an unchanged human wait. Accepted responses and outcome publication prioritize their own
graph/node so unrelated work cannot consume their transaction allowance. Terminal run projection
waits for all child graphs and claimed attempts. These bounds cover graph visits and mutations.
Pipeline and loop child reads use the persisted current child identity and the graph primary key;
historical iterations remain durable without loading their full values on each step. Foreach
controls retain active/succeeded/failed child counters, updated with child insertion and terminal
graph changes in the same transaction. A failed-child index selects the first cause without loading
successful siblings. While bodies remain active, successful child values are not loaded for
coordination.

After every body succeeds, foreach validates ordered result pages of at most 64 children per control
and saves its collection cursor with the continuation. Child positions are unique and completed
graphs are immutable. Full port arrays/artifact collections are assembled by PostgreSQL only when
the bounded pure step selects the parent for completion; the scheduler then rereads database time
before applying that decision. No growing result array is saved on intermediate steps. The final
export still requires one complete value.

Prepared foreach controls read only the current element, element count, concurrency and persisted
cursors/counters. Their parent node reads omit the complete admitted inputs and execution request;
the runtime reconstructs the request from the frozen definition and loads only statically referenced
`args` values. A parent state change restores its original complete admission record before saving.
Graph metadata and selected child summaries omit unrelated inputs. Graph admission and exports load
only referenced input ports after initial contract validation. Initial validation, final exports and
an explicit whole-array binding still receive complete values.

Graph steps now select at most 64 eligible full node records before PostgreSQL serializes their
inputs, requests or results. Attempt, retry, current request, control and child reads are restricted
to that selection. Idle human waits and unchanged child controls do not consume the page. Initial
materialization persists each frozen static dependency and its outstanding count. A guarded terminal
transition releases only its reverse dependencies through a partial GIN index; those readiness
updates commit with the result and continuation, without rewriting dependent values. Admission loads
complete states/outputs for the selected nodes' dependencies. Graph completion checks for unselected
active nodes and loads only the static references of its export bindings, including every coalesce
candidate and checked CEL reference. Database time is refreshed after those reads. Current request
identity is persisted with node state, so retained resolution decisions cannot make a later open
request look answered. Every advance path, including outcome publication, applies the five-second
scheduling context.

A real PostgreSQL regression completes 1,005 switch/control nodes with a 1,004-way dependency join
and a 200,000-byte value. It verifies the 64-record selection ceiling, rollback of result/readiness,
reopening the store after committed progress, duplicate terminal rejection, delayed join admission
and complete exports. A second check verifies 71 idle human waits, retained unrelated decisions,
current-answer consumption and paged cancellation. These use direct production transactions; neither
is SIGKILL or a deployment resource benchmark. Full dependency fan-in and final exports still
require their complete values. PostgreSQL may still detoast the original JSONB to select an element
or input port. Frozen plans and literal arrays, candidate metadata scans, reverse-edge updates for
large fan-out, dependency values and final exports still need byte/time/resource acceptance. The
five-second context bounds execution time but does not prove a workload completes within it.

Human answers and external-operation resolutions consume the first valid accepted response in the
command transaction, using the original database deadlines. River commands wake the scheduler in
that same transaction without a Temporal signal. A run paused for multiple unknown operations
resumes admission only after the last decision in any graph; an already queued ready attempt retains
its redispatch marker when a pause prevents its claim.

`internal/queue.Runtime` connects these decisions to actual River deliveries and PostgreSQL
transactions. Claims commit before execution. The worker fsyncs an immutable outcome before
publication; the attempt record retains its complete evidence in PostgreSQL. Directory
synchronization uses the same OS-aware helper as the artifact store, keeping the confined open
directory handle. Windows syncs file contents but has no explicit directory flush; its metadata
durability remains filesystem-dependent.

Verified outcome publication commits the attempt result, node transition, event, artifact
visibility, slot release, next delivery and River completion together. Finalization can import an
expired owner's matching envelope, while cancelled or superseded nodes retain evidence without
accepting outputs. Known successful envelopes cannot bypass an unconfirmed external-operation
journal.

Deadline and business-retry timers use persisted database timestamps. Each timer-pump transaction
consumes at most 64 overdue records and inserts its wakeup together. A retry creates a new business
attempt only after its backoff, and retains the node's original deadline. A capacity miss changes
only the dispatch generation. Worker heartbeats run independently of timer/outcome scans. Expired
claims are scanned in bounded batches. Saved envelopes go to the finalizer. An expired claim with no
verified final result is fenced and staged as an unknown attempt result; the scheduler consumes that
result, releases ancestor slots and inserts the next delivery in the same transaction. The original
instance deadline and unknown-outcome policy remain authoritative. The first unconfirmed admitted
operation supplies its identity when present; otherwise the lost attempt itself identifies the
unresolved outcome. Even a completed provider response in the operation journal does not restart a
node or reconstruct an unfinished agent workspace. A lost agent prefix cannot become retryable after
`not_executed` evidence.

Checksum, format or ownership-invalid files also produce an unknown outcome after lease expiry;
their original bytes remain untouched. Other storage I/O errors remain retryable maintenance
failures, preserving the chance to recover a temporarily inaccessible verified result.

Late envelopes remain inspectable without replacing a resolution, cancellation or terminal node.
Lost effects after cancellation or deadline expiry produce retained diagnostic events and run
diagnostics. Partial indexes cover unconfirmed operation lookups and cursor-based outcome scans. The
remaining resource/session lifecycle and process crash acceptance are listed below.

The delivery reconciler scans up to 64 active dirty/ready runs and checks at most 64 ready attempts
per run transaction. It repairs a dirty run's missing advance delivery with a new wake generation;
it marks an unclaimed attempt for redispatch and inserts its wakeup together. Redispatch retains the
business attempt number and changes only the delivery generation. Those redispatches now also count
against the scheduler's 64-transition allowance. Original deadlines, pause and ancestor capacity
continue to guard new deliveries. Claimed attempts always use outcome/loss reconciliation rather
than execution redispatch.

Advance wakeups now share an existing viable, unconsumed delivery for the run. Every mutation still
increments the domain generation, and the older delivery consumes all committed generations under
the run lock. The reconciler recognizes this pending delivery instead of repeatedly increasing the
counter. Consumed/malformed/mismatched identities, future backoff, expired handlers and dead worker
heartbeats do not suppress immediate delivery. Human/resolution command transitions remain inside
the original receipt transaction. An eight-caller, 80-mutation regression retains all 81 generations
and one queued advance, then executes and exports its model result exactly once. The expanded repair
matrix includes future backoff, malformed arguments, wrong run identity and impossible future
generation. Request/deadline and recovery checks passed with race detection in **48.613s**; the four
additional defensive repair cases passed in **3.585s**. All nine physical result/stop/deadline races
still passed through real Docker execution in **20.58s**.

The [resource follow-up](scheduler-resource-benchmark.md#advance-backlog-diagnosis-and-fix) traced
the approximately five-second start delay to 80–97 queued advance jobs belonging to completed runs.
Six consecutive short DAGs now create 4–9 advance jobs each, rather than 130, and complete in
**1.80–2.25s**, with first physical calls at **156–231ms**. The original full comparison remains
archived as a baseline before this fix; full resource acceptance remains required.

The
[statement cost profile](scheduler-resource-benchmark.md#postgresql-statement-cost-and-database-clock)
attributes about 98% of measured WAL growth to statement records. The largest writes are durable
nodes, lifecycle/adapter events, public projections and native queue insert/fetch/completion;
Temporal's orchestration/history writes are outside PostgreSQL in the local baseline. This is cost
attribution, not complete deployment disk/I/O acceptance. The profile also identified about 1,330
avoidable database-clock round trips per 64-node DAG. `LockExecutionRun` now materializes the locked
row before evaluating the outer database-clock projection. The returned clock remains after row-lock
waiting, including a wait crossing a deadline. Three production repeats decreased median query calls
from **12,113 to 10,687** while preserving all 64 calls/debits/instances. Full resource budgets and
the remaining workload/volume measurements are still required.

The store's real PostgreSQL lock-wait, attempt deadline, ownership and operation-budget checks
passed with race detection in **3.496s**. All nine physical publication races passed after the clock
change. The request fixture now follows transitive PostgreSQL blocking relationships: a queued
tuple-lock holder may be the command's immediate blocker while waiting on the original owner. Its
four wait-past-deadline cases deliberately add that intermediate transaction and verify the returned
database clock after release. The complete 20-case request contention suite passed in **31.80s**
(**33.739s** package total). This corrects an observation timeout in the direct-edge fixture without
loosening deadlines or expected command results.

Advance and execute metadata contain their validated argument identities. The pinned River public
`JobListTx` API uses its existing metadata GIN index to check current queued/running deliveries,
without application SQL against River's schema. Completed, cancelled, discarded and running jobs
past their configured handler timeout do not suppress repair. Worker records bind the native River
client to its process heartbeat before fetching. A running delivery from a registered dead process
stops suppressing repair after the 20-second heartbeat lease, including a crash between fetch and
domain claim. Clients without a registered mapping retain the finite timeout fallback. A persistent
per-run ready cursor and its partial index ensure healthy queued attempts cannot hide later missing
deliveries. Process-local run scan cursors bound polls; all decisions and row changes are rechecked
under the run lock.

Real River/PostgreSQL checks, with race detection, verify:

- A three-leaf DAG with a forced concurrency miss, three physical provider calls, three budget
  debits, one success event per node and no Temporal start commands.
- Complete 150,000-byte string values consumed downstream while public instance context is
  truncated.
- Duplicate advance, execute and finalization deliveries after completed River jobs are removed,
  with no repeated physical call or event.
- Injected completion failure rolling back result/evidence rows, node transitions, artifact
  visibility, events and slot release; the fsynced envelope remains recoverable.
- Expired live-owner publication rejection, followed by idempotent verified-envelope recovery and
  complete artifact export.
- Human answer validation, first-answer and command-receipt replay, rollback after a failed wake,
  and cancellation preserving the already accepted terminal node.
- Two received model responses whose journal commits fail, producing two operator requests; invalid
  evidence outputs are rejected and admission resumes only after both valid decisions.
- Literal OpenAI HTTP 429 responses causing a fixed-backoff retry and two physical calls/debits; a
  replacement worker resumes the persisted backoff, while a shorter original deadline prevents the
  second attempt and call. This replacement check drains the first worker in the same test process;
  it does not replace SIGKILL acceptance.

- Foreach human waits with out-of-order answers, durable response receipts and a replacement process
  incarnation, preserving three child identities, ordered exports and four materialized instances
  with no leaf slots or attempts.
- Actual nested `pipeline → loop → foreach` execution with six literal provider requests and six
  call debits in each ancestor. A child budget of three stops further physical calls and leaves no
  ancestor slots reserved.
- 70 loop iterations advanced through production PostgreSQL transition transactions retain all 70
  completed graph records. The scheduler's child read returns only iteration 69 with its complete
  output, rather than reloading historical iterations. This check drives transactions directly; it
  does not measure native queue notification latency.
- A 130-item foreach performs no child-value reads while bodies are active, validates collection
  pages of **64/64/2**, rolls back an interrupted cursor update and resumes through a reopened store
  and new runtime. Final arrays retain input order despite lexical child-completion order. Attempts
  to change or recount a completed graph are rejected. These are production transition transactions,
  not SIGKILL evidence for the collection boundary.
- Native River/Docker execution collects three immutable artifacts in input order and verifies their
  bytes and absence of worker-local paths. An empty foreach returns an empty artifact collection.
- A 257-item foreach with a 4.2 MB source array reads one current element and only the referenced
  argument. Child creation rollback preserves the cursor; reopening the store resumes at the next
  committed element. The final ordered export retains all 4,214,544 bytes, and the original parent
  inputs/request retain their checksum. Partial snapshots cannot overwrite admission evidence. A
  separate check preserves an explicit whole-array argument. These are production PostgreSQL
  transactions, not process crash or representative resource measurements.
- Injected child-creation transaction failure rolling back the control position, child row, deadline
  timer, wake generation and River delivery together; retry retains the frozen elements and original
  parent deadline.
- A bounded scan of 70 human bodies, including newly inserted addresses behind the cursor, going
  idle once all waits are stable. A response beyond the cursor still consumes its accepted node in
  its command transaction, and cancellation terminates every other child/request.

Lost-attempt checks against PostgreSQL and literal provider HTTP additionally verify:

- Four competing reconcilers create one resolution request and release the same reservation once.
- A received and journaled model response without a final envelope remains unknown; a saved envelope
  instead finalizes successfully without another generation or operator request.
- A received response whose journal commit fails retains its original operation identity.
- An agent with a completed prefix cannot restart after `not_executed` evidence.
- The fail policy creates a final failure, while cancellation and expired original deadlines keep
  unknown-effect diagnostics and reject late outputs.
- A rejected River insertion rolls back the lost result, pause, request and slot release together.
- Nine recovery cases retain late envelopes as evidence, and an expired owner cannot renew its
  lease.
- A corrupt final envelope creates one operator request, releases capacity, preserves the corrupt
  bytes and cannot be silently replaced; verified resolution performs no extra provider call.
- A 129-record overdue timer set is consumed in batches of 64, 64 and 1 with one durable wake per
  nonempty transaction.

Delivery-repair checks additionally verify:

- Twelve actual River/provider cases covering deleted, discarded, completed, cancelled, expired
  running and healthy advance/execute deliveries. Four concurrent reconcilers create one required
  wake; each run performs one physical generation and one debit with business attempt number 1.
- Eighty ready attempts, where the first 64 healthy jobs do not hide a missing later delivery.
  Capacity exhaustion and admission pause create no new wakes. Restored capacity permits repair.
- Eighty pending redispatches, where the first scheduler transaction processes 64 and a continuation
  processes the remaining 16.
- Injected queue failure rolling back the redispatch marker and wake generation, followed by a
  successful repair with delivery generation 2 and the original business attempt.
- Worker/client bindings cannot change within an incarnation. A live heartbeat keeps a fetched
  delivery valid; an expired heartbeat permits repair, and a fresh heartbeat cannot override the
  configured handler timeout.

The preceding in-process checks age persisted leases to exercise expiry; they do not establish the
database-outage acceptance window.

`TestRiverAdmissionProcessCrashBoundaries` runs the complete `app.Serve` composition in separate
processes against PostgreSQL, with native advance/execute queues paused before run admission. Three
SIGKILL boundaries cover the first delivery insertion, the command-receipt insertion before commit,
and a committed admission whose HTTP receipt is withheld by a reverse proxy. A pre-commit crash
leaves no run, graph, scope, node, attempt, receipt or native delivery; a committed crash retains
exactly one of each. Neither state spends call budgets or starts an external operation while queues
are paused, and no Temporal outbox record is created.

The real client keeps its pending command through process loss and retries it against a replacement
service with the same engine identity. A fresh client receives the identical server receipt for the
same key; a different payload with that key gets HTTP 409 without creating another run or call.
Resuming the native queues makes exactly one literal provider request, with the full
**162,500-byte** input and expected credentials, one attempt/model debit/success event, zero
retained slots, complete exports and the original database admission timestamp/root/node deadlines.
The committed receipt is compared byte for byte on retry. These cases use the existing recovery CI
opt-in and require no Docker or Temporal service.

All three admission cases passed with race detection in **4.23s** (**6.360s** package total).

`TestRiverStartupProcessCrashBoundaries` kills the actual service during River migration 7, worker
registration, and delivery-client binding. Each case uses `pool_max_conns=1`, a retained compiled
definition, the same engine identity, and an unavailable Temporal endpoint. The migration case
starts at version 4: versions 5–6 commit before migration 7 blocks on `river_queue` DDL. After
SIGKILL, version 7's notification table, dropped client tables and other changes roll back together.
The replacement reaches the pinned version 8 and executes the retained definition through a real
human request/answer. The HTTP listener remains closed at each startup barrier; the service lease
can be reacquired after process loss, and replacement startup uses a fresh worker incarnation.

Registration and delivery binding are autocommit statements. PostgreSQL can commit the blocked
statement after the client socket disappears, so these tests accept either committed or rolled-back
registration/binding. An abandoned incarnation remains unused; the replacement creates its own row
and does not rebind the old one. This distinction does not weaken the transactional run-admission
checks above, which use an explicit transaction.

`TestRiverShutdownProcessBoundaries` holds a literal authenticated model request while the actual
service shuts down. Two SIGKILL cases interrupt the worker-drain statement before completion and
kill a committed draining worker before the provider replies. The listener has already stopped; one
admitted operation, model debit and occupied slot remain at each barrier. The replacement waits for
natural ownership expiry, preserves an unknown-outcome resolution and makes no automatic second
provider call. Explicit cancellation after inspection retains the one business attempt and spent
debit, releases slots, and preserves both original deadlines and one terminal event in the
database/SSE replay.

Two further cases release the provider response during graceful shutdown and after terminating the
exact PostgreSQL connection holding the engine's advisory lease. The graceful service exits cleanly;
the ownership-loss case closes its listener and reports failure. Both retain the committed response,
and replacement startup completes the run without a second invocation. These are single-host process
checks using the existing recovery CI opt-in, not cluster or full stop/result arbitration
acceptance.

A fifth shutdown case never releases the active provider response. The ten-second drain times out,
River's hard stop cancels the HTTP request, and the service exits with its shutdown error within the
fixture's bounded wait. It exposed that adapter transport classification discarded cancellation
identity: the host recorded a retryable model failure rather than an unknown outcome. The owned host
now also checks its execution context after an adapter error. This applies to every native adapter;
a confirmed successful response remains a known result, and Temporal execution retains its existing
behavior. Replacement startup preserves the uncertainty for operator inspection, without another
physical call or attempt, and cancellation retains the spent debit and deadlines. The fixture
explicitly allows two business attempts, so lack of replay is not an incidental result of a
one-attempt policy; that version passed in **10.71s** (**12.967s** package total).

All three startup cases passed with race detection in **1.92s**. The expanded five-case
shutdown/ownership-loss matrix passed in **59.80s** (**62.293s** package total), including the
forced stop in **10.92s**. Three PostgreSQL host regressions and all nine physical
result/stop/deadline arbitration cases also passed after the fix; the latter ran in **20.22s**. Each
killed active-call case waits for the natural 20-second ownership lease; tests do not rewrite stored
expiry timestamps.

`TestRuntimeProcessCrashBoundaries` additionally starts the actual Runtime/Host in child processes
and verifies SIGKILL at eight boundaries: native fetch before claim, ownership claimed before its
first operation intent, prepared intent before physical-call admission, committed admission before
HTTP sending, external acceptance before response journaling, committed response before the final
envelope, synced envelope before result publication, and committed result. Database advisory
barriers block production writes; the pre-send case holds the native HTTP transport's dial after the
operation/budget transaction commits and before the provider receives a request. No synthetic
outcome replaces the executor. Replacement processes wait for natural 20-second lease expiry.

The three new claim/intent/pre-send cases verify zero physical generation requests before the
operator's `not_started` decision. Recovery conservatively pauses for resolution rather than
automatically recreating the claimed work. Only the accepted decision permits a second business
attempt, with the original node deadline. Claim/prepared-intent loss spends zero budget before that
decision; pre-send admission has already spent one debit, which is retained, and its authorized
second attempt spends another. Every case finishes with one literal HTTP call, released ancestor
capacity, one success event and the complete 150,000-byte export. The original five boundaries
retain one business attempt/debit; saved envelopes finalize automatically. All eight cases passed
together with race detection in **160.32s** (**161.902s** package total). `make check` and
Linux/Windows lint passed with zero issues. These Unix checks are enabled in integration CI.
Artifact publication has separate process checks below; request, initialization and control
boundaries are covered by their own fixtures elsewhere in this document.

The additional opt-in `database_outage_saved_envelope` case disconnects every PostgreSQL connection
of the actual executor through a test-owned proxy after its provider response is journaled. The
executor saves the confirmed envelope while publication is unavailable, then receives SIGKILL.
Connectivity stays unavailable for six full minutes. A replacement process imports the saved result
with one physical call, one debit, released capacity and unchanged business attempt/deadline. No
domain leases or River timestamps are artificially aged. This check passed with race detection in
**364.477s**; other prolonged-outage boundaries remain required.

`TestRuntimeArtifactPublicationProcessCrashBoundaries` uses the same real Runtime/Host child
process, a Docker Python sandbox that creates a random nonce and an artifact, and a dependent
literal HTTP model call. PostgreSQL advisory barriers select six actual SIGKILL boundaries:

| Boundary                                                                                   | Recovery requirement                                                                                                                                                    |
| ------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Immutable artifact bytes saved, before metadata insertion                                  | Bytes remain private; missing final envelope produces one resolution request and no automatic code or dependent call.                                                   |
| Private artifact metadata committed, before tool response journal                          | Metadata stays private (HTTP 404); the original bytes remain, with the same unknown-outcome policy.                                                                     |
| Artifact visibility changed inside the result transaction                                  | SIGKILL rolls back visibility, successful result, evidence and success event together; the synced envelope recovers the original artifact ID and nonce.                 |
| Native execute delivery completed inside the result transaction, immediately before commit | SIGKILL rolls back the publication transaction; a replacement imports the complete envelope without another sandbox generation or tool debit.                           |
| Dependent execute delivery insertion, before its graph-step commit                         | The predecessor result and artifact are already committed and remain immutable; the interrupted downstream admission rolls back and recovery issues one dependent call. |
| Root result committed                                                                      | Restart keeps the complete exports and public artifact bytes, with no repeated work.                                                                                    |

Dependent execution is admitted in the next bounded scheduler step, reached through the
publication's transactional wake; it need not share the predecessor's result transaction. The
fixture checks both the publication rollback and this committed-prefix boundary. Every case checks
the original root and node deadlines, one code attempt/tool debit/sandbox generation, ancestor slots
released, immutable artifact bytes retained, expected downstream model calls/debits and physical
sandbox cleanup. Missing envelopes are not inferred to mean the code never ran; those cases remain
paused for resolution and are then cancelled. Replacement owners wait for natural lease expiry
rather than aging database timestamps. This fixture is enabled by the existing Unix integration CI
environment; stop/result arbitration and agent-prefix crash boundaries still need their remaining
process checks.

The six cases passed together with race detection in **113.41s** (**115.549s** package total).
`make check` and Linux cross-platform lint passed with zero issues. These are real process-loss
checks on the local filesystem and one host, not shared-storage failure or cluster acceptance.

`TestRuntimeForeachProcessCrashBoundaries` runs the actual Runtime/Host and native River delivery in
separate processes. PostgreSQL barriers select five SIGKILL boundaries: child creation before
commit, child completion before commit, first collection page before commit, the next page after a
committed 64-element page, and parent completion before commit. Recovery preserves committed
positions and child counters, original deadlines, exact ordered exports, and one previously
confirmed physical model call/budget debit. Collection/parent cases use 130 bodies and finish all
64/64/2 pages. These Unix checks use the existing integration CI opt-in. They do not establish
human-answer/cancellation crash coverage or representative resource/latency acceptance.

All five cases passed with race detection in **296.137s**. The 130-body cases each took 78–84
seconds including process loss/restart; this exposes scheduling cost to investigate in the
representative resource comparison, not a release performance result.

`TestRuntimeLoopAndPipelineProcessCrashBoundaries` adds eight real-process cases. Both controls
receive SIGKILL before child creation, child completion and parent completion commit. Loop checks
also stop before the simultaneous replacement of two carry values and after committed carry
advancement but before the next child is inserted. Each interrupted control row remains
byte-for-byte equal to its committed snapshot. Recovery finishes three loop iterations with one
literal HTTP model call, business attempt, budget debit and success event per iteration; no
completed iteration is replayed. The pipeline check retains its two budget scopes, model permission
and child timeout anchored at the original parent admission. Parent deadlines remain unchanged and
all scope slots are released. These checks use the same isolated schemas, process runner and
integration CI opt-in.

All eight cases passed with race detection in **165.339s**. The two foreach creation/completion
cases also passed again after sharing the process runner, in **52.204s**.

`TestRiverHumanAnswerProcessCrashBoundaries` starts the production River service in separate
processes and verifies SIGKILL before the answer update commits and after commit while a reverse
proxy withholds the receipt. The human node belongs to a foreach body; native advance delivery is
paused so parent completion must resume after restart. Before-commit termination leaves the request
open with its original identity/deadline and no answer or command receipt. After-commit termination
preserves the first answer, child completion/counters and receipt together. The client retains its
pending command and retries the same key; a fresh client also replays the exact server receipt. The
committed case retries after the original human deadline. Another key cannot replace the answer.
Resuming native delivery exports the complete **225,000-byte** answer, with two node success events,
unchanged parent/request deadlines and no leaf attempts, external operations, budget debits or
reserved slots. Both cases passed with race detection in **10.246s**. Concurrent response/expiry
crash boundaries remain required; resolution and cancellation command crashes are checked below.

`TestRiverResolutionProcessCrashBoundaries` runs the production River service against PostgreSQL and
a literal OpenAI HTTP fixture. It covers all three API decisions (`succeeded`, `failed`,
`not_started`) with SIGKILL before the last decision commits and after commit while the HTTP proxy
withholds its receipt. Two independent nodes require resolution; the first accepted success leaves
root admission paused. Before-commit termination preserves the original open request, waiting node,
root pause and zero receipts. After commit, the decision, request closure, corresponding node
transition, root resume or definitive stop and receipt survive together. The persistent client
retries its original key, and a separate client replays the same receipt; another key cannot replace
the decision. All six cases passed with race detection. The strengthened `failed` cases passed in
**42.90s**, and the `not_started` cases with verified initial SIGKILL passed in **45.72s**.

For `succeeded`, both models received real replies whose journal commits failed. Neither runs again;
only the dependent third model executes after queue recovery, giving three one-time physical
calls/attempts/debits/success events and one completed operation journal. For `failed`, the provider
records a permanent rejection before its response is lost with the initial SIGKILL. Natural lease
expiry creates the uncertainty request; the operator supplies evidence of that rejection. Only the
two original calls/attempts/debits remain, the rejected model performs zero generations, no
dependent call or root export appears, and the first confirmed node remains immutable. For
`not_started`, the second request reaches the provider but performs zero generations before an
initial SIGKILL. Natural lease expiry creates its uncertainty request. Only explicit evidence of
non-execution permits a second business attempt: four HTTP requests/attempts/debits produce three
actual generations, with one generation per node and only the retried model and dependent model
journals completed. The original confirmed sibling and run/node/request deadlines survive every
case, with no open requests or occupied slots.

The initial successful-decision test also exposed inconsistent rejection of a closed resolution on a
nonterminal run. The shared store now locks the latest matching request and returns HTTP 409 for a
closed decision, while an absent resolution still returns 404. The database request/lost-attempt
regression passed in **7.381s**. Agent-prefix decision crashes are covered by the expanded process
matrix below; the remaining request/result process boundaries still require acceptance.

`TestRuntimeRequestCommandsArbitrateUnderContention` covers **20** human-answer and model-resolution
cases through the production API handler and real River runtime on PostgreSQL. SQL barriers retain
the first command's run/request mutation lock; `pg_blocking_pids` verifies the competing command is
actually waiting before release. Different keys cannot replace the first accepted answer or
resolution. Concurrent identical commands return exactly the same receipt; reusing the key with a
different body returns 409 and leaves one receipt. Both answer/cancellation orders are checked, as
are competing successful/failed resolutions: the first decision, corresponding node transition and
stop cause remain unchanged. Replays preserve both accepted receipts and rejected-command receipts.

Node and root deadline cases use natural **3s** expiration without rewriting domain timestamps. An
answer/decision reserved before expiry remains accepted when its transaction resumes after the
deadline, with one node success event and its original output. A root deadline still fails the run
and closes the other wait; it does not erase that confirmed node. A command blocked on the run lock
until after expiry is rejected and cannot create accepted response evidence. Every case retains the
original run/node/request deadlines, releases all slots and has no Temporal outbox delivery. Human
cases make zero leaf attempts or provider calls; resolution cases keep exactly the one original
physical model call, attempt and debit. All cases passed with race detection in **31.44s**. These
lock-contention checks complement the command SIGKILL fixtures; remaining request-crash boundaries
still require acceptance.

`TestRiverFailFastProcessCrashBoundaries` completes one real MCP tool, keeps a second call active in
the same run-scoped session and receives a permanent HTTP 400 model rejection on a parallel node. It
confirms the synced failure envelope before killing the production service, either while the
stop-cause UPDATE is blocked inside the publication transaction or after its commit while the remote
peer is still active. A temporary cancellation/DELETE outage keeps that peer alive through process
loss. The before-commit case rolls back failure publication and stop state together; the committed
case retains both. After restart, recovery publishes the original saved failure, cancels the
original RPC and deletes the recorded session, without another initialization or tool/model
execution. Both cases passed with race detection in **42.19s**.

The fail-fast checks retain the first tool output and its one success event, one failure event, the
original cause/deadlines, three business attempts, two tool debits, one model debit and no
downstream call, root export, artifact publication or occupied slots. Actual remote HTTP 404 proves
session removal. If recovery discovers the lost peer before applying the saved stop, its
unknown-outcome request remains retained and cancelled; no open operator request survives final
failure.

`TestRiverSandboxFailFastProcessCrashBoundaries` replaces the active MCP peer with actual Python
code in an owned Docker sandbox. The code records its PID and appends a physical execution marker
before sleeping. A local Unix-socket proxy temporarily rejects container DELETE requests, keeping
the original process alive through SIGKILL even if the old worker observes the stop. The test reads
the single marker and original PID's `/proc` entry after process loss. Replacement recovery deletes
that same container and its workspace, closes both resource records and retains the completed MCP
sibling and original failure/deadlines. There is exactly one sandbox resource, three attempts, two
tool debits and one model debit; no retry, downstream operation or exported artifact occurs. Both
boundaries passed with race detection in **42.88s**.

`TestRiverAgentFailFastProcessCrashBoundaries` instead runs an actual two-turn agent: one confirmed
`files.write` creates its report, then the agent enters the blocking run-scoped MCP call. The
sibling model's permanent rejection triggers the same two crash boundaries with MCP cancellation and
Docker deletion unavailable until recovery. The confirmed file survives SIGKILL; the replacement
then cancels the original RPC, deletes the session/container/workspace and closes every owned
resource. Exactly two agent model requests, one sibling model request and three tool debits survive,
with two completed tool journals and three attempts. The agent does not resume, rewrite its file or
publish its collected artifact. Both boundaries passed with race detection in **44.39s**.

`TestRuntimePhysicalOutcomeArbitratesWithStopAndDeadline` covers **nine** publication-contention
cases after real Docker execution. Python writes a file containing a unique nonce and returns the
same nonce; the real host confirms its operation journal and saves/reopens its immutable outcome.
The model-failure variants also make one literal OpenAI HTTP request and retain its permanent 400
rejection. The test invokes the production publication functions with controlled SQL barriers and
verifies the competing transaction's actual lock wait. Native wake continuations are consumed
explicitly after arbitration to finish bounded scheduler steps; this isolates publication after
physical execution and complements the production-process SIGKILL tests.

Both result/cancel and result/sibling-failure orders are checked. A result committed first retains
one node success and its public artifact, even when the root subsequently stops. When stop wins, the
result remains evidence, the node cannot succeed, and the artifact content endpoint returns 404.
Reimporting the same outcome twice cannot change that winner, duplicate an event or release another
slot. Concurrent duplicate publication yields one success and exactly one unclaimed downstream
attempt; stopped runs reject a downstream claim entirely.

Node/root deadline variants expire naturally after **4s**, either while the live publisher waits for
the run lock before staging or while its completed-attempt update is blocked within publication. The
former rejects live ownership and imports the late envelope as evidence only. The latter retains the
completed attempt result but applies the elapsed deadline before node/artifact publication. Both
keep the original deadlines and hide losing artifacts. All cases retain the original outcome file
and artifact bytes, one closed sandbox resource, one physical tool debit/completed journal, zero
occupied slots and no root exports. Failure variants add exactly one model request/debit and failure
envelope. All nine cases passed with race detection in **20.82s**. Process termination during the
deadline/sibling-failure variants and other crash/resource gates remains unverified; the
saved-result and cancellation process cases are covered below.

`make check` and the CLI build passed; Linux and Windows lint reported zero issues. Regenerating
with sqlc **1.31.1** preserved all 17 generated-file hashes. Desktop checks passed 126 frontend
tests, the production frontend build and 26 native tests; the opt-in native live-engine test
remained ignored. These checks do not replace the remaining desktop/client acceptance matrix.

#### Saved result and cancellation process arbitration

`TestRiverStopAndSavedResultProcessCrashBoundaries` adds **four actual service SIGKILL cases** using
the existing recovery package. Python executes in Docker, creates a random nonce and a JSON artifact
containing the complete **180,000-byte input**, and the real host journals the code call and fsyncs
its immutable outcome. In the first two cases a PostgreSQL trigger holds artifact publication before
its transaction commits; a concurrent real HTTP cancel waits behind that transaction's run lock. The
process is killed, both transactions roll back, and the pending client command survives.

- **Cancel before recovered result:** the replacement accepts the client retry before allowing
  finalization. The node remains cancelled, its artifact stays private (HTTP 404), and the
  successful envelope is retained as evidence without graph outputs.
- **Recovered result before cancel:** the replacement finalizes the saved envelope before the client
  retry. The successful node, artifact ID and downloadable bytes survive subsequent root
  cancellation; neither downstream code nor root exports are admitted.
- **Committed result with lost cancel receipt:** publication has already committed and the human
  request exists. The API commits cancel and its receipt while advance is paused; a proxy withholds
  that receipt and the service is killed. The replacement replays the exact accepted receipt and
  preserves the published prefix while completing cancellation.
- **Late result after cancellation:** the fixture temporarily makes the actual fsynced envelope
  unavailable. Natural lease expiry creates `waiting_resolution`; the operator cancels, and only
  then does the fixture restore the original file. The envelope is imported as evidence while the
  cancelled node and private artifact retain their state.

The last case exposed a production bug: `ListExpiredOutcomeAttempts` selected only `claimed`
attempts. A lost attempt had already become completed/cancelled, so restoring its envelope could
never reach finalization. The indexed, 64-record scan now selects expired owned attempts without
stored outcome evidence. Missing/corrupt envelopes for already fenced attempts skip the lost-attempt
transition; verified envelopes use the existing identity/version/artifact checks and guarded
`RecoverOutcome` path. This retains evidence without replaying execution or replacing an operator
decision, terminal node or superseding attempt. Missing fenced files remain in the bounded polling
scan until retention; high-volume storage notifications remain a possible future optimization.

All four cases passed with race detection in **68.48s**. Each keeps one code attempt, one tool debit
and completed physical journal, one cancel receipt and terminal SSE/database event, the original
root/node deadlines, zero occupied slots/open requests, and a physically deleted sandbox. The same
original envelope bytes and artifact checksum/size/nonce survive recovery. The code node has **three
permitted business attempts**, but executes only once. Known prefix artifacts remain public; losing
artifacts remain private. No downstream attempt, root export or Temporal outbox is created.

The existing ten lost-attempt cases now discover late evidence through the production scan and River
finalizer instead of calling publication directly; they passed in **2.90s**. The rollback fixture
discards its already viable pending wake before injecting a new-wake INSERT failure, so advance
coalescing cannot bypass the intended fault. This covers missing/corrupt evidence, saved envelopes,
rollback, unconfirmed operations, agent prefixes, fail policy, cancellation and both deadline
scopes. The broader deadline/failure process matrix and complete resource/client gates remain open.

After the production scan change, all nine physical publication/stop/deadline contention cases
passed again in **20.35s**, and the eight actual runtime process crash cases passed in **160.49s**.
`make check` and Linux/Windows lint passed with zero findings. Regeneration with pinned sqlc
**1.31.1** preserved all 17 generated-file hashes on the second pass. Only the initial schema index,
scan query/generated row, shared reconciler and their acceptance fixtures changed; no application
migration or dependency was added.

### Default service composition and existing clients

`knotra serve --backend river` (`KNOTRA_BACKEND=river`) connects the existing API's admission and
command transactions to the runtime. It initializes River in the application's PostgreSQL schema,
opens the persistent outcome directory and starts guarded workers before accepting HTTP requests.
This path creates no Temporal client, worker, codec store or outbox delivery loop. River is now the
default for the single-process MVP; Temporal requires an explicit backend selection.

The combined service keeps the exclusive engine lease and uses the single-host `local` resource
identity. `--execution-workers` bounds leaf delivery concurrency (default 16); graph/ancestor limits
remain authoritative. Delivery timeout exceeds both current profile limits and the original
durations of active admitted runs by one minute. Editing a profile on restart cannot shorten a
frozen run's timeout. The native schema name must fit River's 46-byte limit; isolated acceptance
schemas use a shorter prefix.

`/v1/info` reports the admission backend, scheduler/state versions and execution delivery timeout.
Maintenance errors are logged and retried independently. Loss of the registered worker heartbeat
stops the HTTP listener and execution runtime; shutdown errors are returned rather than hidden.
Graceful shutdown drains with bounded contexts and closes outcome storage after workers stop.

The same real-process scenarios now run on both backends. The River cases deliberately configure
Temporal at a closed loopback port and verify:

- OpenAI and Anthropic HTTP fixtures each produce one generation across SIGKILL and restart at a
  durable human wait; the existing CLI response completes the original run. A shorter edited profile
  leaves the original request deadline and queue timeout intact.
- Actual Docker commands publish an artifact before SIGKILL. Restart retains its bytes, ID and nonce
  without repeating completed code. HTTP/SSE replay reproduces the durable prefix exactly and the
  continuation contains no duplicate event IDs.
- The existing CLI cancels a second run at its human wait. A later HTTP answer receives 409 and no
  downstream code is dispatched.
- An expired registered worker heartbeat shuts down the real `app.Serve` listener; PostgreSQL and
  its owned schema remain available. This is an injected expiry check, not network-outage evidence.
- A human answer commits its graph transition in the HTTP command transaction while the advance
  queue is paused. Replaying the command and resuming delivery after the original deadline preserves
  the successful answer; delayed delivery cannot turn an accepted answer into a timeout.
- An OpenAI fixture drives a real Docker agent through file creation, an MCP call, invalid output
  and its correction. The published artifact contains the expected bytes; two subsequent direct MCP
  nodes reuse the agent's run session. Admission and terminal execution sessions receive HTTP
  DELETE. PostgreSQL records exactly four model calls, four tool calls and three execution attempts.
  This literal acceptance passed with race detection in **4.52s**.
- SIGKILL after an agent's completed file write and MCP call leaves its third model request
  unconfirmed. Replacement waits for natural lease expiry and opens a resolution request; a
  `not_started` decision cannot restart the completed prefix even with retries configured. Physical
  calls and PostgreSQL budgets remain three model calls and two tool calls, with one business
  attempt, no published artifact and no occupied slot.
- SIGKILL at a human wait after a completed direct MCP call preserves the wait identity and
  deadline. The next tool node reports its lost run session instead of creating an independent one.
  Operator failure resolution ends the run without another physical tool call or debit. Both process
  cases passed with race detection in **21.87s**; root deadlines and confirmed tool records remain
  unchanged, and no lease, deadline or queue row was artificially aged.
- Cancellation during an agent's third model call and during a direct MCP call stops the physical
  request through the real River service. Replaying the cancellation key retains one receipt.
  PostgreSQL retains one attempt, the original deadline and the confirmed agent prefix (three model
  debits and two completed tool operations); cancellation creates no resolution request, published
  artifact, downstream call or retained capacity. The owned Docker sandbox and workspace disappear,
  and the remote run session returns HTTP 404 after cleanup. A third case injects HTTP 503 on its
  first DELETE and requires maintenance to retry deletion of the same session. All three cases
  passed with race detection in **20.74s**; the agent/session SIGKILL regression passed in
  **22.18s**.

The remaining crash matrix, resource measurements and default setup/Compose cutover still remain
required.

### Commands after changing admission backend

`--backend` selects new-run admission. Startup also reads the retained execution backends under the
exclusive engine lease. It starts River for retained River state, including terminal runs that can
still require outcome or resource cleanup. It starts Temporal for live Temporal runs or unsent
outbox commands. Both workers may run in one service; this does not enable multiple hosts. Once
legacy runs and commands finish, River admission starts without a Temporal connection even with
retained legacy projections. Temporal connection startup uses the service context.

Cancel, human response and resolution still route by the immutable backend of the admitted run.
Legacy decisions commit their outbox entry with the receipt; River decisions commit the domain
transition and native delivery with the receipt. `/info.backend` reports admission;
`/info.workerBackends` reports the running workers. The River runner retains its own ownership
fields and outcome evidence while sharing the existing run-session cache with the legacy runner.

Legacy sandbox cleanup finishes before either worker starts. Worker-owned container labels prevent
bulk deletion. Database resource identities additionally protect directories from interrupted
container creation and containers missing a worker label. A failed ownership lookup stops cleanup;
River deletion remains guarded by its existing cleanup claims. The Unix Docker HTTP fixture checks
both successful legacy cleanup and database lookup failure while retaining River containers and
filesystem-only directories. This is a cleanup guard regression, not two-host failure acceptance.

`TestAdmittedBackendSurvivesAdmissionSwitch` uses real PostgreSQL, Temporal, River, HTTP provider
fixtures and SIGKILL. Both Temporal-to-River and River-to-Temporal admission switches retain open
requests and their deadlines. Each direction exercises response, cancellation and uncertainty
resolution for runs on both backends, exact receipt replay by a fresh client, cancelled-answer
rejection and an unchanged SSE prefix. Each model run retains one physical call, one debit and one
unconfirmed operation intent. River runs retain one business attempt and create no Temporal command;
legacy commands create exactly one matching outbox record. After drain, restart with a closed
Temporal port reports only the River worker. Both directions passed with race detection in
**18.26s** (**20.604s** package total). The real Docker agent/MCP session reuse and cleanup
regression passed in **3.49s** (**5.735s** package total), and `make check` passed.

### Shared filesystem and execution host identity

`serve --host-id NAME` (`KNOTRA_HOST_ID`, default `local`) supplies a stable execution host identity
to worker registration, resource labels, cleanup queues and `/info.hostId`. Names contain 1–128
ASCII letters, digits, dots, underscores or hyphens and start with a letter or digit; validation
also runs at registration and queue/resource boundaries. Non-default hosts have a hashed staging
namespace under the engine work directory. The existing `local` and legacy layout remains unchanged.
This separates local workspaces even if configured directories overlap; it is not capability
routing.

`--shared-data-dir DIR` (`KNOTRA_SHARED_DATA_DIR`) keeps `artifacts/` and `outcomes/` separately
from local `--data-dir` staging. Omitting it preserves the existing layout. MCP initialization and
current-call evidence remain in the shared outcome directory. Temporal payloads remain local; hybrid
deployment is still exclusive and single-process. Keep the filesystem contents with the matching
PostgreSQL backup. Changing a storage path does not copy existing data automatically.

The filesystem implementation uses confined `os.Root` handles, synced contents and directory
entries, immutable hard-link publication and atomic rename for mutable current-call records. Private
directory initialization now shares the existing outcome-store parent syncing with artifacts.
Concurrent artifact writers cannot replace an existing content address. An existing file is accepted
only after checking regular-file identity, size and SHA-256; damaged bytes, symlinks and directories
fail without being repaired or replaced.

Suitable shared mounts must provide cross-client read consistency, atomic hard links and
same-directory rename, stable file contents after `fsync` and durable directory entry syncing. All
trusted workers need access under compatible filesystem identities. Windows retains its existing
directory-flush limitation; cross-host metadata durability is not established by the Windows lint
check. No S3 dependency or generic storage interface was added. Network filesystem behavior, storage
failure/recovery and concurrent multi-host backup/restore remain release acceptance work. Stopped
single-host backup/restore is verified below.

Two independent writer processes publish identical artifact bytes and separate complete outcome
envelopes through the same filesystem, then exit without closing storage handles. A fresh reader
recovers both envelopes and their artifacts with one content-addressed file and no temporary files.
The test also rejects replacement of damaged files, symlinks and directories. These checks and real
PostgreSQL artifact visibility/integrity tests passed with race detection in **3.645s**.

The real River process recovery fixture now uses shared storage, writes a Docker-produced artifact
and outcome, then receives SIGKILL at the human wait. The replacement changes both host identity and
local data directory while retaining only PostgreSQL and the shared store. Original artifact bytes,
ID, code nonce, request deadline and SSE prefix survive; CLI response/cancellation work and
completed code is not repeated. This sequential replacement passed in **4.40s** (**6.294s** package
total). It does not prove concurrent two-host failure acceptance. All nine physical publication
races passed after the immutable artifact change (**25.638s** package total), and `make check`
passed. Shared filesystem support is implemented; process roles, capabilities, affinity and actual
concurrent worker hosts remain required before removing the global engine lease.

### Sandbox resource ownership and cleanup

The execution host records a sandbox UUID, engine, host, creating attempt, process incarnation and
ownership generation before creating its directory or Docker containers. Main and firewall
containers carry the same complete ownership labels. Preparation records do not spend call budgets.
Attempt resources become eligible after their claim ends or expires; run sessions remain protected
while their worker and run are live. A dead worker or terminal run permits their cleanup.

Cleanup jobs use an engine/host-specific River queue. The database scanner visits at most 64
resources per poll, advances past live sessions and repairs completed, cancelled, discarded or
deleted native deliveries with a new cleanup generation. Cleanup claims and queue insertion commit
together. Physical deletion requires a committed claim and exact labels for every selected
container. Directory removal uses a confined `os.Root`; failed Docker deletion retains the directory
and cleanup record for retry. Closure and native job completion commit together.

An additional host inventory reads at most 64 containers through Docker's documented `limit` and
`before` filters. It validates identities before querying durable ownership. Matching physical
evidence can reopen a closed record when an unanswered create completes after cleanup; the original
owner and resource UUID never change. A removed pagination anchor resets the bounded scan. See the
[Docker Engine API specification](https://docs.docker.com/reference/api/engine/version/v1.40.yaml).

PostgreSQL, Unix-socket and actual Docker checks with race detection verify:

- 65 protected run sessions cannot hide a later abandoned attempt resource or suppress delivery
  repair. Stale cleanup deliveries cannot claim the new cleanup generation.
- Foreign engine, host, run, instance, attempt, worker and ownership generation cannot reopen a
  closed resource. Matching late evidence remains eligible even while its original worker is live.
- Physical label mismatches cause no deletion. Deletion failure keeps the directory; a resource
  directory symlink cannot remove files outside the owned root.
- Inventory pagination, removed anchors, malformed/foreign identities and oversized pages retain the
  bound and never authorize foreign deletion.
- SIGKILL of a real sandbox executor is followed by replacement cleanup after natural lease expiry.
  A live peer on the same host finishes successfully. A later Docker container with the old exact
  identity is discovered and removed without another business attempt or call debit.
- An unrelated expired claim with an incompatible scheduler version produces a maintenance error
  without suppressing resource cleanup or the live peer's execution. The combined real-process check
  passed in **45.98s**.

Terminal MCP cache cleanup scans at most 64 sorted identities per poll, skips busy initialization
and active calls, and retains entries on close failure. Live runs keep their shared session across
attempts. The pinned SDK closes locally once, caches its close result and ignores HTTP DELETE
status. The adapter therefore records cleanup failures and retries only the original DELETE with the
same endpoint, session ID, protocol version and pinned credentials, using a fresh bounded context.
It never initializes a replacement session. Successful deletion or HTTP 404 removes the cache
identity; HTTP 405 completes local closure and leaves remote expiration to the server, as allowed by
the
[MCP session-management specification](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#session-management).
An adapter test verifies retention after HTTP 503, successful retry, idempotent local closure, no
additional initialization, preserved headers and actual remote HTTP 404. Other errors remain queued
for maintenance.

HTTP MCP execution now registers an owned resource intent before initialization and durably records
the resulting session ID, negotiated protocol version and connection name before any tool call.
Initialization evidence is first published into the existing outcome directory with file/directory
sync, immutable link publication and a checksum, under a separate MCP key namespace. Its envelope
includes the original resource and owner and is bounded to 64 KiB; these bounds do not cap ordinary
execution outcomes. The subsequent PostgreSQL update may fail or have an unknown commit outcome.
Cleanup of a resource missing its database session ID imports the matching file before deletion.
Wrong host/owner, conflicting session identity, unsupported format, corruption or oversized evidence
is rejected. Resource evidence files belong in the same backup as outcomes and PostgreSQL. The
resource uses the same host, worker-incarnation, ownership-generation and lifetime checks as a
sandbox. Its record stores no resolved credential values; cleanup loads the frozen admitted profile
and resolves its credential references. Session IDs are bounded to 8 KiB of visible ASCII,
connection names to 1 KiB and protocol versions to date syntax. Stateless initialization records an
empty ID and requires no DELETE. Late initialization evidence can be recorded after the creating
attempt ends, but cannot replace an existing ID or another owner's evidence. PostgreSQL tests verify
these fences and protection of live run sessions after their creating attempt completes.

After SIGKILL, a replacement uses native cleanup delivery to delete the recorded remote session.
Real agent-prefix and human-wait process tests inject HTTP 503 on the first DELETE and require a
successful retry, a closed durable resource and actual remote HTTP 404, with no tool replay or new
initialization. Changing the profile's endpoint before replacement startup does not redirect cleanup
away from the admitted endpoint. The original two process cases passed with race detection in
**25.27s**; normal agent completion and all three live-cancellation cases also passed after adding
the durable records.

`TestRiverProcessLossPreservesAgentAndMCPSessionUncertainty` now runs eight cases: the original
agent-prefix/run-session loss scenarios and all three agent resolution outcomes at two additional
SIGKILL boundaries. The agent writes `confirmed-prefix` into a real Docker workspace, then performs
a non-idempotent MCP write recorded and synced in the parent provider's filesystem. Two model/tool
replies are durably confirmed before a third model request is accepted and blocked. The fixture
reads the actual workspace file and external write record before killing the service.

After natural lease expiry, the replacement preserves the unknown agent outcome with
`canRetryIfNotExecuted=false`. Each new case then kills this replacement before the resolution
transaction commits, or after commit while the HTTP receipt is withheld. SQL barriers and a reverse
proxy verify the exact boundary. Request status, node transition, accepted time, stop cause and
receipt either roll back together or remain committed. The client retains its pending decision,
retries against a third worker incarnation and receives the identical committed receipt; a fresh
client replays it, while another key gets HTTP 409.

`succeeded` supplies operator-inspected answer/report outputs, using the actual file bytes uploaded
through the artifact API. Both `failed` and `not_started` terminate without root exports; the latter
cannot restart an agent whose prefix already ran, despite a three-attempt policy. Every agent case
retains one business attempt, three model debits/calls, two tool debits/confirmed tool replies and
one physical external write. The last model reply and the unfinished agent/session lifecycle intents
remain unconfirmed; an operator decision does not fabricate those journal responses. Run, node and
resolution deadlines survive, slots return to zero, and no new session initialization or external
effect occurs. All owned resources close, Docker confirms the sandbox is absent, and retrying the
original session DELETE after HTTP 503 produces remote HTTP 404. A cleanup claim killed during the
decision boundary is recovered through its own natural lease expiry.

All eight cases passed with race detection in **234.93s** (**236.988s** package total). The six new
agent-decision cases add a second real process loss; the four failure/non-execution cases in this
run also waited for the interrupted cleanup owner's natural lease. All three live agent/MCP
cancellation regressions passed after the host cancellation fix in **18.78s** (**20.887s** package
total). These checks close the agent-prefix decision boundary, not every remaining publication or
stop/result crash point.

A separate real-service SIGKILL test blocks the first session-identity UPDATE before completion. It
verifies a durable MCP file and no admitted tool call before killing the engine. In one case the
fixture terminates only its own blocked PostgreSQL backend, leaving the identity update rolled back;
in the other it lets the single autocommit UPDATE finish after client death. Both paths retain the
original owner, restore or reuse the same ID, survive the first DELETE returning HTTP 503, and
confirm remote HTTP 404. They finish after explicit failure resolution with one attempt, zero
physical/admitted tool calls, zero call debits/publications/occupied slots and the original
deadline, without another initialization. The two cases passed with race detection in **44.96s**. No
deadline or worker lease was artificially aged.

Cancellation recovery also retains the current `tools/call` RPC ID before physical-call admission
and HTTP dispatch. A separate bounded, checksummed file is atomically replaced for each serialized
call; immutable initialization evidence is preserved. It stores the original owner/session and a
bounded integer/string request ID, without arguments, replies or credentials. Confirmed replies mark
that call complete; a failed completion-marker write keeps conservative cancellation evidence
without discarding the confirmed reply or causing another execution. Replacement cleanup sends
`notifications/cancelled` for an unconfirmed request to the frozen endpoint with the recorded
session/protocol and resolved credentials before DELETE. A failed notification keeps cleanup
retryable; HTTP 404 means the session is already absent. Completed calls are not cancelled.

`TestRiverCancellationProcessCrashBoundaries` runs the production service in separate processes and
kills it before the cancel transaction commits and after commit while the client receipt is
withheld. It pauses native advance delivery to require restart recovery, then retries the same
command through the persistent client journal and a fresh client. Both cases require an actually
interrupted MCP handler, a closed resource and remote HTTP 404, one attempt/tool debit/physical
call/receipt, no downstream execution, outputs, publications or open requests, zero occupied slots
and unchanged run/node deadlines. The first run exposed that deleting a session alone left the
handler active; recording and cancelling the RPC fixed both cases, which passed with race detection
in **41.78s**. The combined agent/session-reuse, live-cancellation, cancel-command and human-answer
crash regression passed in **71.675s**. Cancellation is cooperative: this fixture proves a server
that honors the protocol; it does not prove that arbitrary remote tools stop or undo an already
completed external effect.

This is not complete session/cluster acceptance. Pre-admission preparation still uses the existing
unregistered resources, and MCP sessions still have process-local placement. Process or response
loss before initialization evidence is durably published can leave an unknown remote session; a
pending resource intent is retained and cleanup reports uncertainty instead of claiming successful
removal. Handling this boundary and credential unavailability/rotation across recovery remains
required. Filesystem-only late creation, remaining stop/result process-crash races, shared storage
and two actual worker hosts remain unverified. River startup uses ownership-based cleanup. Hybrid
startup performs legacy cleanup before workers start and protects durable River resource identities
as described above.

### sqlc query generation

All application PostgreSQL queries now use generated pgx/v5 methods in `internal/store/db`. Their
named SQL sources live in `internal/store/queries`; schema analysis uses the complete initial
`schema.sql`. The project has not launched, so the application migration chain and checksum
bookkeeping were removed. A fresh schema is created once in a locked transaction; concurrent opens
and preservation on reopening have PostgreSQL checks. Go code retains transaction ownership,
savepoints and mutation order. Executing initial schema DDL remains with `Store.Open`, and River's
own migrations remain with its library.

`make generate-sql` pins sqlc **1.31.1**, and CI verifies generated files with `make check-sql`.
Generated code adds no runtime dependency. Definition and human-request queries enforce a byte
budget before generated `:many` methods materialize documents, retaining a cursor marker for the
next page. PostgreSQL tests verify this bound and continued pagination.

## Verification commands

Use an isolated development PostgreSQL database. Tests create and remove their own schemas.

```sh
make check
make check-sql

go test -mod=readonly -race -count=1 \
  -run '^TestSessionCleanupRetriesOriginalHTTPDelete$' ./internal/adapters

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  go test -mod=readonly -race -count=1 -timeout=5m -v \
  -run '^TestRiverCancellationStopsAgentAndMCPAndReleasesResources$' ./internal/integration

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RECOVERY=1 \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  go test -mod=readonly -race -count=1 -timeout=10m -v \
  -run '^TestRiverProcessLossPreservesAgentAndMCPSessionUncertainty$' ./internal/integration

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RECOVERY=1 \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  go test -mod=readonly -race -count=1 -timeout=4m -v \
  -run '^TestRiverMCPInitializationIdentitySurvivesCrashBeforeDatabaseCommit$' ./internal/integration

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RECOVERY=1 \
  go test -mod=readonly -race -count=1 -timeout=3m -v \
  -run '^(TestRiverHumanAnswerProcessCrashBoundaries|TestRiverResolutionProcessCrashBoundaries)$' ./internal/integration

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RECOVERY=1 \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  go test -mod=readonly -race -count=1 -timeout=4m -v \
  -run '^TestRiverCancellationProcessCrashBoundaries$' ./internal/integration

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RECOVERY=1 \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  go test -mod=readonly -race -count=1 -timeout=8m -v \
  -run '^TestRiverStopAndSavedResultProcessCrashBoundaries$' ./internal/integration

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RECOVERY=1 \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  go test -mod=readonly -race -count=1 -timeout=4m -v \
  -run '^TestRiver(Agent|Sandbox)?FailFastProcessCrashBoundaries$' ./internal/integration

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" \
  go test -mod=readonly -race -count=1 -timeout=2m -v \
  -run '^TestRuntimeRequestCommandsArbitrateUnderContention$' ./internal/queue

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  go test -mod=readonly -race -count=1 -timeout=3m -v \
  -run '^TestRuntimePhysicalOutcomeArbitratesWithStopAndDeadline$' ./internal/queue

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" \
  go test -mod=readonly -race -count=1 \
  ./internal/store ./internal/api ./internal/queue ./internal/execution ./internal/executor ./internal/scheduler

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RUNTIME_RECOVERY=1 \
  go test -mod=readonly -race -count=1 -timeout=6m -v \
  -run '^TestRuntimeProcessCrashBoundaries$' ./internal/queue

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RUNTIME_RECOVERY=1 \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  go test -mod=readonly -race -count=1 -timeout=3m -v \
  -run '^TestRuntimeReconcilesKilledSandboxOwnerAndProtectsLivePeer$' ./internal/queue

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RUNTIME_RECOVERY=1 \
  KNOTRA_TEST_RUNTIME_OUTAGE=1 go test -mod=readonly -race -count=1 -timeout=10m -v \
  -run '^TestRuntimeProcessCrashBoundaries/database_outage_saved_envelope$' ./internal/queue

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" KNOTRA_TEST_RIVER_RECOVERY=1 \
  go test -mod=readonly -race -count=1 -timeout=10m -v ./internal/queue

go test -mod=readonly -race -run '^TestTemporalBackendNeutralCompatibility$' ./internal/engine

KNOTRA_TEST_TEMPORAL=127.0.0.1:27235 \
  KNOTRA_TEST_REPLAY_WORKFLOW=knotra-history-85b24c65-53d3-4870-af96-b51eb1aeea5e \
  KNOTRA_TEST_REPLAY_RUN_ID=01a10be5-7613-794b-bc6c-140b7e1ee652 \
  go test -mod=readonly -race -count=1 -timeout=5m \
  -run '^TestTemporalReplayRetainedHistory$' ./internal/engine

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" \
  KNOTRA_TEST_TEMPORAL=127.0.0.1:27235 KNOTRA_TEST_RECOVERY=1 \
  go test -mod=readonly -race -count=1 -timeout=5m \
  -run '^TestCloudProtocolsWithRealEngineRecovery$' -v ./internal/integration

KNOTRA_TEST_DATABASE_URL="$KNOTRA_TEST_DATABASE_URL" \
  KNOTRA_TEST_TEMPORAL=127.0.0.1:27235 KNOTRA_TEST_RECOVERY=1 \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  go test -mod=readonly -race -count=1 -timeout=5m \
  -run '^TestLiteralBackendCompatibilityAndNestedBudgets$' -v ./internal/integration
```

The local checks used PostgreSQL **18.0** in the dedicated `knotra-river-migration-test` container,
on loopback port **25434**, and the repository-pinned Temporal **1.9.1** image in
`knotra-temporal-migration-baseline`, on loopback port **27235**. These containers are test-owned;
they are separate from the default Compose services.

One small-pool queue sample delivered, queried and completed 20 advance jobs in **1.045s**. Database
job creation to handler query latency averaged **451.51ms**, with a maximum of **906.39ms**. An
initial infrastructure sample before the Temporal stress run showed Temporal at **204.5 MiB** and
PostgreSQL at **51.77 MiB**. These are local spike samples, not representative release benchmarks or
evidence of achieved deployment savings.

The production HTTP benchmark now runs identical local provider fixtures on both backends, validates
complete exports and physical-call/budget counts, and captures process/container memory and CPU,
database connections, query/WAL deltas and engine storage. Three repeats of each active graph,
15-second idle observations and 60-second human waits completed with no sampling errors. The
[resource comparison](scheduler-resource-benchmark.md) includes reproduction commands and the
archived JSON evidence from before advance coalescing. Median deployment memory decreased from
**521.4 to 358.1 MiB** for the 1,004-node graph, and elapsed time decreased from **66.05 to
16.96s**. However, the 64-node DAG median increased from **5.40 to 6.83s**: two River repeats
delayed their first physical call by about five seconds. Short-DAG query calls and WAL increased
about **3.4 times**. The follow-up diagnosed and fixed the start delay; statement profiles attribute
PostgreSQL cost and eliminate redundant clock reads. Nested foreach, long agent,
timer/recovery/transaction latency, queue maintenance and complete volume growth remain outside this
benchmark. The full resource acceptance gate is still open.

## Operational metric export

The native runtime now registers the existing OTLP reader with durable backlog gauges: dirty-run
age, ready-delivery age, expired leases, unknown outcomes, active scope reservations, due-timer lag,
and unimported immutable outcome count/age. Scheduler steps, publication failures and cleanup
failures are process counters. The initial application schema records `dirty_since`; admission sets
it, coalesced wakes retain it, and applying a wake clears it in the same transaction.

Collection reads generated sqlc queries and River's supported job-list API, uses a one-second
deadline, excludes future jobs/terminal-run timers and avoids loading large outcome values. Saved
envelope metadata includes evidence created after fencing or a terminal decision. Query/storage
failures fail collection rather than reporting zero backlog. No new dependency or HTTP endpoint is
required. Instrument semantics and the pending-file scan ceiling are documented in `running.md`.

`TestSchedulerMetricsCollectDurableBacklogAndFailures` collects with the SDK manual reader against
PostgreSQL and checks coalesced age preservation, future-job exclusion, expired ownership, unknown
outcomes, scope slots, pending fsynced evidence, due timers, actual publication rollback/cleanup
failure counters, cleared backlog, unrelated queues and a database outage. The second regression,
`TestSchedulerMetricsExportThroughConfiguredOTLP`, starts the real runtime and configured OTLP/HTTP
exporter and receives all eight gauges at a local HTTP collector. Both pass under the race detector.

The full CI measurement also found a stale timer-test expectation: 129 due timers correctly consume
in batches of 64, 64 and 1, but coalescing now leaves one viable scheduler delivery for four
committed wake generations. The fixture now verifies that delivery through the production viability
guard. This is a test correction, not a change to timer consumption or business execution.

The first full race-enabled CI run completed service integration in 821.585 seconds, while the queue
package reached Go's 15-minute package timeout during the natural-lease SIGKILL matrix. There were
no other test failures besides the corrected coalescing expectation above. CI now uses the existing
`make integration` 20-minute package limit and a 30-minute job limit that also covers infrastructure
and image setup. The complete rerun passed all eight CI integration packages with zero failures:
queue took 960.702 seconds and service integration took 862.343 seconds. It exercised PostgreSQL,
actual Docker/MCP fixtures, Temporal baseline comparison and all enabled runtime/service SIGKILL
matrices under the race detector. The backup test added afterward was verified separately below; the
CI environment now also enables that additional check.

## Verified stopped-host backup and restore

`TestRiverBackupRestoresHumanAndUncommittedOutcome` passed under the race detector in 23.32 seconds
(25.584 seconds for the package). It uses two actual CLI-admitted Python/Docker runs: one waiting at
a human request and one blocked in artifact publication after its fsynced envelope and complete
210,000-byte input-derived artifact already exist. A real service SIGKILL leaves the latter result
uncommitted. The fixture pauses native deliveries, removes its publication fault, and snapshots the
stopped database with the container's real `pg_dump --format=custom`, plus complete local/shared
directories with `tar`.

It then drops only its own temporary schema and removes its own data directories, restores using
`pg_restore --exit-on-error --single-transaction` and `tar -xpf`, and compares all twenty complete
`knotra_` domain tables byte for byte through canonical JSON. Engine identity, command receipts,
budgets, attempts, requests and event history survive. Before queues resume, the original human
ID/deadline and exact SSE prefix remain reachable, and the uncommitted artifact remains private.
Resuming maintenance publishes the original artifact/envelope; actual CLI human responses complete
both graphs. After a second engine restart, the fixture removes the completed queue record and
delivers the same finalizer again. It retains four code attempts/four tool debits, two terminal
events, zero active slots/resources and the original deadline and random nonce. Preparation never
repeats.

The stopped-host procedure is documented in `docs/running.md`. CI enables this check with
`KNOTRA_TEST_POSTGRES_CONTAINER=knotra-ci-postgres-1`; local runs should name the actual PostgreSQL
container serving their test DSN. The test destroys only its own isolated schema and directories,
never the baseline database or other test schemas. Online backup, network storage and simultaneous
worker hosts are outside this verified procedure and remain cluster acceptance work.

```sh
KNOTRA_TEST_POSTGRES_CONTAINER=knotra-river-migration-test \
  KNOTRA_TEST_RECOVERY=1 KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  KNOTRA_TEST_DATABASE_URL='postgres://knotra:knotra-test@127.0.0.1:25434/knotra?sslmode=disable' \
  go test -mod=readonly -race -count=1 -timeout=6m ./internal/integration \
  -run '^TestRiverBackupRestoresHumanAndUncommittedOutcome$' -v
```

## Native tracing and diagnostic privacy

Native delivery, bounded advancement, node execution and outcome publication now have OpenTelemetry
spans. Delivery attributes retain job/queue, engine, host and the validated domain identities.
Successful attempt claims and resource cleanup add the owning worker and ownership generation;
finalization retains the saved outcome's owner while `knotra.actor.id` identifies the recovering
process. Wake, dispatch and ownership generations have separate attributes. A capacity miss or stale
delivery never invents an ownership token.

Operation intent, call admission and response confirmation are events on the node span, each with
its operation ID. This retains all operations in an agent attempt instead of repeatedly replacing
one span attribute. Debug logs retain execution identity and successful boundaries; warning logs
retain failure type and identity. Queue errors and panic values are sanitized before River logs or
persists them. The error wrapper preserves `errors.Is`/`errors.As`, including River cancellation,
snooze and context interruption. Background/shutdown diagnostics use the same safe error boundary.

`TestRuntimeDiagnosticsKeepIdentityWithoutPrivateData` passed with real PostgreSQL, River and an
HTTP model provider under the race detector. All three physical requests carry the expected private
prompt, input and bearer credential; the provider sends a private output and reasoning field. A
PostgreSQL trigger fails exactly one actual publication with private error text, and native
finalization recovers the original result without another model call. The check inspects ended
spans/events, debug JSON logs and actual persisted River error history, asserting ownership and
operation identities and absence of every private sentinel and endpoint URL. The non-database
delivery check verifies preserved control-error identity and sanitized panic handling. The runnable
pair passed in 2.814 seconds for the package.

The current-source native PostgreSQL/Docker regression passed under the race detector: queue took
141.748 seconds and executor 2.833 seconds. Actual service checks for agent/MCP reuse, cancellation,
heartbeat loss and delayed human delivery passed in 34.154 seconds for integration. `make check`,
`make build`, Linux lint and Windows lint passed. Two earlier local queue invocations encountered
machine sleep: admission/completion wall time advanced by minutes while Go's monotonic test duration
advanced by seconds, correctly expiring the original deadlines. The successful repeat used macOS
`caffeinate -i` for the command duration; production deadlines and test limits were unchanged. The
separately enabled mixed-backend SIGKILL check passed both admission-switch directions in 17.99
seconds (20.068 seconds for the package), preserving original commands and receipts.

## Literal desktop acceptance against both backends

`TestDesktopLiteralBackendCompatibility` passed with the race detector in 43.33 seconds (45.233
seconds for the package): Temporal took 22.29 seconds and River 21.05 seconds. Each starts an
isolated production service and runs the existing Rust native acceptance plus four Playwright flows.
Native SQLite command receipts, binary upload/download and durable SSE cursor replay use the actual
engine. Browser checks cover model streaming/history from a fresh context, human response
validation, a nested Python binary copy with its child graph/inspector, an agent's file write/read
loop, and the hello starter. Each backend retains five successful runs, five actual HTTP model
requests, five root model debits and five root tool debits. Python, file tools and artifact bytes
are real; only the Ollama provider uses a deterministic HTTP fixture. This proves client/backend
compatibility, not live model quality or acceptance of all five starters.

The first client comparison exposed a production contract mismatch: native consumption changed the
internal human request status to `accepted`, which leaked through `HumanRequestPage` and caused the
desktop validator to disconnect. The public projection now maps consumed answers to the contract's
`answered` status while retaining internal evidence. The actual PostgreSQL/River human command test
verifies the HTTP page after consumption, alongside atomic answer reservation, receipt replay and
cancellation. It passed under the race detector in 2.597 seconds. The initial comparison also
exposed an invalid test-only SQL status-column reference; the final check now reads the run
document's public status. Failed evidence remains in the owned integration work directories.

`app/README.md` documents the opt-in command and prerequisites. The desktop workflow now has an
`engine-acceptance` Ubuntu job that installs the documented Tauri Linux libraries and Playwright
Chromium, compiles native tests before their runtime deadline, starts disposable PostgreSQL/Temporal
infrastructure, builds the helper/firewall and runs this same enabled comparison. Failure output
includes retained client/engine logs; infrastructure cleanup runs unconditionally. The workflow YAML
was parsed locally and its opt-in flag, compile ordering and cleanup condition checked.

A local repeat with `CI=1` passed in 55.24 seconds (57.315 seconds for the package): Temporal took
29.96 seconds and River 25.28 seconds. Playwright owned its Vite server for each backend rather than
reusing a prestarted server. This verifies the CI-mode runtime on macOS; the actual Ubuntu GitHub
job has not yet run. Live inference acceptance for the remaining starter packages, verification of
the GitHub job and final resource/latency acceptance remain open; the representative measurements
are updated below.

## Updated single-host resource evidence

The then-current rebuilt binary completed all sixteen production-service benchmark observations on
fresh disposable pinned containers, with no sample errors. Both backends preserve exact exports,
node counts, physical model calls and root model debits. The short-DAG median is Temporal 4.88
seconds versus River 2.15; the large-graph median is 68.93 versus 16.37. Deployment memory/CPU are
lower for River, but its PostgreSQL WAL remains approximately 2.75 times the short-DAG baseline and
2.31 times the large-graph baseline. Full allocated PostgreSQL/history directory snapshots now
expose disk growth; reused WAL allocation is explicitly accounted for as a measurement limitation.

The
[resource report](scheduler-resource-benchmark.md#current-source-comparison-after-coalescing-and-clock-changes)
contains the tables and archived JSON with the actual binary digest. Black formatting, Python
compilation, all sixteen measurement checks and allocated-storage arithmetic passed. This updates
the existing workloads; nested foreach, long agents, latency/retention measurements and numerical
budgets remain required. It does not complete the resource release gate.

The subsequent nested-foreach comparison verifies a complete ordered 8×8 export, 73 instances and 64
actual HTTP model requests/root debits on both backends. Three repeats first exposed River's
notification throttle: median elapsed 8.48 seconds and first physical call 2.81 seconds. Lowering
the pinned SDK's fetch/insert-notification cooldown from 100 ms to 1 ms, while retaining a
one-second idle poll interval, reduced these medians to 4.73 seconds and 176 ms. Advance queue p95
waits dropped from 894–973 ms to 54–68 ms. Before/after and SQL-profile reports are archived in the
[resource document](scheduler-resource-benchmark.md#nested-foreach-and-notification-latency).

The after comparison remains approximately 23% slower than Temporal and uses more SQL/CPU. The
profile locates repeated sibling graph reads during focused outcome publication, including 1,580
graph passes and 4,614 run/clock reads in its final repeat. Bounded focused advancement with
fairness/recovery validation was the next optimization, implemented below. `make check` passes with
zero lint issues; the current-source PostgreSQL/Docker race regression passed for queue in 118.364
seconds and executor in 2.960 seconds. Black, Python compilation and all focused benchmark
export/count checks passed. The earlier full four-workload resource comparison predates this tuning
and must be refreshed after the remaining optimizations.

Focused publication now selects only the result instance's graph without changing the durable global
cursor or ending a partial global pass. Scan progress and a general transactional wake preserve
parent propagation, freed-slot admission, stop decisions and sibling fairness. The new PostgreSQL
check injects rollback, preserves an ahead-of-focus cursor and sibling wait, reopens the store, and
completes the original ordered `[3,1,2]` export through normal advancement. It passed under the race
detector in 2.628 seconds.

The [focused foreach report](scheduler-resource-benchmark.md#focused-result-publication) passed
three repeats per backend with unchanged complete 8×8 exports, 73 instances and 64 physical
requests/root debits. River median elapsed is 2.88 seconds versus Temporal 3.63, with deployment CPU
1.98 versus 2.53 seconds. River query median decreases from 35,088 to 23,109; the final SQL profile
has 748 scope/graph passes and 2,978 run/clock reads, versus the earlier 1,580/4,614. Its WAL still
exceeds Temporal, so this does not close the resource gate.

`make check` passed with zero lint issues. The complete ordinary PostgreSQL/Docker queue and
executor race runs passed in 123.060 and 3.043 seconds. Explicit SIGKILL checks at
`child_before_commit` passed for foreach, loop and pipeline in 68.985 seconds for the package,
preserving committed child state, original deadlines, physical operations and one-time publication.
Other process-loss boundaries remain part of the final complete matrix, rather than being inferred
from these three selected checks.

The current-source literal desktop comparison also passed with `CI=1` in 43.59 seconds (45.873
seconds for integration): Temporal took 23.80 seconds and River 19.79. Both retain the native SQLite
receipts/SSE check and four browser flows with exactly five successful runs, five physical HTTP
model requests and five root model/tool debits. The provider remains a literal fixture; this is
client compatibility evidence, not live-model or Ubuntu CI execution evidence.

## Named single-host recovery audit

The plan names the following process-loss boundaries. Each maps to an existing runnable check. The
complete CI-equivalent run above passed all enabled checks; the six-minute production-runtime outage
is a separately verified opt-in check. These are single-host checks, with the plan's
client/cluster/resource gates still outstanding.

| Required boundary                           | Existing check and evidence                                                                                                                                                                                                                      |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Before/after admission commit               | `TestRiverAdmissionProcessCrashBoundaries`: delivery insertion, admission commit and lost receipt; real CLI/API input, original receipt and exactly one physical generation.                                                                     |
| Fetched before domain claim                 | `TestRuntimeProcessCrashBoundaries/fetched_before_claim`: no domain ownership, intent, slot or physical call before SIGKILL; replacement claims the same business attempt.                                                                       |
| Ownership claim and operation intent        | Runtime modes `ownership_claimed_before_intent`, `prepared_intent_before_admission` and `operation_admitted_before_send`: distinguish uncharged prepared intent from charged admission; original deadline and explicit safe retry limits remain. |
| External acceptance before response journal | Runtime mode `external_accepted_before_journal` and agent-prefix service checks retain unconfirmed evidence and require resolution, with actual provider/MCP effects measured independently.                                                     |
| Journaled response before envelope          | Runtime mode `response_journaled_before_envelope` recovers confirmed response bytes with one physical call and no extra debit.                                                                                                                   |
| Saved artifact bytes and metadata           | `TestRuntimeArtifactPublicationProcessCrashBoundaries`: synced bytes, private metadata and publication boundaries preserve the original artifact ID/nonce and HTTP visibility.                                                                   |
| Saved envelope, before/after result commit  | Runtime modes `envelope_saved_before_result_commit`/`result_committed`, artifact publication checks and `TestRiverStopAndSavedResultProcessCrashBoundaries` retain immutable completion, events, budgets and next wake.                          |
| Child completion before parent advancement  | Five foreach and eight loop/pipeline process boundaries preserve materialized children, iteration/collection/carry values, original deadlines and completed prefixes.                                                                            |
| Human response and resolution acceptance    | `TestRiverHumanAnswerProcessCrashBoundaries` and six `TestRiverResolutionProcessCrashBoundaries` modes verify commit/lost-receipt boundaries, stable requests, replay and original deadlines.                                                    |
| Cancellation and fail-fast commit           | `TestRiverCancellationProcessCrashBoundaries` and MCP/sandbox/agent fail-fast process tests verify before/after stop commit, actual interruption, stable receipts and protected completed work.                                                  |
| Migration/registration startup and shutdown | `TestRiverStartupProcessCrashBoundaries` and five shutdown cases cover migration, worker registration, client binding, bounded drain and forced interruption.                                                                                    |
| Database unavailable beyond five minutes    | Runtime mode `database_outage_saved_envelope` preserves complete fsynced evidence across six minutes of disconnected PostgreSQL and SIGKILL, without aging leases or repeating the effect.                                                       |

Real PostgreSQL request/expiry contention, nine physical result/stop/deadline races, ownership-aware
operation admission, late-evidence finalization and once-only slot release complement those process
checks. MCP initialization identity and original credentials survive process loss before database
commit; physical cleanup protects live peers and retries the original remote session. Multiple-host,
shared-storage failure, desktop and resource acceptance remain open.

## Internal process-role boundary

`queue.Config.Role` selects `all` (the default), `api`, `scheduler` or `executor`. API clients only
insert jobs; scheduler clients consume advance/finalize queues; executor clients consume execution
and host-specific cleanup queues. Each role requires its own consumers and rejects unknown roles or
missing handlers. Split roles may insert validated typed jobs for another client without registering
that client's consumers. Scheduler maintenance covers timers, outcomes and delivery repair; executor
maintenance covers local resources and sessions. API runtimes retain their incarnation heartbeat
without running reconciliation polls or River consumer services.

Worker registration persists the role in existing `capabilities` JSON. Attempt claims require
compatible execution versions and role `all` or `executor`, before acquiring scope slots or
admitting operations. Existing role-less worker records retain combined behavior. Role selection
therefore fences both queue consumption and direct domain claims.

`TestSplitRolesExecuteDAGThroughIndependentClients` starts three separate runtime/client objects
against one PostgreSQL schema and shared outcome/artifact storage. With only API and scheduler
started, execution remains queued and physical calls remain zero. Direct API/scheduler claim
attempts return no ownership. Starting the executor completes the original three-node DAG with exact
full export, three physical HTTP model calls, three root budget debits and three outcomes owned by
that executor. This is an in-process role-boundary check, not a two-host acceptance test.

The `serve` command still uses the combined role and exclusive engine lease. CLI role wiring, worker
profile/helper/secret capabilities, placement affinity and real multi-host failure/storage
acceptance remain required before exposing split-process deployment. The production resource reports
above describe the earlier scheduler-tuning binary, before this internal role change.

Validation: the split-role/claim checks passed with real PostgreSQL and the race detector; the
complete ordinary queue suite passed in 93.214 seconds and store in 7.756 seconds. The initial broad
run exposed a direct-blocker assumption in request-race synchronization. Its middle waiter now uses
the existing recursive blocking ancestry check, matching the API waiter. All four human/resolution
node/root deadline cases passed three repetitions (42.348 seconds for the package); original
deadlines and response/expiry assertions are retained. `make check` passes with zero lint issues.
Process-crash and two-host release acceptance remain separate gates.

## Remaining implementation

The [queue-retention probe](scheduler-resource-benchmark.md#queue-retention-and-maintenance-probe)
passes with 10,000 expired jobs removed and six fresh/nonterminal jobs retained; execution history,
attempts, budgets and events are unchanged. Cleaner DELETE statements take 20.549 ms and generate
540,000 bytes of WAL in this single observation. The native runtime race check now uses the real
cleaner and verifies that redelivery after retention preserves the original full outputs and exactly
three physical model calls. Representative workload, latency, parked-wait recovery and bounded
maintenance probes are recorded; numerical release budgets remain to be agreed.

The
[current consolidated resource report](scheduler-resource-benchmark.md#current-consolidated-workloads-october-6-2026)
passes all 22 observations on the current binary after cooldown/focused-publication tuning: DAG,
large controls, nested foreach, idle and minute-long human waits. River's median large-graph time is
5.217 seconds versus Temporal's 66.014; deployment memory is lower for every workload. River still
generates more WAL, and DAG/nested-foreach query counts remain approximately 3.1/5.9 times
Temporal's. Long-agent, deadline and parked-human recovery measurements below complement this
cohort. The bounded retention probe above adds maintenance evidence; numerical release budgets and
the broader release acceptance remain open.

The [human wait recovery comparison](scheduler-resource-benchmark.md#human-wait-process-recovery)
adds three production-service SIGKILL/restarts per backend, preserving 32 original requests,
deadlines and instances each time. River's median API readiness is 111.5 ms; responding to all
requests completes the run in 380.1 ms from the first response command. This verifies parked
single-host waits and does not close active-operation/outage or cluster recovery acceptance.

The complete [long-agent comparison](scheduler-resource-benchmark.md#long-agent-comparison) adds
three real-sandbox attempts per backend with exact model/MCP counts and root budget debits. River's
median deployment memory is 123.9 MiB versus Temporal's 250.0 MiB; CPU is 4.210 versus 4.071
seconds. This closes the long-agent workload measurement, not the complete resource gate.

The [human latency comparison](scheduler-resource-benchmark.md#human-response-and-deadline-latency)
adds three deadlines and 96 accepted answers per backend. River's median deadline completion lag is
564.9 ms versus Temporal's 135.4 ms; median per-run answer completion p50 is 2.0 ms versus 3,169.7
ms. These are persisted completion measurements, separate from HTTP polling and process recovery
latency. Resource/release acceptance still requires the remaining measurements and gates.

The explicit storage/basic-graph/nested-control exit criteria in phases 3–5 now have implemented
code and verified checks. Their broader release acceptance remains in phases 6–9; these preliminary
gates do not establish cluster readiness, resource acceptance or default cutover:

- **Phase 3:** six concurrent initial-schema opens preserve one engine identity and existing data;
  two concurrent River migrators resume target 8 with single-connection pools. Actual service
  SIGKILL covers migration, registration and delivery-client binding. Native PostgreSQL tests cover
  atomic admission/completion rollback, competing claims, expired/stale ownership, once-only scope
  release, receipts and events. The complete CI-equivalent run passes these checks.
- **Phase 4:** the same 23 controlled-clock scenarios run against the Temporal baseline and the pure
  scheduler at bounds 1, 2 and 64, including empty graphs, branch/needs skips, ordered coalesce,
  optional values and invalid outputs. Literal HTTP/MCP/Docker service tests exercise actual leaf
  execution; native repeated-delivery, publication rollback and expired-owner finalization tests
  verify one physical operation and immutable results.
- **Phase 5:** human, foreach, loop and nested pipeline controls combine with the four literal leaf
  types and switch to cover all nine node types. Five foreach and eight loop/pipeline SIGKILL
  boundaries retain indexes, collection/carry values and original deadlines. Human bodies occupy
  open-body capacity without leaf slots and resume out of order without rematerialization. Nested
  permission scenarios and fourteen literal root/child budget comparisons retain ancestor scope
  behavior and complete child inputs/exports.

| Phase | Remaining work                                                                                                                                                                                  |
| ----- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 6     | Final full-matrix release verification after remaining client, cluster and cutover work; the named single-host recovery boundaries are mapped to passing checks above.                          |
| 7     | Remaining live starter acceptance, Ubuntu client CI execution and final resource/latency budgets; representative workload/retention probes and both-backend literal desktop checks pass.        |
| 8     | Host capabilities, process roles, sandbox/MCP affinity, shared-storage failure and two-host backup/failure acceptance; filesystem storage paths and host identity/staging are implemented.      |
| 9     | Verified default cutover, Temporal retirement and fresh setup/release/CI/documentation updates. Stopped single-host River backup/restore is verified; no deployed legacy-run drain is required. |

The final acceptance audit must cover every release criterion and crash injection point in the plan.
Passing the current tests does not complete the migration.
