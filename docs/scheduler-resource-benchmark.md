# Single host scheduler resource comparison

The benchmark runs the production `knotra serve` binary with each backend through the v1 HTTP API.
It uses the same PostgreSQL instance, fresh backend schemas, the same engine profile, concurrency
16, and a local OpenAI-compatible HTTP fixture with a 10 ms response delay. It verifies complete run
exports, the number of node instances, physical model requests and persisted model budget debits. No
paid provider is involved.

This comparison covers a 64-node DAG with four layers of 16 model nodes, 1,000 switches plus four
model nodes with 512 KiB prompts, idle startup, and 32 simultaneous human requests held open for 60
seconds before cancellation. Each active graph runs three times in one backend process; caches,
history and storage accumulate across repeats. Temporal runs first, then River. Both backends start
from fresh application schemas. The resource sampler runs approximately once per second and the HTTP
completion poll runs every 250 ms.

The October 6, 2026 run used an Apple M4 with 10 logical CPUs and 16 GiB host RAM on macOS 27.0.1
arm64. Docker 29.4.0 reported 10 CPUs and approximately 8 GiB VM memory. Both backends used the same
production binary; its SHA-256, database version, image IDs and container caps are recorded in the
JSON report. The checkout was dirty, so the Git HEAD alone does not identify that binary's source.

Reports identify the measured binary, rather than subsequent source changes. The scheduler-tuning
cohorts below precede internal process-role changes; later builds can have a different digest.

## Current consolidated workloads, October 6, 2026

The [current consolidated report](benchmarks/scheduler-current-workloads-2026-10-06.json) is
complete: 11 observations per backend, empty sampler error lists, and the measured production binary
digest. It runs after notification cooldown and focused result publication tuning. This
fresh-container comparison adds nested foreach after the large graph and before the human wait. Each
active graph has three repeats; idle and the minute-long human wait have one observation. The
previous interrupted partial comparison is excluded.

| Workload                      | Temporal elapsed, s | River elapsed, s | Temporal deployment memory, MiB | River deployment memory, MiB | Temporal deployment CPU, s | River deployment CPU, s |
| ----------------------------- | ------------------: | ---------------: | ------------------------------: | ---------------------------: | -------------------------: | ----------------------: |
| Idle, 15 s                    |              15.005 |           15.005 |                           255.6 |                        109.8 |                      0.891 |                   0.883 |
| Short DAG, 64 model nodes     |               4.889 |            2.287 |                           329.3 |                        182.8 |                      3.077 |                   1.679 |
| Large graph, 1,004 nodes      |              66.014 |            5.217 |                           537.5 |                        374.8 |                     38.456 |                   4.524 |
| Nested foreach, 73 instances  |               5.144 |            3.153 |                           551.9 |                        410.5 |                      3.577 |                   2.141 |
| Human wait, 32 requests, 60 s |              63.500 |           60.293 |                           552.6 |                        437.1 |                      7.293 |                   4.120 |

River is approximately 2.1 times faster on the short DAG and 12.7 times faster on the large graph in
this cohort. Sampled deployment memory is lower for every workload. Idle CPU is essentially
unchanged; active-workload CPU is lower here. The separate long-agent report below retains a small
CPU increase for River, so this is not a claim that every workload saves CPU. Nested foreach runs
after the large graph in the same process; its memory includes retained process/database state. Its
values should not be substituted for the isolated focused-publication comparison below.

| Workload database metric | Temporal query calls | River query calls | Temporal WAL, MiB | River WAL, MiB |
| ------------------------ | -------------------: | ----------------: | ----------------: | -------------: |
| Short DAG                |                3,656 |            11,183 |             0.953 |          2.686 |
| Large graph              |               23,255 |            29,448 |             6.934 |         15.360 |
| Nested foreach           |                3,852 |            22,731 |             1.115 |          3.807 |
| Human wait               |                2,410 |             1,920 |             0.716 |          4.397 |

| Active workload latency | Temporal admission, ms | River admission, ms | Temporal first physical call, ms | River first physical call, ms |
| ----------------------- | ---------------------: | ------------------: | -------------------------------: | ----------------------------: |
| Short DAG               |                    8.5 |                75.1 |                          2,831.4 |                         165.4 |
| Large graph             |                  103.8 |               196.3 |                         50,385.6 |                       1,679.6 |
| Nested foreach          |                    5.6 |                16.1 |                          1,216.8 |                         173.5 |

The database tradeoff remains: River's median DAG/nested-foreach query counts are approximately
3.1/5.9 times Temporal's, and WAL remains higher in all four active observations. Application
connection peaks reach 12 for River and 11 for Temporal. The large graph's first model call waits
for scheduler progress through the preceding control nodes; it is not a pure queue fetch latency.
Deadline lateness and answer/restart latency are measured separately below. This report does not
establish queue retention/maintenance cost or a complete release acceptance budget.

