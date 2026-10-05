# Knotra v1: connections, environments and permissions

Implementation is located in `internal/contract`, `internal/engine` and `internal/adapters`.
Configuration is described in [startup guide](../running.md), verified scenarios and support
boundaries — in [report](../verification.md).

Status: normative contract v1, verified against implementation on October 5, 2026. Structural JSON
Schema and textual semantics are applied jointly. Requirements for external systems are checked
during admission; unsupported providers are rejected before startup.

This document defines `kind: EngineProfile`, resource permissions in `Pipeline` and restrictions on
their usage. [Main contract](v1.md) defines the graph and nodes, [checks](validation.md) —
validation stages, [JSON Schema](../../schemas/knotra-v1.schema.json) — structure of both documents.
Semantic requirements of this document apply additionally to JSON Schema.

## 1. Configuration Boundary

`Pipeline` — portable process package. `EngineProfile` — trusted configuration of a specific engine,
installed by its operator. The pipeline package cannot load or modify the profile, even if it
contains YAML with `kind: EngineProfile`.

The engine selects one profile for startup before plan preparation. Profile installation method,
profile selection via API, PostgreSQL and Temporal addresses, Docker access and server settings
themselves belong to deployment configuration and are not fields of this format. `metadata.name`
identifies the profile; `metadata.version` serves as a user label, not a trust or immutability
mechanism.

| Portable `Pipeline.spec`                                | Trusted `EngineProfile.spec`                                      |
| ------------------------------------------------------- | ----------------------------------------------------------------- |
| `models.<alias>.connection` and optional model settings | Real provider, default model, API address and authorization       |
| `mcp.<alias>.connection`, session scope                 | Transport, URL or MCP command, credentials, list of allowed tools |
| `sandboxes.<alias>.profile`                             | Image, resources, network and environment permissions             |
| `secrets.<alias>.ref`                                   | Secret value source in the engine environment                     |
| Requested node limits and permissions                   | Maximum allowable limits and capabilities                         |

Names on the right are **canonical names** within the selected EngineProfile. Names on the left are
**local aliases** of a specific Pipeline. Identical spelling is allowed but does not change the
scope of reference resolution. For example, `spec.models.writer.connection: production_model` links
alias `writer` with canonical connection `production_model`.

Declaring an alias does not grant the agent a tool and does not pass it a secret value. During plan
preparation, the engine resolves all declared references and checks their validity, including in
conditional branches and nested pipelines that may not execute.

Unknown fields are forbidden. Resource section identifiers correspond to `[a-z][a-z0-9_]{0,63}`.
Permission lists contain exact names without wildcard; repetitions are forbidden.

## 2. EngineProfile Root

| Field            | Requiredness | Value                                                |
| ---------------- | ------------ | ---------------------------------------------------- |
| `apiVersion`     | Required     | Strictly `knotra/v1`                                 |
| `kind`           | Required     | Strictly `EngineProfile`                             |
| `metadata`       | Required     | General metadata, as in Pipeline; `name` is required |
| `spec`           | Required     | Profile configuration                                |
| `spec.secrets`   | Optional     | Map of secret sources; default `{}`                  |
| `spec.models`    | Optional     | Map of model connections; default `{}`               |
| `spec.mcp`       | Optional     | Map of MCP connections; default `{}`                 |
| `spec.sandboxes` | Optional     | Map of sandbox profiles; default `{}`                |
| `spec.limits`    | Required     | All five upper limits from section 8                 |

Missing optional sections are not filled with discovered machine connections. An empty catalog does
not provide the corresponding capability. Fields with explicit `null` do not replace missing fields
and are rejected by the schema.

## 3. Secrets and Environment Variables

### 3.1. Source in Profile

Each `spec.secrets.<name>` contains a single required field `env`: environment variable name
following template `[A-Za-z_][A-Za-z0-9_]*`.

`env` is read in the trusted engine component performing the connection or creating sandbox. This is
the Knotra server/executor environment. The environment of the CLI computer that started it is not a
source, is not passed automatically and does not participate in substitution.

Missing or empty value of the used secret causes a connection availability error before startup of
the corresponding external operation. Deployment must ensure a common source for executors allowed
to perform such an operation. Local offline Pipeline check can verify only the link, not the
presence of the secret value on the server.

