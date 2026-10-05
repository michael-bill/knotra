# Knotra Vision

Status: product direction verified with implementation on October 5, 2026. Current capabilities and
further directions are described in the [roadmap](roadmap.md).

## Purpose

Knotra lets developers describe a complete AI workflow, execute it under their control and inspect
how its result was produced.

The user defines nodes, their inputs and outputs, dependencies, models, instructions, tools,
environments, and execution rules. The engine accepts the description, validates it, and executes
the process. Desktop provides an editor and monitoring, CLI — access for humans and automation; both
use a common HTTP API.

Example process: obtain a research topic, gather materials via MCP, analyze them using a model and
Python, create a document, and pass it to a human for review. The acceptance scenario in
`examples/integration` checks CSV analysis, agent code, MCP, review, and the final report. Domain
datasets for quality assessment remain a development direction.

## Current Stage Boundaries

The current stage includes:

- Pipeline notation and portable package format.
- Engine and node executors.
- Connections to models and MCP.
- Isolated environments for commands and files.
- Storage of state, events, and results.
- HTTP API, CLI, and desktop client.

Desktop on Tauri/React is implemented as an API client. Local drafts and client logs are separated
from the engine's authoritative execution state.

The first deployment is oriented for use under your control on a single machine: user computer or
server. A public service for independent users will require separate development of isolation,
quotas, access, and operations.

## Main Execution Principle

The pipeline defines the process structure: which actions must be performed, how data is passed, and
under what conditions branches are selected.

The agent receives autonomy within its node. It can choose allowed tools, write and execute
programs, read materials, and repeat actions until a result is obtained or a limit is reached.

The agent does not arbitrarily change the pipeline structure. Process expansion is expressed through
controlled constructs: collection processing, loops, and subpipelines. All three constructs are
supported in v1.

## Product Principles

### Explicit Contracts

Pipelines and nodes have inputs and outputs. Structured data is validated against a schema. Files
are passed as artifacts with identifiers and metadata.

Connections reflect data transfer and execution dependencies. The user must understand why a node
started and where it received its input from.

### Pre-Execution Validation

The engine checks format, connections, required parameters, connection availability, and necessary
model capabilities. Some checks are performed by CLI without connecting to the engine.

Static validation does not guarantee future availability of an external service or model response
quality. Execution results are checked separately.

### Fixed Run Description

Each run uses an immutable version of the package and allowed settings. Editing creates a new
version and does not change an already started process.

Freezing inputs and configuration makes the result's provenance explainable. It does not guarantee
verbatim matching of new model responses or immutability of external services.

### Observable Execution

Desktop and CLI show node instance states, current attempts, events, errors, run inputs and outputs,
human requests, and artifacts. The real run is represented as a graph with selection of nested
instances, step inspector, and timeline.

The inspector shows node inputs and outputs, attempts, tool calls, available model messages,
duration, and token count when the provider returns them. Rewind reads saved states; comparison
matches instances across runs. Cost in money and domain-specific quality assessment are not yet
calculated.

History relates to specific runs, node instances, and attempts. It is available after reconnecting
the client.

### Controlled Autonomy

Tools, secrets, network, and resources are issued explicitly. Engine constraints set the upper bound
of pipeline rights. Limits can act on a single node and on the entire run.

A human can respond to requests, check results, and confirm actions according to pipeline policy.
Waiting for a human is saved execution state.

### Recovery with Clear Guarantees

Continuing the process, retrying an attempt, and relaunching from a selected point are different
operations. Completed work is accounted for during recovery, while actions with uncertain outcomes
require special handling.

Cancellation stops further work as the request is processed. It does not roll back external actions
already performed.

### Portability

The pipeline is delivered as a package with the main YAML and related files. Deployment
configuration binds logical connections and secrets to a specific engine.

Local and remote clients use the same API model. Moving a process does not mean automatic transfer
of environment variables, files, or user computer credentials.

## Main Entities

| Entity           | Meaning                                                                |
| ---------------- | ---------------------------------------------------------------------- |
| Pipeline package | YAML, prompts, schemas, scripts and other declared materials           |
| Pipeline version | Fixed content of the package                                           |
| Run              | Execution of a version with specific inputs and settings               |
| Node definition  | Operation described in the pipeline                                    |
| Node instance    | Specific operation in a run, including collection element or iteration |
| Attempt          | Separate execution attempt of an instance                              |
| Connection       | Access settings to a model, MCP or another external service            |
| Environment      | Isolated working environment for files and commands                    |
| Artifact         | Saved file or set of files with provenance and metadata                |
| Event            | Record of execution progress or user action                            |

## Utility Criterion for First Version

The user must be able to describe a small real-world process, verify it, run it via CLI or desktop,
observe its execution, respond to a human request, and obtain final files.

After restarting the engine, the user must see saved history and correct run state. In case of an
error, it must be clear which node and which operation did not complete, what can be repeated, and
where the operation outcome is unknown.

First version guarantee boundaries and work sequence are described in
[development stages](roadmap.md).
