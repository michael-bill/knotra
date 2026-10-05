# Knotra Notation

Status: implemented contract v1, verified on October 5, 2026. The compiler, executor, and adapters
are located in `internal/contract`, `internal/engine`, and `internal/adapters`. Run is described in
[guide](running.md).

The contract specifies exact YAML fields, data types, references, states, and observable behavior.
Contract fixtures and tests verify these rules.

| Document                                                | What it defines                                                       |
| ------------------------------------------------------- | --------------------------------------------------------------------- |
| [Contract v1](notation/v1.md)                           | Pipeline, ports, Binding, scopes, and nine types of node              |
| [Execution Semantics](notation/execution.md)            | Ready, skip/null/error, loops, ABI, retry, cancellation, and recovery |
| [EngineProfile](notation/engine-profile.md)             | Models, MCP, secrets, sandbox, capabilities, and permissions          |
| [Validation and Package](notation/validation.md)        | YAML/CEL/DataSchema, files, limits, and diagnostics                   |
| [JSON Schema](../schemas/knotra-v1.schema.json)         | Structural validation of Pipeline and EngineProfile                   |
| [Contract fixtures](../contracts/v1/fixtures/README.md) | 39 positive and negative cases                                        |
| [Client APIs](api/desktop-v1.md)                        | Package reception, commands, events, human requests, and artifacts    |

Supported: `llm`, `agent`, `code`, `tool`, `switch`, `human`, `foreach`, `loop`, and `pipeline`.
Portable process description is separated from engine trusted configuration; JSON values are
separated from file artifacts.

JSON Schema validates structure. The semantic compiler additionally validates references, DAG, CEL,
and permissions; admission validates actual resources. Execution tests validate states and recovery.
Python fixture checks cover only parsing and structure; the Go suite also validates semantics.

Implemented model adapters are Ollama, OpenAI Responses and Anthropic Messages. Unknown providers
are rejected at admission. Real scenarios and check limits are described in
[report](verification.md), further directions in [roadmap](roadmap.md).
