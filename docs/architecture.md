# Knotra Architecture

Status: architectural boundaries verified against the implementation on October 4, 2026. The engine
and CLI implementation is described in the [run guide](running.md),
[HTTP API contract](api/desktop-v1.md) and [Temporal review](temporal.md). Below are preserved
architectural principles, including directions for subsequent development.

## Components

| Component                | Responsibility                                                                         |
| ------------------------ | -------------------------------------------------------------------------------------- |
| Desktop                  | Editing YAML/graph, local drafts, runs, review and artifacts via API                   |
| CLI                      | Package validation, run submission, monitoring, management and result retrieval        |
| Engine API               | Receiving packages and commands, access to definitions, runs, events and artifacts     |
| Plan Validation and Prep | Parsing YAML, validating schemas and dependencies, resolving settings, fixing the plan |
| Knotra Orchestration     | Graph semantics, branching, limits, inputs and outputs                                 |
| Temporal                 | Persistent workflow history, operation scheduling, waits and recovery                  |
| Knotra Executors         | Invoking models and tools, working with environments and results                       |
| Agent runtime            | Model-tool interaction cycle, context and completion of the node                       |
| Connection Adapters      | Provider API differences, MCP and access configuration                                 |
| Environment Management   | Creation, usage, stopping and cleaning sandbox                                         |
| Stores                   | Packages, metadata, events, checkpoints and artifacts                                  |

These are logical modules of a single project. The API and worker are delivered as a single Go
process on a single host; PostgreSQL enforces sole ownership of the engine ID. CLI and desktop are
separate clients. Scaling the worker to multiple hosts requires shared file and payload storage and
remains a direction for development.

Desktop preserves drafts, receipts and SSE-cursors in SQLite; browser preview uses localStorage.
These data do not replace Temporal history or the engine database.

## Path from YAML to Execution

1. CLI or desktop reads the package and performs available local validations.
2. API receives the package and run parameters. All necessary related files are passed explicitly.
3. The engine re-validates the package, permissions, connections and executor capabilities.
4. An immutable internal plan is formed: package version, inputs, allowed settings, links to
   connections and limits.
5. A Temporal workflow is started, executing this plan according to Knotra rules.
6. Executors perform operations and save results and events.
7. CLI or desktop reads state and event stream via API. Client disconnection does not cancel the
   run.

Secret values are not included in the user package or workflow history. The plan stores links to
secrets. Their resolution occurs in a trusted component before use.

## Separation of Knotra and Temporal

Knotra defines what each node means, what data it needs and what constitutes completion. Temporal
provides the mechanism for reliable execution of this logic.

Workflow executes deterministic control logic. Model calls, MCP, file and container work are
performed in Activities. Replaying saved history should not by itself trigger external actions.

YAML lacks Temporal entities mandatory for the user. The plan must contain sufficient data to
execute the fixed version without reading mutable drafts.

Agent workflow and Activities detailing must account for history cost, recovery points and side
effects. One long-running agent node cannot be considered automatically recoverable just because it
is started as an Activity.

Code changes in executors and plan format must account for existing runs. Updating long-lived
processes requires a worker and replay compatibility policy for saved histories; the current
delivery does not promise automatic workflow migration between incompatible versions.

## Execution Graph

The foundation is a dependency graph without arbitrary feedback loops. Repetition is expressed by a
separate cycle construct with explicit conditions and limits.

Independent nodes can be executed in parallel. Parallelism is limited by engine resources, run
policy and provider limits.

To merge branches, it must be explicitly defined which results are expected and how skipped or
error-completed branches are accounted for. The first version supports a limited, documented set of
rules; there is no implicit behavior selection.

In the first version, data is passed after node completion. Model adapters stream visible fragments
through observation events and journal the complete response before publishing outputs. Downstream
nodes receive validated, complete values; visible streaming does not pass partial outputs along
graph edges.

## Agent runtime

The agent runtime is a Knotra module. It:

- Gathers instructions, input data and available tools.
- Contacts the selected model adapter.
- Checks permissions for each tool request.
- Executes the allowed call and returns the model result.
- Manages context and limits.
- Passes validated results to the graph; human wait executes a separate `human`-node.
- Checks final outputs and fixes completion.

Responses to external operations are stored in the internal log with binding to the run, instance,
and attempt for recovery. The client API provides states, events, diagnostics, inputs and outputs of
runs, human requests, and artifacts; bounded prompt, model and tool observations are available
through per-instance history. Truncation and missing observations are explicit; provider-private
reasoning stays internal. Internal model reasoning, not provided by the provider, is not a
requirement for history.

Success of the node is determined by checking the result contract. Checking JSON structure or file
existence does not replace checking substantive quality. The latter can be performed by code,
another agent, or a human.

Automatic context truncation is not yet implemented; the agent is limited by explicit limits.
Transfer of one agent's entire history to another is not default behavior.

## Data and Artifacts

Structured data is validated against JSON Schema. Between nodes, serializable values and references
to artifacts are transferred.

An artifact contains an identifier, type, size, checksum, and origin information. The local sandbox
path is not a portable file identifier.

The input artifact is placed in the recipient's environment via explicit binding. Results of
completed nodes are immutable; correction creates a new version of the result.

Saving outputs and completion marking must be approved: a node cannot be declared successful if its
published outputs are unavailable. For operations between multiple storage systems, repeatable
publication and recovery from partial failure are required.

The first version uses file storage when executors are deployed on one machine. Transition to
executors on different machines requires shared storage, for example via an S3 adapter.

## State Storage