Profile and plan store the source name, not its value. The engine resolves the secret immediately
before use. Variable rotation affects subsequent usages, including in a resumed run; changing the
source name itself in the profile does not change the fixed plan of an already accepted run. History
may contain the secret name and access fact, but not the value.

### 3.2. Links in trusted connections

Only the object `{secretRef: <canonical-secret-name>}` is allowed in
`models.<connection>.auth.<field>`. The provider adapter defines `<field>`.

MCP `headers` and `env` use one of two mutually exclusive objects:

| Object                | Meaning                                          |
| --------------------- | ------------------------------------------------ |
| `{value: "..."}`      | Literal string for non-secret value              |
| `{secretRef: <name>}` | Full value of the EngineProfile canonical secret |

Prefixes, concatenation, CEL, `${...}`, and templating are not performed in these objects. If an
HTTP header needs `Bearer …`, the secret source contains the entire header string. Nonempty field
names, CR/LF restrictions and other transport requirements are checked after value resolution; the
secret is not included in the error text.

Calling a model or MCP allows the trusted adapter to use this connection's credentials. It does not
grant the pipeline itself the right to read or export these credentials. Therefore, the model
authorization secret does not need to be duplicated in `Pipeline.spec.secrets` or
`pipeline.permissions.secrets`.

### 3.3. Explicit secret passing to sandbox

`Pipeline.spec.secrets.<alias>` contains a single field `ref`, which specifies the canonical
EngineProfile name. Fields `env` in nodes `agent` and `code` set the environment variable map. For
each variable, exactly one option is allowed:

| Object                              | Meaning                                             |
| ----------------------------------- | --------------------------------------------------- |
| `{value: "..."}`                    | Literal string                                      |
| `{secret: <pipeline-secret-alias>}` | Value of the declared alias secret of this Pipeline |

Missing `env` means `{}`. Values cannot be expressions or data bindings. For passing input data to a
program, node contracts described in the main document are used.

For `{secret: token}`, the following conditions must simultaneously be met:

1. Alias `token` is declared in the current `Pipeline.spec.secrets`.
2. Its `ref` exists in EngineProfile.
3. The canonical secret name is included in `allowedSecrets` of the selected sandbox profile.
4. When calling a nested Pipeline, the canonical secret name is included in all active
   `pipeline.permissions.secrets` of its ancestors.

Plan preparation rejects an unresolved secret. The engine must not remove a forbidden variable and
continue with another program meaning.

Sandbox does not inherit environment variables from CLI, API server, or worker process. The base
environment is formed from the fixed image and runtime service values; then explicitly set variables
are applied. Reserved execution interface variables defined by the main contract cannot be
overridden by user `env`.

Issuing a secret to sandbox means that the program inside can read it. The engine must exclude its
own credentials from automatic output, history, and plan; an arbitrary program with an explicitly
issued secret may include it in its result. A separate mechanism for automatically proving the
absence of secrets in user data is not promised by the contract.

## 4. Model connections

### 4.1. EngineProfile.spec.models

| Connection field | Requiredness / default value | Semantics                                                       |
| ---------------- | ---------------------------- | --------------------------------------------------------------- |
| `provider`       | Required                     | Identifier of the installed adapter                             |
| `model`          | Required                     | Non-empty default model identifier                              |
| `baseUrl`        | Optional                     | Base API URL; if absent, the documented adapter address is used |
| `auth`           | Default `{}`                 | Authorization fields map; each value is `{secretRef: ...}`      |
| `parameters`     | Default `{}`                 | Default provider parameter values, representable in JSON        |

`baseUrl`, if specified, must be an absolute HTTP(S) URL without userinfo and fragment. Its
normalization, required path suffix, and HTTP allowance are determined by the adapter. Network calls
are not directed to a value obtained from a model, prompt, or Pipeline inputs. Unknown `provider`,
unknown field `auth`, missing mandatory authorization, and unsupported parameter are connection
preparation errors.

The installed adapter must provide a verifiable description of its fields `auth`, `parameters`, and
model capabilities. The general scheme allows their extension but does not grant permission to
arbitrarily pass unknown fields to the API. The set of implemented adapters is versioned together
with the engine; the adapter name in the profile does not install it automatically.

The current engine implements `ollama`, `openai`, and `anthropic`. Cloud connections require
`auth.key` with `secretRef`. Parameters, addresses, and capability check boundaries are given in
[adapter documentation](../../internal/adapters/README.md). For cloud models, admission records the
available model ID, not weight digest. Support for text/tools relates to the adapter protocol; a
specific model must support function calling. `imageInput` is currently available only in Ollama.

