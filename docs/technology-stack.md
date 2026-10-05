# Technology Stack

Verified against code on October 5, 2026. Versions are locked in `go.mod`, `go.sum`, `app/bun.lock`,
`app/src-tauri/Cargo.lock` and `compose.yaml`. Practical setup is described in
[startup guide](running.md) and [desktop README](../app/README.md).

| Area                        | Implementation                                     | Purpose                                                   |
| --------------------------- | -------------------------------------------------- | --------------------------------------------------------- |
| Engine, API, CLI and helper | Go; Cobra for CLI                                  | Network operations, execution management, commands        |
| YAML                        | `go.yaml.in/yaml/v3` and own strict checks         | Limited YAML 1.2 profile with precise diagnostics         |
| JSON Schema                 | `santhosh-tekuri/jsonschema/v6`, Draft 2020-12     | Document structure, inputs and outputs                    |
| Expressions                 | `google/cel-go`                                    | Limited computations without external I/O                 |
| Orchestration               | Temporal Go SDK                                    | Workflow, Activities, history, waits and recovery         |
| API                         | HTTP/JSON, OpenAPI and SSE                         | General boundary for CLI, desktop and browser preview     |
| Engine data                 | PostgreSQL + `pgx/v5`                              | Migrations, operations, queries, projections and metadata |
| Models                      | HTTP adapters Ollama, OpenAI, Anthropic            | Capability check and agent loop                           |
| MCP                         | Official `modelcontextprotocol/go-sdk`             | Streamable HTTP and stdio in sandbox                      |
| Sandbox                     | Docker Engine API via Unix socket; Linux Go helper | Files, processes, limits, network and watchdog            |
| Artifacts and payload       | Local files + PostgreSQL metadata                  | Bytes outside Temporal history                            |
| Diagnostics                 | JSON slog + OpenTelemetry OTLP/HTTP                | Traces, HTTP counter and request duration                 |
| Interface                   | React, TypeScript, Vite                            | Editor, graph, runs, review and artifacts                 |
| Editor and graph            | CodeMirror, React Flow + Dagre                     | YAML and visual connections                               |
| Native desktop              | Tauri 2, Rust, reqwest, rusqlite                   | HTTP/SSE, SQLite, file checks and system dialogs          |
| Frontend tools              | Bun, Vitest, Playwright, Prettier                  | Dependencies, checks and formatting                       |
| Development infrastructure  | Docker Compose                                     | PostgreSQL and Temporal development server                |

## Component Boundaries

User code runs in the sandbox image. Python and other node dependencies are installed into this
image, not into the Go engine process. The Docker Go SDK is not used: the Engine API client is
implemented in `internal/adapters/docker.go`. The Docker CLI is used to determine context and
prepare images.

The YAML library provides a tree with source positions. Knotra additionally rejects aliases,
anchors, duplicate keys, forbidden tags and numeric forms. JSON Schema is supplemented by a semantic
compiler; one library parse is not a contract check. Rules are specified in
[notation](notation/validation.md).

Workflow manages the graph deterministically. External operations execute Activities. Temporal
stores history, PostgreSQL stores accepted commands and projections, and the file catalog stores
artifacts and large payloads. All three layers are needed for recovery. A workflow snapshot does not
automatically restore the agent's file system. Details — in [architecture](architecture.md) and
[Temporal](temporal.md).

Ollama native chat, OpenAI Responses and Anthropic Messages are implemented. Their request formats,
parameters and streaming responses are handled separately; the agent loop and operation log are
shared. Cloud catalogs confirm model ID availability but not weight immutability. S3 remains a
development direction. Details — in [adapters](../internal/adapters/README.md).

Desktop saves drafts and client receipts in SQLite; browser preview — in localStorage. Authoritative
execution always resides in the Go engine. Demo executes only prepared local data.
[API](api/desktop-v1.md) fixes the same protocol for clients.

## Delivery and Verification

API and worker currently run in one process on one host. Compose runs development infrastructure
separately; it is not a production recipe. Remote access requires HTTPS and bearer token. Separate
access, storage and enhanced isolation solutions are needed for independent users.

Go is checked by `make check`, frontend — Vitest, TypeScript build, Prettier and Playwright, native
code — `cargo test --locked` and rustfmt. Python scripts use Black; auxiliary fixture checks have
their own requirements file. Exact commands and real scenarios are provided in
[CONTRIBUTING](../CONTRIBUTING.md) and [verification report](verification.md).