| Data                                                | Storage Location                                                      |
| --------------------------------------------------- | --------------------------------------------------------------------- |
| History and authoritative workflow state            | Temporal                                                              |
| Knotra definitions, versions, and metadata          | PostgreSQL Knotra                                                     |
| State views for search and output                   | PostgreSQL Knotra, synchronized with Temporal                         |
| Detailed events, usage accounting, and result links | PostgreSQL Knotra                                                     |
| Files, large results, and saved materials           | Artifact Storage                                                      |
| Secret values                                       | Environment of the trusted component; later separate secret providers |

Temporal uses its own database, separated from the application database. At the first stage,
databases may reside on one PostgreSQL server.

The state view in Knotra does not create a second independent orchestrator. Possible lag from
Temporal must be accounted for by the API and reconciliation mechanism.

Management history, detailed logs, and streaming response fragments have different storage
requirements. Large files and every text fragment do not fit into Temporal history without
necessity.

## Errors and Recovery

Three operations are distinguished:

| Operation                   | Meaning                                                                     |
| --------------------------- | --------------------------------------------------------------------------- |
| Continue                    | Resume the same process from the saved state                                |
| Retry                       | Re-execute a failed operation according to specified rules                  |
| New run from selected point | Create a separate execution with explicit reuse of saved inputs and results |

The working model of states includes waiting for dependencies, queue, execution, human wait,
successful completion, error, skip, and cancellation. Exact names and allowed transitions are fixed
in the execution specification.

Automatic retry depends on error type and action nature. Temporary read errors, incorrect model
result, and uncertain outcome of an external record require different rules.

For operations that change external state, idempotency keys are used if the service supports them,
and result validation via a stable identifier. If it is impossible to determine whether an action
was performed, this is explicitly reflected in the state. The general guarantee "external action
will be executed exactly once" is not provided.

The first level of recovery saves completed nodes and process state. An unfinished operation may
require retry or verification of its outcome.

Continuing an agent mid-node requires coordinated saving of messages, tool results, and files. A
workflow checkpoint is not a snapshot of the sandbox file system. The recovery level within a node
is determined separately and checked for failures.

A cancellation request stops scheduling new work and is passed to active executors. Already
performed external actions are not rolled back automatically. Compensating actions, if needed, are
described separately.

## Isolated Environments

Agent commands and code nodes execute in a sandbox. The trusted runtime manages the model and tools
from outside; the model key does not need to be placed in the agent's file environment.

One active node instance preserves the working environment between its actions. Different nodes
receive separate working environments and exchange declared data and artifacts.

The initial implementation uses the Docker Engine API. The environment has a specified image,
working directory, resource limits, and network policy. Access to host files and the managing Docker
API is not passed to the agent by default.

The image and its version are fixed when preparing for run. Installing additional dependencies via
the agent requires permission and affects environment reproducibility.

On a retry attempt, a new environment is created or an explicitly saved state is restored according
to node rules. Implicit use of files from failed attempts is excluded.

Declared results are retained until the environment is deleted. For diagnostics, a snapshot of files
from a failed attempt may be retained for a limited retention period. Prolonged human waiting should
not require indefinite holding of a running container; the method for releasing and restoring the
environment must be defined before such support is implemented.

Docker is chosen for the first version under its own management. For independent untrusted users, a
separate profile with enhanced isolation is required; candidates are gVisor, microVM, or an external
sandbox provider.

## MCP and Permissions

The following are separated:

1. Description of the MCP connection.
2. General tool permissions for the pipeline.
3. Additional permissions for a specific node.

The mere presence of a connection in a catalog does not grant access to all its tools. Final access
is limited by engine policy and external service credential rights.

Permissions are checked on every call within a trusted component. MCP tool information about its own
behavior does not replace Knotra access policy.

Remote servers are supported via Streamable HTTP and spawned processes via stdio. For a process,
dependencies, environment, and lifecycle scope are defined. General connection configuration does
not imply a shared mutable session for all nodes.

Schemas of discovered tools are considered when preparing for run. Changes in remote server
capabilities require explicit discovery and handling; fixing the schema does not fix external
service behavior.

## Secrets and Engine Access

The pipeline contains logical references to secrets. Values are obtained by a trusted component from
the engine environment or executor. A secret is passed only to the adapter or process that needs it.

Passing a secret into sandbox is a separate permission. Not all engine environment variables are
inherited. Secrets are not automatically substituted in prompts and are not output in diagnostics as
plain values.

A remote engine uses its own connections and secrets. Passing a package does not automatically load
client computer environment variables.

The local API by default listens on loopback. Remote deployment requires authentication and secure
transport; TLS and bearer token are implemented for one trusted principal. RBAC for independent
users is not yet available.

## API and CLI

CLI uses the HTTP/JSON API. The API contract is described via OpenAPI. Events are transmitted via
SSE with identifiers for resuming reading after reconnection.

Stored events allow restoring the execution picture; an active SSE connection is not a storage or
lifespan condition for a run. Processing of redelivered events must not create duplicates in the
client view.

CLI provides human-readable output and machine-readable JSON format. Package validation is available
locally; authoritative validation of permissions, connections, and available capabilities is
performed by the engine.

Human responses are addressed to a specific saved request. Resending a command must not accidentally
apply a response to another request. Identifiers and rules for resending are specified in
[API](api/desktop-v1.md).

## Observability

Events and logs are linked by identifiers of run, node, instance, attempt, and operation.
OpenTelemetry is used for technical tracing and metrics.

Runtime limits outgoing model/tool calls and the number of agent steps. Unhandled model responses
are stored in the operations journal. Client observations include token counts, call durations and
first-token timing when supplied. Monetary estimates and a pricing catalog are not implemented. A
profile sets limits on time, parallelism, instances, model/tool calls, and environment sizes. Real
exported OTLP metrics are described in the [run guide](running.md).

Retention periods, log volume, and access to sensitive inputs and outputs are defined by engine
policy. Prompts and tool results may contain user data even in the absence of secret tokens.