Allocated PostgreSQL storage spans **47.47 to 96.86 MiB** during Temporal and **81.03 to 216.12
MiB** during River; Temporal's allocated history directory spans **0.65 to 16.11 MiB**. Engine data
directories end at **18,162,892 bytes** for Temporal and **191,188 bytes** for River. The same
PostgreSQL container is reused, so these snapshots include allocation reuse and are not independent
fresh-volume disk comparisons. Maximum sample gaps are **1.171 seconds** for Temporal and **1.133
seconds** for River. The script validates all 64 DAG calls, all four large-graph calls, the exact
8-by-8 foreach export/64 calls/73 instances, and a cancelled human run with 32 instances, with
matching root model debits and zero physical/root tool debits.

To reproduce this cohort, add `--workloads short_dag large_controls nested_foreach human_wait` to
the command below, retaining `--repeats 3 --idle-seconds 15 --wait-seconds 60`.

## Baseline before advance coalescing, October 6, 2026

The [archived JSON report](benchmarks/scheduler-single-host-2026-10-06.json) completed successfully
with all 16 measurements and no sampling errors. The table gives medians of three repeats for active
graphs and the single observation for idle/human waits. Memory is the sampled peak of each
observation; CPU is accumulated CPU seconds, not elapsed seconds or a percentage.

| Workload                      | Temporal elapsed, s | River elapsed, s | Temporal deployment memory, MiB | River deployment memory, MiB | Temporal deployment CPU, s | River deployment CPU, s |
| ----------------------------- | ------------------: | ---------------: | ------------------------------: | ---------------------------: | -------------------------: | ----------------------: |
| Idle, 15 s                    |               15.01 |            15.01 |                           236.9 |                        111.1 |                       0.54 |                    0.67 |
| Short DAG, 64 model nodes     |                5.40 |             6.83 |                           327.9 |                        182.3 |                       2.50 |                    1.95 |
| Large graph, 1,004 nodes      |               66.05 |            16.96 |                           521.4 |                        358.1 |                      31.19 |                    6.28 |
| Human wait, 32 requests, 60 s |               64.08 |            60.25 |                           598.6 |                        376.4 |                       5.38 |                    2.84 |

The large graph is approximately 3.9 times faster in this fixture, with 31% lower sampled deployment
memory and 80% lower deployment CPU. The short DAG uses less deployment memory/CPU but is 27% slower
by median. River's three short-DAG observations were **1.97, 6.83 and 7.24 s**, with first physical
calls at **222, 5,145 and 5,040 ms** from run creation. Temporal's first-call median was **3,065
ms**. These observations motivated the advance coalescing follow-up below; this baseline does not
describe the subsequently fixed queue behavior.

| Active workload metric, median     | Temporal short DAG | River short DAG | Temporal large graph | River large graph |
| ---------------------------------- | -----------------: | --------------: | -------------------: | ----------------: |
| Admission round trip, ms           |                6.3 |            78.2 |                 97.7 |             187.7 |
| Engine RSS peak, MiB               |               72.1 |            51.3 |                209.7 |             209.9 |
| Engine database connection peak    |                  8 |              12 |                    9 |                11 |
| Database query calls               |              3,666 |          12,526 |               23,269 |            33,023 |
| Database WAL delta, MiB            |              0.957 |           3.241 |                6.840 |            15.372 |
| Application schema net growth, MiB |              0.695 |           1.648 |                3.930 |            13.453 |

Removing Temporal reduces required deployment components, but River moves substantial work into
PostgreSQL: short-DAG WAL and query calls are about **3.4 times** the baseline, and large-graph WAL
is about **2.2 times** the baseline. Admission is also slower because the River command performs
domain preparation in its transaction. The same default application pool is used; the observed
connection peaks reflect backend behavior rather than a separately capped equal connection count.
The database cost still needs accounting before setting release budgets. The start delay is
addressed below.

The total engine data directory sizes at the end were **18,157,800 bytes** for Temporal and **98,500
bytes** for River. These exclude Temporal's SQLite history and PostgreSQL storage; they are not
total deployment disk usage. Net schema growth can decrease during a later observation and must not
be interpreted as the amount of data written by that workload.

## Current source comparison after coalescing and clock changes

The [current-source report](benchmarks/scheduler-current-single-host-2026-10-06.json) completed all
16 measurements with no sample errors. The production binary was rebuilt after the tracing and
desktop human-status changes. Hardware, pinned images, concurrency, fixtures and observation lengths
match the baseline above; fresh disposable containers were created for this repeat. Maximum sample
gaps were 1.23 seconds for Temporal and 1.13 seconds for River. Active rows show three-repeat
medians; idle and human wait each have one observation.