### 4.2. Pipeline.spec.models

| Resource Field | Required / Default Value | Semantics                                                              |
| -------------- | ------------------------ | ---------------------------------------------------------------------- |
| `connection`   | Required                 | Canonical name `EngineProfile.spec.models`                             |
| `model`        | Default connection model | Binding of a specific model identifier                                 |
| `parameters`   | Default `{}`             | Explicit connection parameter overrides                                |
| `requires`     | Default `[]`             | Required capabilities: `toolCalling`, `structuredOutput`, `imageInput` |

Effective parameters are computed via a shallow merge by key: first `connection.parameters`, then
`Pipeline.models.<alias>.parameters`. The alias value entirely replaces the value of the matching
key, including the object or array. `null` is a pass-through JSON value if the adapter allows it;
there is no special removal operation for parameters.

Parameters cannot override `provider`, `baseUrl`, authorization, messages, tool schemas, response
schema or other parts of the request managed by Knotra. The adapter checks such conflicts before
execution. In the absence of a parameter, the documented behavior of the adapter/API is applied,
recorded in the plan via adapter version and final configuration.

`requires` supplements requirements derived from the node type. Each `agent` requires `toolCalling`,
even if it is not listed in alias and no external tools are issued to it. The engine checks
feasibility and rejects unsupported configuration; it does not replace the model nor remove the
requirement. Support from the provider for a structured response does not cancel the result check by
Knotra contract.

Fields `nodes.<id>.llm.model` and `nodes.<id>.agent.model` refer to the alias of the current
Pipeline. Several aliases can use one connection with different models and parameters. Resolution of
the connection in a nested Pipeline allows exactly the selected connection with such overrides; v1
does not contain a separate list of allowed models or parameters within the connection. For
distinguishing rights on the provider side, corresponding credentials and connections EngineProfile
are used.

## 5. MCP

### 5.1. General Connection Fields

| Field             | Required / Default Value | Semantics                                                       |
| ----------------- | ------------------------ | --------------------------------------------------------------- |
| `transport`       | Required                 | `streamable_http` or `stdio`                                    |
| `allowedTools`    | Required, `[]` allowed   | Exact names of MCP tools that are allowed to be called          |
| `toolPolicies`    | Default `{}`             | Effect and idempotency policy for each tool                     |
| `allowRunSession` | Default `false`          | Permission to share an MCP session within one Pipeline instance |

Tool name corresponds to `[A-Za-z0-9_.-]{1,128}`. `allowedTools: []` does not allow calls. A tool
discovered by the server does not become allowed automatically. All keys `toolPolicies` must be
included in `allowedTools`; absence of policy means `effect: unknown` without idempotency key.

The engine discovers requested tools and checks their input schemas during run preparation. It is
impossible to satisfy an explicit grant for a missing tool. Operator policy has priority over
description and annotations returned by the server. Changing name or schema of a tool after freezing
the plan requires stopping the corresponding call with incompatibility diagnostics; new tools are
not added to the plan. Schema fixation does not guarantee immutability of remote implementation.

MCP sampling, roots, elicitation, prompts and resources are not issued automatically from this
section. V1 defines tool calls here; the server receives no additional reverse capabilities solely
based on successful connection.

### 5.2. Streamable HTTP

For `transport: streamable_http`, `url` and general required fields are mandatory. `url` is an
absolute HTTP(S) URL without userinfo and fragment; this is the full MCP endpoint. Whether
unencrypted HTTP is allowed is determined by deployment configuration. `headers` is an optional map
`CredentialValue` from section 3.2, default `{}`.

Header names are checked as HTTP field-name; case-insensitive match is considered a duplicate.
Transport headers (`Host`, `Content-Length`, `Connection`, `Transfer-Encoding`), MCP negotiation and
session identifiers are assigned by Knotra client; overriding them via profile is forbidden.
Connection settings do not move along redirect to another origin together with credentials; such
redirect causes connection error. Authorization v1 is set by static headers from trusted profile;
interactive OAuth flow is not implied.

HTTP call executes a trusted MCP engine client. It does not require issuing network or HTTP
credentials to the agent sandbox.

### 5.3. Stdio

For `transport: stdio`, the following are mandatory:

