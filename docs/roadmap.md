# Development Stages

Status as of October 5, 2026. Implemented capabilities are separated from further directions;
schedules for new features are not assigned.

## Implemented

- Contract YAML v1, JSON Schema, strict parsing, CEL, secure package files and semantic compiler.
  All 39 fixtures are verified by Go tests.
- All nine node types: `llm`, `agent`, `code`, `tool`, `switch`, `human`, `foreach`, `loop`,
  `pipeline`; nested graphs, permissions and budgets.
- Temporal workflow, PostgreSQL, immutable definitions and plans, durable timers, large payloads
  outside history and Continue-As-New.
- Ollama, OpenAI Responses, Anthropic Messages, custom agent loop, Docker sandbox, MCP Streamable
  HTTP and isolated stdio, explicit secret grants and network restrictions.
- HTTP API and CLI: admission, run submission, SSE, human requests, cancellation, resolution of
  unknown outcome, receipts and artifacts.
- Desktop on Tauri/React: YAML and graph editing, package import/export, connection to Go engine,
  runs, review and artifacts. Drafts and logs are saved in SQLite; browser preview uses
  localStorage.
- Quickstart with persistent infrastructure, doctor and CLI archives with both Linux helper
  architectures.
- Five domain scenarios; research with code verification, approval and publication after restart.
- Live run graph: nested instances, attempts, tools, response stream, rewind and comparison.
- Checks with real Ollama, Docker, MCP, Temporal and PostgreSQL, including parallel runs and
  recovery after SIGKILL.

Configuration is provided in [running guide](running.md), checks — in [report](verification.md),
ready commands — in [CLI reference](../internal/cli/README.md). Acceptance scenario is located in
[`examples/integration`](../examples/integration/README.md).

Product verification plan on five developers is in [pilot](pilot.md). Actual pilot and paid cloud
API calls require participants and keys; automatic checks do not replace them.

## Next Directions

| Direction                           | What is required                                                       |
| ----------------------------------- | ---------------------------------------------------------------------- |
| Shared artifacts and multiple hosts | Object storage, shared payloads and worker ownership model             |
| Agent recovery within attempt       | Persisted conversation, tool results and workspace snapshot            |
| Long-running runs on update         | Workflow versioning and compatibility check of saved histories         |
| New run from selected place         | Explicit reuse of results without changing old run                     |
| Operations                          | Event/file retention, backup and storage quotas                        |
| Multiple users                      | Authentication, RBAC, quotas and enhanced isolation                    |
| Desktop delivery                    | Signed builds, notarization and check of each supported OS             |
| Quality assessment                  | Domain examples, model/prompt comparison, quality and cost measurement |

Local mode without Temporal requires a separate architectural solution. Workflow recovery does not
promise recovery of a live container or continuation of the program with an arbitrary instruction.
Cancellation does not roll back external effects. Current guarantees are described in
[execution semantics](notation/execution.md).

## Criteria for Extensions

Contract change updates schema, diagnostics, fixtures, runtime and documentation together. New
adapter checks permissions before external action. Storage change preserves existing data and
history. Execution after failure, idempotency and unknown outcome are checked separately from
successful scenario.

Default checks do not require model weights or paid requests. Real integration is enabled explicitly
documented with exact command. [Participation rules](../CONTRIBUTING.md) describe the change
process.