| Workload                      | Temporal elapsed, s | River elapsed, s | Temporal deployment memory, MiB | River deployment memory, MiB | Temporal deployment CPU, s | River deployment CPU, s |
| ----------------------------- | ------------------: | ---------------: | ------------------------------: | ---------------------------: | -------------------------: | ----------------------: |
| Idle, 15 s                    |               15.01 |            15.01 |                           224.3 |                        115.3 |                       0.82 |                    0.64 |
| Short DAG, 64 model nodes     |                4.88 |             2.15 |                           314.4 |                        173.6 |                       2.40 |                    1.37 |
| Large graph, 1,004 nodes      |               68.93 |            16.37 |                           513.6 |                        368.6 |                      31.14 |                    6.24 |
| Human wait, 32 requests, 60 s |               63.41 |            60.27 |                           500.6 |                        366.2 |                       5.21 |                    2.88 |

The short DAG is 2.27 times faster with approximately 45% lower deployment memory and 43% lower
deployment CPU. The large graph is 4.21 times faster, with approximately 28% lower memory and 80%
lower CPU. River's first short-DAG calls occur at 161–226 ms across all three repeats, confirming
that the earlier 5-second admission backlog is absent in this comparison.

| Active workload metric, median     | Temporal short DAG | River short DAG | Temporal large graph | River large graph |
| ---------------------------------- | -----------------: | --------------: | -------------------: | ----------------: |
| Admission round trip, ms           |                6.2 |            88.4 |                 97.8 |             181.0 |
| First physical call, ms            |            2,817.5 |           188.1 |             50,568.9 |           5,791.1 |
| Engine RSS peak, MiB               |               69.8 |            51.5 |                204.4 |             206.9 |
| Engine database connection peak    |                  8 |              12 |                   10 |                12 |
| Database query calls               |              3,654 |          10,749 |               23,357 |            29,729 |
| Database WAL delta, MiB            |              0.962 |           2.643 |                6.653 |            15.348 |
| Application schema net growth, MiB |              0.648 |           1.438 |                3.969 |            13.359 |

The PostgreSQL tradeoff remains: River writes approximately 2.75 times the short-DAG WAL and 2.31
times the large-graph WAL, with slower admission and more connections/query calls. Temporal's
separate history writes are excluded from those PostgreSQL counters. Engine memory alone is similar
for the large graph; the measured deployment reduction includes removing the Temporal service.

The new allocated-directory snapshots show PostgreSQL growing from **47.63 to 94.52 MiB** during
Temporal's observations and from **81.01 to 191.60 MiB** during River's. Temporal's history
directory grows from **0.65 to 14.33 MiB**. Final engine data content is **18,157,890 bytes** for
Temporal and **98,504 bytes** for River. PostgreSQL is reused between backends, including WAL
allocation retained after the first schema is dropped; these directory sizes are neither per-run
write volume nor proof of lower River disk use. The report exposes the complete directories rather
than inferring disk savings from the much smaller engine data directory.

Exports, node counts, physical model calls and root model debits passed for every run. These results
update the four existing workloads; nested foreach, long agents, timer/transaction/recovery latency,
retention cost and numerical operational budgets were still open at this point. Later sections add
nested, agent, timer, parked-wait recovery and bounded retention evidence; transaction duration,
broader recovery acceptance and numerical budgets still require final release verification.

The comparison above predates the notification cooldown tuning below; its binary digest identifies
that version. Repeat the full four-workload comparison after subsequent scheduler tuning.

## Advance backlog diagnosis and fix

The [before/after diagnostic reports](benchmarks/river-advance-coalescing-2026-10-06.json) reproduce
six consecutive 64-node DAGs on River and inspect admission backlog and native job timings. Before
the fix, each DAG created **130 advance jobs**. A separate three-repeat probe observed **80 and 97
available advance jobs belonging to succeeded runs** before the next admission. The new run's first
advance started after roughly 4–5 seconds, while execution delivery after that advance was fast. The
model fixture was not the source of the delay.

`Runtime.Wake` now retains an existing viable, unconsumed advance delivery for the same run. The
domain wake generation still increases for every committed mutation, and the older job consumes the
latest generation while holding the run lock. A consumed identity, malformed/mismatched job, dead
worker, expired handler or future backoff cannot suppress a new immediate wake. Delivery repair uses
the same viability check. Human/resolution commands still apply their domain changes inside the
receipt transaction before queue insertion; coalescing does not defer an accepted answer past its
original deadline.

| Six-repeat diagnostic metric         |   Before |    After |
| ------------------------------------ | -------: | -------: |
| DAG elapsed median, s                |     6.75 |     1.86 |
| First physical model call median, ms |    5,061 |      166 |
| Advance jobs per DAG                 |      130 |      4–9 |
| Database WAL median, MiB             |    3.174 |    2.657 |
| Database query calls median          | 12,517.5 | 12,046.5 |

All six fixed runs completed in **1.80–2.25 seconds**, with first model calls at **156–231 ms** and
exactly **64 physical calls, 64 model debits and 64 node instances**. This focused probe uses a
one-second idle observation and freshly created schemas on a reused database. It diagnoses the start
delay; it does not replace a new complete Temporal/River resource comparison after the fix. WAL
decreased modestly, while the greater PostgreSQL query/WAL cost remains a separate concern.