| Field          | Semantics                                                                     |
| -------------- | ----------------------------------------------------------------------------- |
| `sandbox`      | Canonical name `EngineProfile.spec.sandboxes`, inside which the server is run |
| `command`      | Non-empty list of strings: executable file and arguments                      |
| `allowedTools` | General list of available tools                                               |

`env` — optional map `CredentialValue`, by default `{}`. `command` is run inside a separate sandbox
MCP-server as argv without implicit shell, `$VAR` extensions and command substitution. Explicit
shell run is allowed if the operator specified it in `command`. Required programs and dependencies
must be contained in the image.

Each `secretRef` in `env` must exist in the profile and belong to `allowedSecrets` of the selected
sandbox. This is a trusted MCP configuration check. A pipeline authorized to use this connection is
not required to additionally request raw server credentials or the profile of its service sandbox.

Launching the supporting MCP process is an action of a trusted adapter; `sandbox.allowedTools`
limits tools provided to agents and code nodes, and does not prohibit launching this explicitly
declared process. Resources, network, and `allowedSecrets` of the profile apply to it fully.

The MCP server does not share the file system, environment variables, and processes with the agent.
Its stdin/stdout are used for the MCP protocol, stderr — for a diagnostic stream excluding known
secret values. It is not passed Docker socket, host files, and worker-process variables.

### 5.4. Alias and session scope

`Pipeline.spec.mcp.<alias>` contains mandatory `connection` — canonical name of MCP connection, and
optional `session`: `node` by default or `run`.

| `session` | Scope                                                                                                |
| --------- | ---------------------------------------------------------------------------------------------------- |
| `node`    | Separate MCP session on alias and attempt of node instance. Actions of one agent reuse it repeatedly |
| `run`     | Common MCP session on alias and one execution instance of this Pipeline                              |

`run` is allowed only with `connection.allowRunSession: true`. One call of a nested pipeline forms
its own session scope; different calls do not share it. Different aliases of one connection create
different sessions. Permissions of each call are checked independently from the shared session.

Calls within one `run`-session are executed sequentially. Order of simultaneously ready branches is
not determined by YAML: if it affects the result, dependency is specified in the graph. Mutable
state of the session is not considered node output and does not replace data transfer via ports.

After session loss, its internal state cannot be silently considered restored. Continuation is
possible only with a server-supported session or according to node re-execution rules; if the
previous action result is uncertain, `onUnknownOutcome` is applied. MCP-server state does not
automatically enter Temporal checkpoints.

### 5.5. ToolPolicy and repeated calls

Each `toolPolicies.<tool>` record contains mandatory `effect` and optional `idempotencyArgument`.

| `effect`  | Semantics                                                                                               |
| --------- | ------------------------------------------------------------------------------------------------------- |
| `read`    | Operator confirms that the call does not change significant external state; repeat by policy is allowed |
| `write`   | Call changes external state                                                                             |
| `unknown` | Effect is not classified; on uncertain outcome it is treated as a potential write                       |

`effect` determines admissibility of repeats, and does not grant call right and does not require
human confirmation. Required human participation is expressed in the graph. Automatic repeat
`write`/`unknown` is allowed after proven absence of effect or with an active service idempotency
mechanism. Otherwise, node uncertain outcome policy is used.

`idempotencyArgument` — non-empty JSON Pointer to field of string key in tool arguments object. All
intermediate segments must refer to objects; array addressing is not supported. Field along this
path is reserved by Knotra and must be allowed by tool input schema. Agent and `tool.arguments`
cannot set it independently. From the model-shown schema, this field and requirement of its presence
are excluded. On key insertion, engine creates missing intermediate objects; present value of
another type is an argument error. Then engine validates full arguments against original schema
before sending. If tool schema cannot be correctly projected thus, connection of this tool is
rejected during preparation.

The engine creates a stable key for one logical tool invocation and reuses it within allowed retries
of that operation. For a direct tool node, all execution.retry attempts of one instance belong to
one logical MCP call and preserve this key. A new logical invocation, new iteration, or new run
receives another key. Recording the key and intent to perform an action must precede sending the
request. The operator setting `idempotencyArgument` confirms support for the key by an external
service, its valid uniqueness scope, and preservation of protection throughout the entire possible
retry interval within the run timeout. If such a guarantee is absent from the service, the field is
not set. Numeric key retention time and transformation of the server-side idempotency mechanism are
not separate v1 fields.

Presence of a key does not promise exactly-once for an arbitrary MCP server. If the protection
duration or scope no longer fits for safe retry, the outcome remains undefined. A completed call is
not re-executed when replaying saved history.

## 6. Sandbox profiles

### 6.1. Fields

Each `EngineProfile.spec.sandboxes.<name>` defines one fixed profile:

| Field            | Requiredness / Default Value | Semantics                                                         |
| ---------------- | ---------------------------- | ----------------------------------------------------------------- |
| `image`          | Required                     | Non-empty reference to an image available in backend environments |
| `resources`      | Required                     | All four constraints from the table below                         |
| `network`        | Required                     | One of the network policies                                       |
| `allowedTools`   | Required, `[]` allowed       | Allowed sandbox built-in tools                                    |
| `allowedSecrets` | Default `[]`                 | Canonical names of secrets allowed in `env`                       |

| `resources` | Range                    | Meaning                                                              |
| ----------- | ------------------------ | -------------------------------------------------------------------- |
| `cpu`       | Number, `0 < cpu <= 128` | Upper limit of CPU time, in equivalents of number of CPU             |
| `memoryMiB` | Integer `1..2147483647`  | Maximum container memory in MiB                                      |
| `diskMiB`   | Integer `1..2147483647`  | Total limit of mutable disk, including workspace and temporary files |
| `pids`      | Integer `1..2147483647`  | Maximum OS tasks, accounting for child processes and threads         |

MiB equals 1048576 bytes. Parameters are limits, not guarantees of resource allocation. Mutable disk
includes writable layer and dedicated work volumes; immutable image layers and already published
artifact storage do not enter this limit. Backend must prevent bypassing the limit via additional
volume, tmpfs, or swap. A backend unable to ensure the requested constraint rejects the profile
during capability check, rather than ignoring the field.

If `image` uses a tag, the engine resolves it to an immutable digest before the run and records the
digest in the plan. A retry must not fetch new content of the same tag. The pipeline selects a
profile via `spec.sandboxes.<alias>.profile`; image change, resource, or network fields are absent
in alias. Other requirements are set by another trusted profile.

### 6.2. Network

| `network`                         | Behavior                                                                         |
| --------------------------------- | -------------------------------------------------------------------------------- |
| `{mode: none}`                    | External network is unavailable; processes of one sandbox may use local loopback |
| `{mode: allowlist, hosts: [...]}` | Outbound access allowed only to listed hosts                                     |
| `{mode: any}`                     | Outbound network allowed within backend and deployment constraints               |

In `allowlist`, the list of `hosts` is required and non-empty. An element is an exact ASCII DNS name
or IP address; URL, port, path, wildcard, and CIDR are forbidden. For DNS, comparison is
case-insensitive and ignores trailing dot; IPs are compared after normalization. Duplicates after
normalization are an error. A Unicode DNS name must be represented in IDNA ASCII beforehand.

Host record allows its TCP/UDP ports; separate port restriction in v1 is absent. DNS serves a
controlled resolver; resolving a DNS query does not open an arbitrary outbound channel. Backend must
prevent bypass via direct IP, alternative resolver, proxy, or redirect to an unallowed host. An
allowed name does not mean allowing an arbitrary value to which it subsequently redirects the
request. A specific backend must document the implemented filtering model and reject unsupported
`allowlist`.

Sandbox network does not manage trusted runtime calls to models and MCP: they are checked via
connections and grants. Network policies also do not open inbound ports from host. Port publishing
settings in v1 are absent.

### 6.3. Tools and lifecycle

`allowedTools` may contain `files.read`, `files.write`, `process.exec`. These names are
simultaneously used in agent grants. `process.exec` allows execution of an arbitrary command within
sandbox; such a command may read and modify its accessible file system. Absence of
`files.read`/`files.write` does not turn shell into a tool without access to files.

`agent` always selects the sandbox alias. `code` requires that its selected profile allows
`process.exec`; its declared command is an explicit request for this capability. For a code node
`defaults.tools` does not issue additional actions. Placement of declared inputs and collection of
outputs is performed by the trusted runtime as part of the data transfer contract, not as agent
tools.

Each attempt of instance `agent` or `code` receives a separate workspace. The environment is
preserved between actions of this agent and is not shared with other nodes. A new attempt uses a new
workspace with the original declared materials; implicit continuation in a partially modified
directory is forbidden. The management checkpoint is not considered a disk snapshot.