The concurrent burst regression commits 80 wake mutations through eight callers, retains one advance
job and all 81 generations, and confirms that maintenance does not churn the counter. Native
delivery still produces the exact export with one physical call. Repair checks also cover future
backoff, malformed arguments, mismatched run identity and impossible future generation. These checks
complement the real PostgreSQL request/deadline and physical publication race suite.

For a focused rerun, add `--backends river --workloads short_dag --repeats 6 --idle-seconds 1` to
the benchmark command. River records `advance_backlog_before_admission` and `delivery_timings`,
including first node/job starts and p50/p95/maximum native queue waits. These are direct database
timestamps; first model call timestamps come from the host fixture.

## PostgreSQL statement cost and database clock

The [statement counter profiles](benchmarks/river-statement-cost-2026-10-06.json) capture three
short-DAG repeats before and after combining the run lock and database clock read. Both profiles use
production HTTP/provider execution and verify 64 physical calls, budget debits and node instances
per repeat. SQL text is PostgreSQL's normalized statement text, truncated at 2,000 characters.
Capture it only on a disposable database; the regular benchmark leaves this off.

| Three-repeat profile metric, median | Before |  After |
| ----------------------------------- | -----: | -----: |
| Database query calls                | 12,113 | 10,687 |
| Separate database clock calls       |  1,781 |    449 |
| Elapsed time, s                     |   2.27 |   1.87 |
| WAL LSN delta, MiB                  |  2.651 |  2.597 |

`LockExecutionRun` previously selected the locked metadata, then issued a separate clock query. It
now materializes `SELECT ... FOR UPDATE` in a CTE and evaluates `clock_timestamp()` in the outer
projection. Putting the clock in the inner select would evaluate it before a contended row lock
finishes. The existing real PostgreSQL lock-wait and attempt-admission deadline checks verify that
the returned time is after the wait and cannot grant capacity past the original deadline.
Ownership/budget checks still sample fresh database time at their required boundaries.

This eliminates approximately 1,330 separate clock round trips per DAG and reduces total calls by
about 12% in the profile. The elapsed observations are a small local sample, not a general
throughput guarantee. The change targets read traffic; it does not claim a material WAL reduction.

For the final repeat after the change, PostgreSQL attributed **2,639,971 bytes** of statement WAL
against **2,694,104 bytes** of LSN growth. Approximately 98% of that interval is accounted for by
statement counters; the rest includes transaction/background/observer accounting outside those
statement records. The largest categories are concrete execution records:

| Recorded writes in that repeat                    | Calls | Statement WAL, bytes |
| ------------------------------------------------- | ----: | -------------------: |
| Authoritative node insert/update                  |   192 |              362,400 |
| Scheduler lifecycle events                        |   258 |              309,461 |
| Adapter observation events                        |   256 |              299,223 |
| Public instance projections                       |   256 |              159,742 |
| River insert/fetch/completion statements combined |   167 |              402,212 |

The remainder includes claims, outcomes, results, slot reservations/releases, operation intent and
response journals, budget updates, deadlines and run counters. River stores authoritative graph
progress in PostgreSQL; Temporal's equivalent orchestration/history writes are in its separate
SQLite server in this deployment. Comparing PostgreSQL WAL alone therefore omits part of the
Temporal baseline's write cost. The profile explains where River's PostgreSQL writes go; full
deployment disk/I/O measurements and numerical operational budgets remain required. Durable
evidence, publication events and external-action fences have not been removed to reduce writes.

Add `--profile-sql` to a focused benchmark command to record per-statement call, SQL time, WAL byte,
WAL record and full-page-image deltas. SQL execution time includes lock waits and overlapping
statements, so it must not be read as CPU consumption. These extra snapshots add observer work and
can include completion of earlier deliveries; the samples are sequential, with fresh backend schemas
on a reused database.

## Nested foreach and notification latency

The new `nested_foreach` workload has eight outer rows and eight inner columns, with concurrency
four at each level and a shared root limit of sixteen. It creates 73 instances, including 64 real
HTTP model requests. The fixture reads the complete row/column input context and returns
`row * 8 + column`; column-dependent 10–31 ms response delays vary completion order. The benchmark
asserts the exact ordered 8×8 export, instance count and 64 root model debits on both backends.

Both the [before](benchmarks/scheduler-nested-foreach-before-2026-10-06.json) and
[after](benchmarks/scheduler-nested-foreach-after-2026-10-06.json) comparisons completed three
repeats per backend on fresh disposable pinned containers, without sample errors. They use a
one-second idle observation and the same model fixture; they are focused diagnostics rather than the
full comparison.

| Three-repeat median     | Temporal before | River before | Temporal after | River after |
| ----------------------- | --------------: | -----------: | -------------: | ----------: |
| Elapsed, s              |            4.13 |         8.48 |           3.84 |        4.73 |
| First physical call, ms |           1,140 |        2,809 |          1,051 |         176 |
| Deployment CPU, s       |            2.63 |         3.27 |           2.72 |        3.22 |
| Database query calls    |           3,816 |       29,815 |          3,809 |      35,088 |
| PostgreSQL WAL, MiB     |           1.084 |        3.776 |          1.121 |       4.025 |

River's pinned SDK uses `FetchCooldown` both to debounce queue fetches and throttle insert
notifications. Its default 100 ms window can suppress the notification for a new bounded graph
continuation inserted by a fast transaction. The next periodic poll then delivers it roughly one
second later. Before tuning, advance queue p95 waits were 894–973 ms; after setting a 1 ms cooldown,
they were 54–68 ms, with maximum waits 103–108 ms. Idle polling remains at one second. First
physical calls now arrive in 134–237 ms, and foreach elapsed median drops approximately 44%.

The remaining cost is explicit: River still uses more CPU/queries and remains approximately 23%
slower than Temporal in the after comparison. A
[three-repeat SQL profile](benchmarks/scheduler-nested-foreach-sql-2026-10-06.json) reproduces the
remaining cost with exact exports and operation counts. Its final repeat performs 1,580 repeated
graph-selection/scope/control reads and 4,614 locked run/clock reads for 73 instances. The latter
accumulate 24.73 seconds of SQL time, including overlapping lock waits, not CPU. Publication
advances revisited runnable sibling graphs as well as the focused result graph. The bounded focused
advancement below addresses that repeated publication work; durable records and ownership checks are
retained.

### Focused result publication

Publication now selects only the graph containing its result instance. It preserves the general scan
cursor and records scan progress without declaring a partial focused visit to be a completed global
pass. A transactional general wake still propagates child completion, freed slots and stop
decisions; normal advancement retains the bounded global scan and answered-request priority.

The [three-repeat comparison and SQL profile](benchmarks/scheduler-focused-foreach-2026-10-06.json)
completed with no sample errors and unchanged 73 instances/64 physical calls/64 root debits. River's
median elapsed falls from 4.73 to **2.88 seconds**, compared with **3.63 seconds** for Temporal in
this repeat. Median deployment CPU is **1.98 versus 2.53 seconds**. River's query median falls from
35,088 to **23,109**. Its final repeat has 748 scope/graph passes and 2,978 run/clock reads, versus
1,580 and 4,614 in the earlier SQL profile. Locked run/clock SQL time drops from 24.73 to 11.46
seconds; those totals include overlapping waits. PostgreSQL WAL remains higher than Temporal:
**3.769 versus 1.087 MiB** by median. This focused comparison does not close the remaining resource
release gates or replace a refreshed full-workload comparison.

`TestRuntimeFocusedGraphPreservesScanAndRecoversSiblingAnswers` verifies transactional rollback,
preserved cursor, untouched sibling state during focused advancement, then reopening the store and
consuming sibling answers/parent collection through normal advancement. It retains the ordered
`[3,1,2]` export and four instances. The isolated PostgreSQL race check passed in 2.628 seconds.

To reproduce the focused comparison, add `--workloads nested_foreach --repeats 3 --idle-seconds 1`
to the command below; add `--backends river --profile-sql` for the SQL diagnostic. The script's
original default workloads remain unchanged so earlier comparisons are reproducible.

For a long agent comparison, use `--workloads long_agent --repeats 3`. Build the Linux helper with
`make helper`, ensure `python:3.13-alpine` is available to Docker, and set `KNOTRA_SANDBOX_HELPER`
to the absolute `.knotra/bin/sandbox-helper` path. On macOS, prefix the command with
`caffeinate -i env` to prevent system sleep during the observation. Each agent performs 33 model
requests and 32 sequential MCP calls, with one second of fixture latency per request/call. The model
selects the advertised MCP tool and sums the actual tool responses. Acceptance requires export
`496`, values `0..31` in order, matching root model/tool budget debits, attempt `.a1`, and exactly
one observed Docker sandbox. The model and MCP HTTP servers are local fixtures; this measures
orchestration and sandbox resources, not live model inference or a remote MCP deployment.

## Long agent comparison

The [October 6 long-agent report](benchmarks/scheduler-long-agent-2026-10-06.json) is complete:
three approximately 67-second attempts per backend plus a 15-second idle observation. All six
attempts retain export `496`, 33 physical model calls/root model debits, 32 physical MCP calls/root
tool debits, one execution attempt and one observed sandbox. Both sampler error lists are empty.

| Median per agent attempt              |  Temporal |     River |
| ------------------------------------- | --------: | --------: |
| Elapsed time                          |  67.111 s |  67.908 s |
| Deployment peak memory                | 250.0 MiB | 123.9 MiB |
| Deployment CPU                        |   4.071 s |   4.210 s |
| Admission                             |    9.7 ms |   27.9 ms |
| First physical model call             |  495.0 ms |  172.1 ms |
| Database query calls                  |     3,367 |     6,282 |
| WAL generated                         | 0.481 MiB | 0.544 MiB |
| Peak application database connections |         4 |         6 |
| Sandbox peak working set              |  4.62 MiB |  3.85 MiB |
| Observed sandbox CPU lower bound      |   0.175 s |   0.163 s |