No v1 profile allows arbitrary custom host mounts, Docker socket, privileged mode, or a shared
workspace between nodes. Input and output placement paths are specified within the workspace under
the primary contract. Arbitrary additional Docker fields are not profile extensions.

## 7. Permission Calculation

### 7.1. Agent Tools

Common explicitly issued tools are contained in `Pipeline.spec.defaults.tools`: `mcp` is a map of
MCP-alias to a list of tool names, `sandbox` is a list of built-in tools. Missing fields equal empty
sets.

`agent.tools` has the same fields plus `inherit`, which defaults to `true`:

- At `inherit: true` the common grant Pipeline and this agent's grant are united by sets.
- At `inherit: false` only this agent's grants are used.
- Missing `tools` means inheritance of common grants.
- Empty `tools.mcp` or `tools.sandbox` at `inherit: true` does not remove the common grant.

For each requested MCP tool, the engine resolves the alias to its canonical connection and checks
`allowedTools` connections and all active permissions of nested Pipeline calls. For a built-in tool,
`allowedTools` selected sandbox is checked.

If at least one requested tool is invalid, plan preparation ends with an error. The engine cannot
silently pass only the intersection to the model: this would change an explicitly described task. At
correct description the model receives only the computed set; each actual call is rechecked by the
trusted runtime.

`tool` node requests its single MCP tool via `tool.server` and `tool.name`; this is an explicit
grant of the node itself. It passes the same connection and nested call restrictions, but does not
require repetition in `defaults.tools`. `llm` does not receive tools via defaults. Control nodes and
`human` do not execute tools on behalf of the agent.

### 7.2. Nested Pipelines

Node `type: pipeline` contains `pipeline.file` and mandatory `pipeline.permissions`. The latter must
contain all four fields, even if values are empty:

| Field       | Format                                                 | What allows child Pipeline                           |
| ----------- | ------------------------------------------------------ | ---------------------------------------------------- |
| `models`    | List of canonical connection names                     | Model calls through these connections                |
| `mcp`       | Map of canonical connection to non-empty list of tools | Only listed MCP tools                                |
| `sandboxes` | List of canonical profile names                        | Selection of these profiles for agent and code nodes |
| `secrets`   | List of canonical secret names                         | Explicit retrieval of these secrets in sandbox env   |

`models: []`, `mcp: {}`, `sandboxes: []`, `secrets: []` mean absence of the corresponding
permission. Automatic inheritance of all caller Pipeline rights is not present. Using parent or
child alias in these fields is forbidden: values are resolved directly in EngineProfile. This
eliminates dependency of rights on alias renaming.

Child Pipeline uses its own aliases, defaults and input description. Its resource catalog must
entirely fit within call permissions: declared model alias, sandbox or secret must be permitted, and
MCP-alias must have a connection record in `permissions.mcp`. Each actually requested tool is
additionally checked against the list of this record. Unused grants do not create permissions beyond
the upper bound.

Connection rights include trusted authorization and service sandbox of its MCP server. They do not
include export of secrets of this connection to code or issuance of MCP server sandbox to agent. For
such separate use, corresponding `secrets` and `sandboxes` are required.

At nesting, the active upper bound equals intersection of permissions of all calls from root run to
current Pipeline and EngineProfile restrictions. For MCP, sets of tools intersect by canonical
connection. New `pipeline.permissions` can only narrow the active bound; request for expansion is a
preparation error, even if branch does not execute. Child reference to common EngineProfile does not
allow bypassing parent restriction.

Root Pipeline gets access to catalog of selected profile within explicitly described operations.
Nested `foreach` and `loop` use catalog and defaults of their Pipeline; they do not create new
delegation area. New bound arises only at `type: pipeline`.

## 8. Limits and Common Counters

`EngineProfile.spec.limits` must explicitly set all fields. No automatic numeric default values
exist.

| Field                | Format                     | Value                                                                |
| -------------------- | -------------------------- | -------------------------------------------------------------------- |
| `timeout`            | `[1-9][0-9]*(ms\|s\|m\|h)` | Maximum run duration, including waits                                |
| `maxConcurrentNodes` | Integer `1..2147483647`    | Maximum number of nodes executing real work simultaneously           |
| `maxNodeInstances`   | Integer `1..2147483647`    | Maximum number of logical node instances per run                     |
| `maxModelCalls`      | Integer `1..2147483647`    | Maximum number of queries sent to models                             |
| `maxToolCalls`       | Integer `1..2147483647`    | Maximum number of MCP calls and built-in tool invocations in sandbox |