River uses approximately 50% less deployment memory here, but approximately 3.4% more CPU, 86.6%
more database queries and 13.1% more WAL. Elapsed time is approximately 1.2% longer; the fixed 65
seconds of external latency dominate this workload. These measurements do not establish a universal
CPU saving or close the release resource gate. Maximum observed sample gaps are 1.203 seconds for
Temporal and 1.442 seconds for River. Sandbox CPU is the lower bound described below; deployment
memory includes simultaneous sandbox observations.

The disposable infrastructure was reused after fixture preflight failures (missing required sandbox
grants, then an incorrect provider tool name). Those failed attempts are excluded from the six
successful attempts; existing Temporal history and PostgreSQL allocation remain included in the
recorded infrastructure snapshots. The fixture now uses the advertised tool name and returns a
JSON-RPC method-not-found response for unsupported discovery methods. No product permission check
was relaxed.

## Human response and deadline latency

The [October 6 human latency report](benchmarks/scheduler-human-latency-2026-10-06.json) is
complete. Each backend runs three one-node human deadline scenarios (`execution.timeout: 10s`) and
three 32-node human response scenarios. Responses use the production HTTP command endpoint, after
all requests are open and a two-second wait. All response scenarios export `true` and retain
`approved: true` for all 32 instances. Every scenario has zero physical model/tool calls and zero
root model/tool budget debits. Sampler error lists are empty.

| Measurement                                               |   Temporal |    River |
| --------------------------------------------------------- | ---------: | -------: |
| Median deadline-to-node-completion lag                    |   135.4 ms | 564.9 ms |
| Maximum deadline-to-node-completion lag across three runs |   155.5 ms | 604.4 ms |
| Median per-run answer-to-node-completion p50              | 3,169.7 ms |   2.0 ms |
| Median per-run answer-to-node-completion p95              | 5,866.4 ms |   3.4 ms |
| Maximum answer-to-node-completion lag across 96 answers   | 6,365.9 ms |   5.1 ms |
| Median duration of 32 response HTTP commands              |   111.1 ms | 289.9 ms |
| Median first-response-command-to-observed-run-completion  | 6,499.1 ms | 291.3 ms |

Node completion is its persisted public `finishedAt`, compared with the original request deadline or
durable request `accepted_at`; these lags exclude HTTP observation polling. The timeout check
requires `DEADLINE_EXCEEDED` and no completion before its deadline. For River, exactly one
`node_deadline` timer must be consumed after its due time; the median due-to-consumed lag is 519.4
ms. This distinguishes the one-second reconciliation poll from subsequent completion. The reported
p50/p95 values are medians of three per-run percentiles, not pooled percentiles. Three deadlines
demonstrate behavior in this sample, not a worst-case latency bound.

River completes accepted answers quickly here, but the sequential HTTP commands take longer and its
deadline poll adds latency relative to Temporal. These scenarios measure steady-process
response/deadline latency, not recovery after worker death or database failure. Infrastructure was
reused after the initial fixture assertion incorrectly read instance outputs outside their `values`
envelope; that partial run is excluded, while its Temporal history remains in storage snapshots.

To reproduce, add
`--workloads human_timeout human_resume --repeats 3 --wait-seconds 2 --idle-seconds 1` to the
command below. The original default workloads and human cancellation scenario remain unchanged.

## Human wait process recovery

The [October 6 recovery report](benchmarks/scheduler-human-recovery-2026-10-06.json) contains three
successful SIGKILL/restart scenarios per backend. Each scenario opens 32 human requests, waits two
seconds, kills the production service, waits for its confirmed `-SIGKILL` exit, and starts a new PID
with the same schema, profile and data directory. Engine identity, original instance IDs, request
IDs, open status and deadlines must remain unchanged. HTTP responses then complete all 32 original
instances with `approved: true` and export `true`. All six scenarios have zero physical model/tool
calls and zero root model/tool budget debits; sampler errors are empty.

| Median per recovery scenario                      |   Temporal |    River |
| ------------------------------------------------- | ---------: | -------: |
| SIGKILL-to-API-ready                              |   112.7 ms | 111.5 ms |
| First response command-to-observed-run-completion | 6,502.5 ms | 380.1 ms |
| Duration of 32 response commands                  |    83.4 ms | 378.5 ms |
| Per-run accepted-answer-to-node-completion p50    | 5,510.0 ms |   2.1 ms |
| Deployment peak memory                            |  252.6 MiB | 97.0 MiB |
| Deployment CPU lower bound                        |    2.895 s |  0.676 s |

API readiness is the first successful `/info` response with the original engine identity; it does
not prove all queue/workflow deliveries have recovered at that instant. Subsequent exact request and
instance comparisons plus successful answer completion establish recovery of these parked human
waits. The response-to-run-completion measurement includes HTTP polling. This is a single host,
available-database scenario; it does not measure recovery of active model/tool operations, pending
outcome publication, host loss or a database outage.