`Pipeline.spec.limits` may contain any subset of these fields. A missing field inherits the nearest
active upper bound. An explicit value must be less than or equal to it; exceeding it is an error,
not silently truncated. `0` and special "unlimited" are not supported.

For a child Pipeline, its limits inherit and may tighten those of the calling Pipeline;
EngineProfile limits remain in effect for the entire root run. A child Pipeline's local timeout
starts from the beginning of its execution; it never extends the parent's remaining time. The root
timeout count begins upon engine acceptance of the run. Queue wait, retry backoff, human
intervention, and indeterminate outcomes are included in the timeout and do not pause the clock.

`execution.timeout` limits a node by its primary contract. Any local timeout acts within the
remaining time of all encompassing operations. Neither a new node nor a new attempt nor a new child
Pipeline resets the upper timeout.

Counters belong to the root run and each nested Pipeline instance with its active limits. Each
operation reserves budget simultaneously across all encompassing scopes. Reservation is serializable
relative to competing operations so parallel branches do not spend the same last permission. Engine
recovery, retry, new iteration, and launching a child within the prior run do not reset already
spent budget.

Accounting rules:

- `maxNodeInstances` accounts for instances of all node types, including control nodes. An instance
  is created and accounted for before its `when` check; a skipped node also has an instance. A retry
  of one instance does not create a new instance. Body iteration or a new child call creates its own
  instances. Nodes of a branch that was never instantiated are not accounted for.
- `maxConcurrentNodes` accounts for active work of a leaf node. Waiting on dependencies, queues,
  human intervention, retry, or response from a child graph does not occupy a slot. Control nodes
  `foreach`, `loop`, and `pipeline` do not hold a slot while waiting for their child nodes. A model
  query or tool call inside an agent uses that agent's slot.
- `maxModelCalls` accounts for each actual outgoing request scheduled by the adapter, including
  requests of subsequent permitted node attempts and agent requests. Re-reading an already saved
  response does not spend budget.
- `maxToolCalls` accounts for each outgoing MCP call or built-in tool invocation, including retries.
  The primary run of a declared command `code` also counts as one `process.exec` call. Starting an
  auxiliary MCP server, establishing a protocol session, discovery, and artifact transfer are not
  counted as tool invocations.
- Commands and programs inside `process.exec` do not spawn separate `maxToolCalls` for each system
  call or HTTP request; they are limited by sandbox, resources, and timeout.
- Reservation occurs before sending. After possible sending it is not returned to budget even on
  error or indeterminate outcome. If it is proven that an operation could not start, the engine may
  atomically cancel the reservation. A lost worker is not such proof.

SDK providers and MCP must not perform hidden retries bypassing these counters. Built-in SDK retries
are disabled: a request error ends a node attempt. A new send is allowed only as a permitted next
attempt per execution contract; there is no separate hidden transport retry counter.

Upon exhausting the counter, a new operation is not sent and a limit error is created.
`maxConcurrentNodes` limits parallelism by waiting for a free slot; occupied slots in themselves are
not an error. Upon timeout expiration, planning stops, and active work receives cancellation.
Completed external effects are not rolled back.

These limits are not monetary budget and do not promise precise provider billing accounting. An
interrupted request may be charged, and tokens may become known only after response. V1 contains no
fields for monetary limits and global quota between independent root runs; an operator may
additionally limit infrastructure.

## 9. Freezing and changing configuration

Upon run acceptance, the engine freezes the used EngineProfile version by content, final aliases,
model parameters, grants, limits, image digests, adapter versions, and discovered tool schemas.
Secret values, runtime session ID, and OAuth tokens are not included in the user snapshot.

Editing a profile changes configuration for subsequent runs. An ongoing run uses the previously
fixed plan; substitution with a broader current configuration is forbidden. An operator may revoke
permission or stop connection at the engine level. Such revocation blocks further operations and is
reflected in run state; the saved plan does not provide right to bypass an active operator ban.

Reissuing a result from saved history does not request a secret and does not call an external
service. Repeating a real operation passes the current checks and uses the secret via the saved
link. Changes to the secret, state of a removed MCP, provider updates, and external data can change
the result of a real operation even with the same package and plan.