The sampler serializes native process snapshots with restart, rather than reporting a missing PID as
a sample error. It carries the last pre-SIGKILL CPU counter into the replacement process's counter.
CPU between that snapshot and termination is not observed, so `engine_cpu_is_lower_bound: true`
marks these records and their deployment CPU is a lower bound. Memory observations can miss the
brief startup interval; maximum sample gaps are 1.104 seconds for Temporal and 1.097 seconds for
River. These short scenarios are latency checks, not a stable capacity benchmark.

To reproduce, add `--workloads human_recovery --repeats 3 --wait-seconds 2 --idle-seconds 1` to the
command below. The production binary is unchanged from the preceding latency comparison.

## Queue retention and maintenance probe

The [October 6 queue-retention report](benchmarks/scheduler-queue-retention-2026-10-06.json) is
complete, with no sampler errors and the measured production binary digest. After a successful
64-node DAG, the River-only probe inserts 10,000 synthetic terminal jobs finalized eight days
earlier, split across completed/cancelled/discarded states. It also inserts six protected jobs:
fresh terminal jobs and available/pending/future-scheduled jobs. The normal River cleaner removes
all 10,000 expired jobs while preserving all six protected jobs. A canonical database fingerprint of
run projections, instances, attempts, budgets and events remains unchanged.

| Single observation                             |                       Value |
| ---------------------------------------------- | --------------------------: |
| Seed insertion time                            |                    102.3 ms |
| Seed-to-observed-cleanup time                  |                    27.737 s |
| Cleaner DELETE calls                           |                           2 |
| Cleaner SQL execution time, summed             |                   20.549 ms |
| Cleaner WAL                                    |   540,000 bytes (0.515 MiB) |
| Cleaner WAL records                            |                      10,000 |
| Deployment CPU including seed/wait/observation |                     4.813 s |
| Deployment sampled peak memory                 |                   205.0 MiB |
| Total observation WAL including seed           | 9,801,104 bytes (9.347 MiB) |
| PostgreSQL allocated storage growth            | 4,247,552 bytes (4.051 MiB) |

The production client uses River 0.48.0 defaults: 24-hour completed/cancelled retention, seven-day
discarded retention, and a 30-second cleaner interval. The 27.737 seconds includes waiting for the
next maintenance tick; it is not the DELETE execution duration. Statement deltas isolate the
cleaner's actual DELETE queries from fixture insertion, polling and other maintenance. Total CPU/WAL
include these other costs. Rows disappearing does not reclaim all allocated table/index/WAL storage;
physical growth remains recorded. This is one bounded synthetic-backlog observation, not a sustained
arrival-rate or production vacuum/retention capacity limit.

`TestRuntimePublishesFullDAGAndRedispatchesCapacityMiss` also uses the actual cleaner, rather than
manual deletion: expired two-day completed/cancelled jobs and eight-day discarded jobs are removed;
fresh completed/cancelled jobs and a two-day discarded job survive. Completed DAG outputs, attempts,
budgets and events remain identical before/after cleanup and after re-enqueueing the original
attempt/finalizer identities; physical model calls remain exactly three. This checks that queue
retention cannot become execution-history deletion or permit repeated work.

To reproduce the probe, use
`--backends river --profile-sql --workloads short_dag queue_retention --repeats 1 --idle-seconds 1`
with the disposable-container command below. A preceding completed workload is required so the
unchanged-domain check has real execution history. The original default benchmark workloads remain
unchanged.

## Reproduce

Use disposable containers: the script creates and drops application schemas, but Temporal history
remains in its container. Do not use a production database or an existing Temporal deployment. These
image digests match the measured run; PostgreSQL is 18.0 Debian, rather than the 18.6 Alpine image
in the current default Compose file. Temporal uses the same SQLite development server and resource
caps as Compose. This measures the existing local deployment, not a production Temporal cluster.

```sh
make build
docker run -d --name knotra-scheduler-benchmark-pg \
  --memory=512m --memory-swap=1g --pids-limit=128 \
  -p 127.0.0.1:25435:5432 \
  -e POSTGRES_USER=knotra -e POSTGRES_PASSWORD=knotra-test -e POSTGRES_DB=knotra \
  postgres@sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c \
  -c shared_preload_libraries=pg_stat_statements -c compute_query_id=on
docker run -d --name knotra-scheduler-benchmark-temporal \
  --memory=2g --pids-limit=512 -p 127.0.0.1:27236:7233 \
  temporalio/temporal:1.9.1@sha256:ad4c82c97bd12b417d1ea942610dbcd511afb250c4d5ed26c694009533df447e \
  server start-dev --ip 0.0.0.0 --ui-ip 0.0.0.0 \
  --ui-disable-news-fetch --db-filename /home/temporal/benchmark.db
```

Wait until `docker exec knotra-scheduler-benchmark-pg pg_isready -U knotra -d knotra` and
`docker exec knotra-scheduler-benchmark-temporal temporal operator cluster health --address 127.0.0.1:7233`
succeed, then run:

```sh
docker exec knotra-scheduler-benchmark-pg psql -XAt -v ON_ERROR_STOP=1 \
  -U knotra -d knotra -c 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements'
KNOTRA_TEST_DATABASE_URL='postgres://knotra:knotra-test@127.0.0.1:25435/knotra?sslmode=disable' \
  python3 scripts/benchmark_scheduler.py \
  --postgres-container knotra-scheduler-benchmark-pg \
  --temporal-container knotra-scheduler-benchmark-temporal \
  --temporal-address 127.0.0.1:27236 --repeats 3 \
  --idle-seconds 15 --wait-seconds 60 \
  --output .knotra/benchmarks/scheduler-comparison.json
```

A successful report has `complete: true`, both backends, all eight measurements per backend, and
empty `sample_errors` lists. Partial reports are saved after each measurement and remain marked
incomplete on failure. The reported work directory retains the profile and engine logs. Inspect them
before removing the disposable containers:

```sh
docker rm -f knotra-scheduler-benchmark-pg knotra-scheduler-benchmark-temporal
```

## Metric definitions and limits

- Elapsed time starts before definition upload/compilation and ends when HTTP observes the required
  terminal status. Admission-to-terminal time starts before the run creation request. Budget
  validation and the final resource snapshot are excluded from these times.
- Admission latency is the run creation request round trip. First physical call latency includes
  admission and scheduling; it is not a pure queue delivery metric. Throughput counts all graph
  nodes divided by elapsed time, and is omitted for idle and human waits.
- Deployment memory is the maximum sampled sum of native engine RSS and the working sets of the
  required containers: PostgreSQL plus Temporal for Temporal, PostgreSQL for River. It does not sum
  independently observed component peaks. It excludes the fixture, Docker VM overhead and other
  local services. These are mixed native/container accounting values, not whole-machine RAM.
- Deployment CPU sums the engine process and required containers' CPU counter deltas. PostgreSQL
  includes the observer's queries; resource deltas also include post-run budget validation and
  snapshot collection. The idle Temporal container remains running during River's measurements but
  is excluded from River's required deployment components. Other test containers remain idle; the
  host is not a dedicated benchmark machine.
- When `long_agent` is requested, the sampler also inventories running sandboxes by the owned engine
  label and adds their simultaneous working sets to deployment memory. It records the last observed
  CPU counter for each transient sandbox as `sandbox_cpu_seconds_lower_bound`; deployment CPU
  includes this component. Container deletion can hide the final partial sample interval, so this
  CPU contribution is a lower bound. The report retains observed sandbox counts and separate sandbox
  peaks; it does not silently count unrelated containers. Inventory/stats queries add observer
  overhead. Long-agent reports also retain the actual sandbox image ID and trusted helper digest.
- Database connections count the engine's application name, including River's notification
  connection. Query calls, SQL execution time, transactions and WAL are database-wide deltas and
  include observation queries and background maintenance. PostgreSQL statistics can flush late; SQL
  execution time can exceed wall time when statements overlap or wait for locks. It is not a CPU
  metric.
- Schema growth includes tables, indexes and TOAST storage. Filesystem storage is cumulative engine
  data directory content, excluding the profile and log. Temporal's SQLite history file,
  PostgreSQL's other physical files and Docker volumes are not included in that filesystem field.
- New reports also record allocated storage before/after each observation for the complete
  PostgreSQL data directory and, for Temporal, `/home/temporal` containing its development history.
  Paths are recorded in `infrastructure_storage_paths`; `du -sk` reports allocated KiB converted to
  bytes. This includes PostgreSQL WAL/preallocation and Temporal SQLite files. Net growth can be
  negative after cleanup or reuse and is not a write-I/O counter. These snapshots happen only at
  observation boundaries, rather than on every resource sample. Archived reports produced before
  this addition lack these fields. Both backends reuse the PostgreSQL container: dropping the
  Temporal application schema does not reset its WAL allocation or caches before River starts.
  Therefore compare the recorded directory snapshots together with logical schema and WAL deltas; a
  reused allocation or zero net growth does not prove that a backend writes no data.
- Startup migrations happen before the idle sample. Sampling can miss short memory spikes; the
  report records sample count and maximum gap. The process is reused for all workloads, so a human
  wait after a large graph retains that process's heap and database cache.

## Acceptance still required

This is one host comparison, not completion of the resource release gate. Nested foreach tuning,
long agent attempts, timer lateness, scheduler transaction duration, recovery latency, queue
retention cost and representative history/volume growth across those workloads still need
measurements. Numerical scheduling and resource budgets must be based on the observations. A memory
improvement does not excuse unexplained WAL, query or admission regressions, and these measurements
do not replace the crash or client compatibility matrix.
