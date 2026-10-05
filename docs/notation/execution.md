# Knotra v1 execution semantics

The implementation lives in `internal/contract`, `internal/engine` and `internal/adapters`. Setup is
described in the [running guide](../running.md); verified scenarios and support boundaries are in
the [report](../verification.md).

Status: normative v1 contract, checked against the implementation on 5 October 2026. The structural
JSON Schema and textual semantics apply together. External system requirements are checked at
admission; unsupported providers are rejected before execution.

“Must”, “prohibited” and “error” specify mandatory behavior for a compatible executor. If the
structural JSON Schema accepts a construct prohibited by these semantics, the validator must reject
it before execution whenever possible. External actions are not performed while parsing, compiling
the graph or evaluating CEL.

## 1. Execution units

A **Graph** is a set of `inputs`, `nodes` and `outputs`. The root graph is in `Pipeline.spec`;
`foreach.body` and `loop.body` contain separate graphs. A called `pipeline` uses a graph from
another `Pipeline` document in the same frozen package.

A **node definition** is an entry in `nodes` with an immutable identifier within its graph. A **node
instance** is a particular execution of that definition: each `foreach` element and `loop` iteration
has its own nested node instances. An **attempt** is a permitted repeated execution of an instance.
Retrying an attempt creates neither a new definition version nor a new root run.

Every instance has a stable address containing the root run, nested graph chain, element/iteration
indexes and node identifier. The engine also assigns separate IDs to attempts and external
operations. API identifier formats are not part of YAML; their stability and uniqueness are
required.

YAML key order does not define execution order. Dependencies and data readiness determine it.
Independent instances may execute concurrently; the precise order of calls to independent external
services is not guaranteed. Array element order, `switch.cases` order and command argument order
matter.

The package, resolved resources and compiled plan are frozen before execution. Changing a source
file does not change an ongoing run. Replaying saved history must not itself repeat external
actions.

## 2. Graph inputs and defaults

A graph call passes an object containing its input port values. Undeclared input names are
prohibited. The following order applies to each port:

1. If the name is present in the supplied object, use that exact value, including `null`.
2. If the name is absent and `InputPort.default` is specified, use that value.
3. If no value exists and `required` is `true` or omitted, fail the call with an input error.
4. If no value exists and `required: false`, the port remains absent; its name is not included in
   the `inputs` object.
5. Validate present values against the port's schema or artifact contract.

`default` is an input port mechanism. The annotation with the same name inside JSON Schema does not
insert data or change the document. The default must satisfy the port schema during package
validation. Artifact ports prohibit `default`: a package cannot create an authorized artifact
reference from a constant.

These rules apply equally to the root graph, nested `body` and called pipeline. A root run with
invalid inputs is not admitted; invalid inputs for a nested graph fail its controlling node.

## 3. Binding values

A `Binding` has exactly one form:

| Form                    | Value                                                                                        |
| ----------------------- | -------------------------------------------------------------------------------------------- |
| `value`                 | Literal JSON value without interpolation, computation or recursive parsing of nested strings |
| `from`, optional `path` | Value of one existing port/variable, with optional JSON Pointer extraction                   |
| `expr`                  | Result of a restricted CEL expression in the permitted scope                                 |
| `coalesce`              | First present alternative in declaration order                                               |

For scheduling, a binding has one of four results:

- **pending**: required sources have not yet reached a resolved state;
- **present(value)**: a value exists, including JSON `null`;
- **missing**: a value is absent;
- **error**: evaluation or source resolution failed.

`missing` is internal data-resolution state, not a JSON value. It is not serialized as a string,
`null` or an empty object. `skipped` is a node instance state; its outputs yield `missing`. An
absent optional output of a successful node also yields `missing`, while the source remains
`succeeded`.

### 3.1. `from` and `path`

`from` addresses an entire port. The structural schema and scope table below define permitted names.
Names within a path are not searched for in outer graphs. Referencing a nonexistent definition, port
or unavailable variable is a package error, not `missing`.

`path` is a JSON Pointer: the empty string denotes the whole value; `/` separates tokens; `~1`
denotes `/` and `~0` denotes `~`. A token is decoded first, then used as an object key or array
index. Array indexes permit `0` or a decimal number without leading zeros; `-` does not denote an
element. An absent object key or array index yields `missing`. Applying a nonempty path to a number,
string, boolean or `null`, or using an invalid array index, is an evaluation error.

If the source itself is `missing`, the path is not evaluated. A `pending` source yields `pending`.
Paths into artifact references or artifact collections are prohibited: their internal representation
is not arbitrary user JSON.

### 3.2. `expr`

CEL returns typed JSON-compatible values. NaN, values outside Knotra's numeric profile and CEL
values not representable in JSON are prohibited. `when` and `until` must return boolean; strings,
numbers, `null` and objects are not converted to truthiness.

Expression references to graph ports are determined statically. Dynamic node or port-name selection
is prohibited. An expression waits for permitted sources that have not finished; if a required
expression source is absent, the binding yields `missing`. CEL short-circuiting does not remove a
statically discovered dependency on another node. Use `coalesce` for alternative branches and
`required: false` for optional inputs.

After port resolution, ordinary CEL errors remain errors: absent object fields, division by zero,
incompatible types and computation-limit violations. `coalesce` does not catch them. CEL may check
for optional fields in an already received JSON object. Access to absent optional `args` without a
presence check is an error.

CEL receives no secret values, current time, randomness, network, filesystem, Temporal internals or
uncommitted results of neighboring nodes. Artifact references pass through `from`; creating artifact
references from JSON or CEL is prohibited.

### 3.3. `coalesce`

Alternatives are checked left to right. `missing` allows the next alternative; `present`, including
`present(null)`, ends selection. `error` fails evaluation. Before selection, the engine waits for
terminal states of every statically discovered source in every alternative, including later ones.
Selection then does not depend on parallel branch speed. Any source error triggers global fail-fast,
even when an earlier alternative could have supplied a value.

If every alternative yields `missing`, the whole `coalesce` yields `missing`. Expressions after the
selected alternative are not evaluated; their sources are already resolved, and their references
participate in port-existence validation, terminal-state waiting and cycle checking. An early
alternative's result does not cancel other branches: the graph executes all its non-skipped nodes.

`coalesce` selects a value. It is not error handling, a request race, selection of the first
completed node or automatic model switching.

## 4. Scopes

| Location                        | Available variables                                                               |
| ------------------------------- | --------------------------------------------------------------------------------- |
| `nodes.<id>.inputs.<port>.bind` | `inputs` and `nodes` of the current graph                                         |
| `Graph.outputs.<port>.bind`     | `inputs` and `nodes` of this graph                                                |
| Common `node.when`              | `inputs`, `nodes` of the current graph and `args` for this node's resolved inputs |
| `switch.cases[].when`           | This `switch`'s `args`                                                            |
| `tool.arguments`                | This `tool`'s `args`                                                              |
| `foreach.with`                  | This `foreach`'s `args`, `iteration.item`, `iteration.index`                      |
| `loop.state.<name>.initial`     | This `loop`'s `args`                                                              |
| `loop.with`                     | `args`, current `state`, `iteration.index`                                        |
| `loop.state.<name>.next`        | `args`, current `state`, `iteration.index`, completed `body.outputs`              |
| `loop.until`                    | `args`, current `state`, `iteration.index`, completed `body.outputs`              |

`inputs.<name>` denotes an input of the current graph. `nodes.<id>.outputs.<name>` denotes a
published node output in the same graph. `args.<name>` denotes a declared input of the current node
after evaluating `inputs.<name>.bind` and checking its contract. `body.outputs.<name>` denotes only
a declared export of the completed loop body; its internal nodes are inaccessible from outside.
Matching names do not merge these namespaces.

Inside `body`, the table applies again: a body node sees body inputs and neighbors. Outer `args`,
`state`, `iteration` and parent graph nodes do not enter it. Required values are passed through
`with` to declared body inputs. Resource and schema registries belong to the containing `Pipeline`
document; a nested `Graph` uses them without creating its own registries.

`pipeline` opens another document's scope: its inputs, schemas, resources, defaults and nodes
resolve in that document and are constrained by supplied permissions. There is no access to parent
graph data beyond passed inputs.

## 5. Readiness and dependencies

For each graph, the compiler derives dependencies from `nodes` references in input bindings, common
`node.when` and `needs`. Dependencies in `coalesce` preserve alternative order and structure. All
potential edges participate in DAG checking, including alternatives and condition branches that a
particular run might not need. Self-references and cycles are prohibited.

A **strong data dependency** is a source whose value is necessary to resolve a required input. An
**optional dependency** is a source of an input with `required: false`. An **alternative
dependency** is a source of a `coalesce` branch. Optionality permits absence, not early reads: the
engine waits for the required source's final result. `coalesce` waits for all its static sources to
become terminal, then selects in alternative order; not all values need be present.

`needs` is a separate ordering dependency with an **all-success** rule:

- Every listed node must finish `succeeded`.
- While a required node is unfinished, the consumer waits.
- If any required node is `skipped`, the consumer also becomes `skipped`.
- Failure or cancellation of a required node follows global run termination; `needs` cannot continue
  after failure.
- `needs` transfers no data, changes no `args` and grants no access to unexported values.

Instance admission follows this logical order:

1. Check cancellation, run failure, deadlines and limits.
2. Resolve `needs`; if all-success is impossible, mark skipped.
3. Resolve input bindings. Wait for required `pending` sources. Fail the node on `error`.
4. If a required input yields `missing`, mark the node `skipped` because of a missing required
   input. Omit optional `missing` inputs from `args`.
5. Validate present input values against schemas. Mismatches are errors, including `null` when its
   schema disallows it.
6. For a specified common `when`, wait for all its static sources in the current graph. Absence of a
   strong source means `skipped` because of a missing dependency. Otherwise evaluate: `false` means
   `skipped`; errors or non-booleans, including present `null`, fail the node. An absent `when`
   means `true`.
7. Admit the instance to execute its type and allocate necessary resources.

A skipped node creates no sandbox, calls no model or MCP, creates no human request and publishes no
outputs. Skipping because of a missing required input propagates branch selection through the graph.
If that branch's result is a required graph output without an alternative, the graph ultimately
fails because the result is absent.

Skipping differs from an invalid reference name: a typo is always diagnosed as an invalid contract,
not silently turned into a skipped branch.

## 6. Graph completion and publication

All graph nodes execute whether or not graph outputs reference them. v1 does not evaluate only a
subgraph of demanded outputs. A graph succeeds when every instance is terminal, each is `succeeded`
or `skipped`, and all required graph outputs have been computed and validated.

Graph outputs follow `Binding` rules. `required` defaults to `true`. A required `missing` output is
a graph error; optional `missing` outputs are absent from the published object. Every present output
is validated against its contract. A successful graph may contain skipped nodes if its final
contract is satisfied.

A node publishes outputs **atomically**: either all required JSON ports and file artifacts are
validated, saved and available with confirmed `succeeded`, or no public successful result exists.
Absent optional ports are not published. Partial model tokens, stdout, draft files and tool events
are observations; they do not become other nodes' inputs.

Artifacts are first saved as prepared objects, then included in committed publication. After a
failure, the engine must recover or complete publication using a stable identifier. Saving a file
without committing the result does not make a node successful; repeating the commit does not create
a new logical result.

Publication of controlling `foreach`, `loop` and `pipeline` nodes is also atomic for the parent
graph. Successful nested instance results remain in history, but parent consumers receive only the
controlling node's completed output. Publication does not roll back effects of executed external
operations.

## 7. Common result contract

For `llm`, `agent`, `code` and `human`, result objects contain declared **JSON-port** names as keys
and the port values directly as values. Extra keys are prohibited. Required JSON ports must be
present; optional ports may be omitted. An explicit `null` is schema-validated and is not absence.

For `agent` and `code`, file ports do not appear in this JSON object. The trusted executor collects
them according to `outputs.<name>.collect`. All declared file ports are required; `FileOutput` has
no `required: false`. A node with only file outputs returns an empty JSON object.

`collect.path` specifies the exact relative file path in the sandbox working directory; there is no
wildcard or recursive directory collection. The path is checked after resolving filesystem
references and cannot escape the sandbox or access protected control data. The file must exist, be
regular and readable, and satisfy size limits. Symlinks in any segment and hard links are
prohibited; validation and reading must prevent path substitution between them. Before collection,
code runtime terminates remaining child processes; bytes cannot change once publication begins.
`collect.mediaType` must appear in allowed `artifact.mediaTypes`. Declaring a type does not prove
that file contents are valid.

`FileOutput.artifact.collection` permits only `false` or absence. Controlling `foreach` creates
collections that pass as artifact collections; one `collect.path` does not collect a collection.
Undeclared files remain temporary materials and are not published automatically.

## 8. `llm`: one logical model call

`llm.model` references a resource from `spec.models`. `instructions` is optional and `prompt`
required; both are read from literal `text` or a package file. Message preparation and input
representation follow the [main contract](v1.md). These strings have no built-in template language;
they are not executed and receive no implicit secret substitution.

v1 prohibits artifact inputs for `llm`; it accepts JSON inputs. The node makes one logical model
request and expects one result object under section 7. The adapter passes the final JSON contract to
the model. Knotra additionally checks result fields and nested values regardless of provider
capabilities.

Pipeline tool permissions do not turn `llm` into an agent: this node's model does not invoke MCP,
sandbox or child nodes. Requests to invoke a tool, refusal instead of the required result,
incomplete/truncated output, invalid JSON or schema violations are errors. Markdown fences,
extracting an object from arbitrary prose and guessing missing fields are not part of the ABI.

There is no automatic extra request to repair a result. Repeating the whole request is possible only
under an applicable attempt policy and counts toward limits. The model never changes automatically.

## 9. `agent`: model and tool loop

The agent receives a fresh isolated working environment from `sandbox`, its validated inputs,
instructions, prompt and effective tool permissions. Inputs are passed in a separate typed object
`{"values": {...}, "artifacts": {...}}` with the same descriptor representation as in section 10.
One working environment lives within an attempt between tool calls. History of other agents and past
runs is not included automatically.

`agent.maxSteps` — maximum number of model turns per attempt; the first request counts as the first
turn. A turn returns a request for one or several tools or a text response. Completion is expressed
by calling the control tool `knotra.finish`, described below. Every model request also counts toward
the shared `maxModelCalls` limit, including requests in subsequent permitted node attempts.
Automatic transport retries within an attempt do not exist.

Tools requested in a single turn are executed sequentially in the order returned by the provider.
Each tool is separately checked against permissions, argument schema and limits. Results are
returned to the model before the next turn. The engine preserves the identity of each logical call.
If the provider does not give a definite order, the adapter must fix it before executing actions.

When the model on the last allowed turn requests regular tools, the engine fails the node with a
limit error before executing this new set: there is no room for the next model turn. A correct
`knotra.finish` on the last turn is allowed. A request delivery error ends the attempt; runtime does
not perform hidden re-sending, keeping it under the same turn number.

`knotra.finish` — reserved control tool that runtime provides to any agent regardless of external
grants. Its arguments are directly a JSON-ports object per section 7; for an agent with only file
outputs this is `{}`. Required files must exist by this point. This call must be the only tool
request in its model turn. It checks JSON-ports and file outputs, then atomically publishes success.

If `finish` contains invalid arguments, missing/incorrect files or adjacent calls in the same turn,
the entire set of tools is not executed. An agent receives a validation error for the next turn if
budget `maxSteps` remains. Such correction belongs to the same agent cycle and does not reset
limits. If turns are exhausted, the attempt ends with `OUTPUT_INVALID`. A model's textual statement
of completion without `finish` does not publish outputs: runtime reports on the required way of
completion, spending the next available turn; upon exhausting turns a limit error is recorded.

Tool errors with a definite outcome may be returned to the agent as a tool result so that it can
make the next decision within `maxSteps`. Cancellation errors, deadline errors, general budget
violations, permission violations and uncertain outcomes of mutating actions are managed by the
engine and do not provide the model the right to bypass stopping. A new call after a known error is
a separate agent action; it is not masked as a transport retry.

The agent uses only declared allowed tools. Self-added MCP server, creating a new graph node,
privilege escalation or access to orchestrator state are absent. A request for human data is
expressed by a separate `human` graph node; there is no implicit arbitrary agent dialogue with the
user in v1.

### 9.1. Built-in sandbox tools

The names below are stable logical Knotra names. The model adapter may encode them into
provider-allowed names, preserving a unique reverse mapping. Connecting MCP with a similar name does
not replace the built-in tool or `knotra.finish`. Each regular built-in call is checked against
agent grant, sandbox profile, argument schema and general limits and spends `maxToolCalls`. All
objects of arguments and results in this table are closed: unspecified fields are forbidden.

| Tool           | Arguments                                                                                                                         | Successful result                                                         |
| -------------- | --------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| `files.read`   | Required `path: Path`; `root: workspace \| package`, default `workspace`; `encoding: utf8 \| base64`, default `utf8`              | `{content: string, encoding: "utf8" \| "base64"}`                         |
| `files.write`  | Required `path: Path`, `content: string`; `encoding: utf8 \| base64`, default `utf8`; `mode: create \| replace`, default `create` | `{bytesWritten: integer}`                                                 |
| `process.exec` | Required `command: Argv`; optional `timeout: Duration`, default `5m`                                                              | `{exitCode: integer, stdout: string, stderr: string, truncated: boolean}` |

`files.read` selects either `/workspace` or the read-only `/package`. One regular file of no more
than 1 MiB (1048576 bytes) is read; a larger value, directory, link, or going beyond the selected
root causes an error. `utf8` requires valid UTF-8, without replacement of erroneous sequences.
`base64` returns standard Base64 alphabet with padding; the limit applies to source bytes.

`files.write` works only relative to `/workspace`, creates missing parent directories and writes one
file. In `create` an existing path causes an error; in `replace` a regular file must exist. Symbolic
links in any segment of the path are forbidden. Input mounts and protected control data are not
writable. `content` is decoded according to `encoding`: UTF-8 or standard Base64 with correct
padding without extraneous characters. Decoded bytes size no more than 1 MiB; `bytesWritten` equals
this size. Successful write replaces the content entirely, without append; a partially written file
is not considered a successful result of the tool.

`process.exec` uses argv without an implicit shell, cwd `/workspace` and the node's effective `env`.
Its local timeout is limited by the remaining node and run deadlines. Non-zero exit code is saved in
`exitCode` and by itself is not a delivery error of the tool: the agent receives the result and
decides further actions. Inability to run process, timeout expiration or runtime stop give tool
error/cancellation. Invalid sequences in stdout/stderr are replaced with U+FFFD; each returned
stream is limited to 1 MiB UTF-8, truncation occurs at character boundary, `truncated` is true if at
least one stream was truncated. Truncated diagnostic stream is not an artifact.

Regular tools are executed sequentially. Process and all its child processes are sandboxed;
background processes do not continue after returning `process.exec`. If the main command left child
processes, runtime terminates them before returning result. Before collecting files in
`knotra.finish` runtime ensures absence of executing processes capable of changing these files. Call
`process.exec` records potential side effects and prohibits subsequent automatic restart of entire
agent attempt.

## 10. `code`: command ABI

`code.command` — non-empty array of process arguments. First element is executable file, rest are
passed as separate arguments without merging and shell-interpolation. If shell is needed, it is
specified explicitly as command program. Strings are not interpolated by Knotra means.

Program runs in sandbox, working directory — `/workspace`. Declared package materials available in
read-only mounted `/package`. For each artifact entry `agent`/`code` field `mount` is mandatory; for
JSON input or other node type it is forbidden. One file is placed at exact path `mount` relative to
`/workspace`. Collection is placed in directory `mount` with names `0`, `1`, … in order of original
collection, without zero-padding and without deriving names from file metadata. Empty collection
creates empty directory.

All input mounts are read-only. Paths of different mounts cannot coincide or be one inside another;
they cannot overlap runtime control files or package materials. Mounted input does not give access
to arbitrary host source files. Missing optional artifact is not mounted. Collection descriptors
contain exact paths of individual files, not directory path.

Trusted executor sets reserved variables:

| Variable             | Value                                                           |
| -------------------- | --------------------------------------------------------------- |
| `KNOTRA_INPUT_JSON`  | `/knotra/input.json`, prepared UTF-8 JSON file with node inputs |
| `KNOTRA_OUTPUT_JSON` | `/knotra/output.json`, mandatory file of JSON-result object     |

`KNOTRA_INPUT_JSON` contains object `{"values": {...}, "artifacts": {...}}`. In `values` are present
JSON inputs by port names, in `artifacts` — present artifact inputs. Both objects are present,
including empty ones. One artifact is represented by trusted descriptor, collection — array of such
descriptors in original order:

| Descriptor field | Value                                                                                                                       |
| ---------------- | --------------------------------------------------------------------------------------------------------------------------- |
| `id`             | Mandatory non-empty opaque string identifier of registered artifact; not path and not automatically dereferenced URL        |
| `mediaType`      | Mandatory exact MIME-type without parameters, passed port contract                                                          |
| `size`           | Mandatory non-negative integer number of bytes in Knotra numeric profile                                                    |
| `sha256`         | Mandatory string of 64 lowercase hexadecimal characters — SHA-256 of saved bytes                                            |
| `path`           | Only in agent/code context with prepared sandbox: absolute path of mounted input file; for human and other consumers absent |

The descriptor does not contain credentials values and does not grant additional access rights
itself. A user JSON with the same fields does not become a trusted link. Real paths are defined by
`mount` and are located in the working directory. The control input file is available for reading
only, the output file — for program writing. User `env` cannot override names with the prefix
`KNOTRA_`.

Success requires simultaneously: completion of the process with code `0`, a correct
`KNOTRA_OUTPUT_JSON` file with a single JSON object, satisfaction of all JSON schemas, and
successful collection of file outputs. Before collection, runtime terminates remaining child
processes so they do not continue changing results. Absence of result at zero code is an error.
Result at non-zero code is not published. For file outputs only, the program writes `{}`.

stdout and stderr are diagnostic streams, not output data transfer channels. Trailing whitespace
characters in JSON are allowed; multiple JSON documents and extraneous text are forbidden. After
program start, automatic retry of the entire attempt `code` is forbidden: its external effects
cannot be reliably derived from command text. Retries of confirmed failures before process start
within `retry` and deadlines are allowed.

## 11. `tool`: direct MCP call

`tool.server` selects the logical MCP resource, `tool.name` — the exact name of the declared and
permitted tool. `tool.arguments` is computed after inputs and common `when` in the `args` scope; the
result must be a present object and pass the tool's input schema, recorded at run preparation.
`missing` and `null` instead of an object are an error. Unknown arguments are processed by this
schema, not removed by the engine.

The node has exactly one declared output `result`. `tool.response` determines its value:

- `structured` — MCP value `structuredContent`; absence of this field is an error, even if there is
  a text `content`;
- `content` — the entire MCP array `content`, in saved order, in its JSON representation.

`structured` is used by default. The engine does not extract JSON from text, does not concatenate
content blocks, does not automatically download resource links, and does not convert MCP content
into published file artifacts. The obtained value is additionally checked against `outputs.result`.

Protocol/transport error or MCP response `isError: true` — attempt error, not successful `result`.
Negative MCP response by itself does not prove absence of external change: when deciding on retry,
the trusted effect policy of the tool and confirmed outcome are considered.

## 12. `switch`: branch label selection

`switch.cases` is checked top-down in the `args` scope. The first `when` returning `true` selects
`name` of this case. Subsequent conditions are not computed. If all conditions equal `false`, the
required `switch.default` is selected. Error of reached condition ends the node with error.

Case names are unique; label `default` does not match them. Labels are data and need not match node
names. The engine does not start hidden branches by name match and does not cancel other nodes.

The node has one derived output `route`: a string belonging to the set of case labels and `default`.
Declaring `outputs` at `switch` is forbidden by document structure. Subsequent nodes receive `route`
via input binding and use their own `when`; their skips follow general rules.

## 13. `human`: saved request and response

After input preparation and checking `when`, the node creates one saved request with a stable
identifier, prompt, inputs available to the reviewer, and response contract. Inputs are represented
by object `{"values": {...}, "artifacts": {...}}`; artifact descriptors lack sandbox field `path`.
Access to artifact content is provided via its identifier with standard permission check. Then the
node transitions to `waiting_human`. CLI closure, connection break, and re-reading of request do not
create a new request and do not end waiting.

Response — JSON port object per section 7. Incorrect response is rejected with diagnostics; request
remains open and node continues waiting until correct response, cancellation, or deadline. Meaning
like "approved" or "rejected" is set by output port schema and subsequent `switch`/`when`; no
special implicit boolean fields exist.

Response is addressed to a specific request. First accepted valid response atomically closes request
and publishes result. Redelivery of same response with same operation identifier does not create new
acceptance event. Conflicting response on closed, cancelled, or expired request is rejected. If
closure and response occur simultaneously, result is determined by first committed operation.

Wait timeout follows effective `execution.timeout` of node and run deadline. Infinite wait at finite
common deadline does not arise. In v1 `human` has one attempt: automatic sending of new request
after expiry is forbidden.

## 14. `foreach`: limited collection processing

`foreach.over` — name of declared input **of this node**, not Binding, CEL expression, or graph
input name. It must exist in `inputs`; after resolving `args[over]` it must be a present JSON array
or artifact collection. Inappropriate type is error. Order of source collection is fixed before
element creation.

For each element with index `i` from `0` to `N-1`:

1. `iteration.item` equals the current value or one trusted reference to an artifact;
   `iteration.index` equals `i`.
2. All bindings of `foreach.with` are computed in the scope of `args` and these variables'
   iteration. Bindings do not see results of other elements and do not see each other.
3. Keys of `with` form the input object of a separate call to `body`. `missing` omits the input;
   then requiredness and defaults of body inputs are applied.
4. A separate instance of the body graph is executed with its own data scope and separate working
   environments of its nodes.

`concurrency` limits the number of simultaneously open calls to the body, including bodies awaiting
human approval. The general limit of active engine nodes applies additionally. The value of
`concurrency` does not guarantee actual run of exactly that number of elements and does not change
the order of results.

`foreach` has no own `outputs` in YAML. For each body port `p`, the controlling node derives a port
with the same name:

| Body Port                                     | Derived Port of `foreach`                                       |
| --------------------------------------------- | --------------------------------------------------------------- |
| JSON with schema of `S`                       | JSON-array with schema of elements of `S`, length of `N`        |
| One artifact with a set of media types of `M` | Collection of artifacts with the same set of `M`, length of `N` |

Each body output must have `required: true` or an omitted `required`. Collecting optional outputs
with ambiguous index correspondence is forbidden. Nested artifact collection as a body output is
forbidden; JSON-arrays inside JSON ports are allowed and remain nested arrays, without automatic
flattening.

The value of the derived port at position `i` — the value of this port from the element body of `i`,
regardless of the order of element completion. At `N = 0` the body is not started, and all derived
outputs are empty arrays or empty artifact collections; `foreach` completes successfully.

Success requires success of all `N` bodies. Error of any body terminates the controlling node and
run according to the fail-fast rule; remaining bodies receive cancellation. A partially successful
array is not published. Skipped nodes inside the body are allowed only upon successful computation
of all mandatory exports of the body.

## 15. `loop`: sequential limited loop

The loop has **do-while** semantics: if the node itself is not skipped, the body executes at least
once. `maxIterations` — hard limit of the number of calls to the body. Iteration indexes start at
`0`; the number of completed iterations starts from `1` after the first successful body.

`loop.state` describes named slots of portable state. Each slot has a JSON contract or artifact
contract, mandatory `initial` and `next`. Absence of `state` is equivalent to an empty set of slots.
Slots do not become common mutable variables of the body or other nodes.

Before the first iteration all `initial` are computed only from `args`, independently of each other,
and checked against slot contracts. `missing` is forbidden. Cannot refer to another initial slot,
future body result or number of an iteration not yet started.

Iteration `i` executes as follows:

1. Fix the old state of `state_i` and `iteration.index = i`.
2. Compute `with` from `args`, `state_i` and index; form body inputs with ordinary rules
   missing/default/required.
3. Execute a fresh separate graph of `body` until successful completion.
4. Compute `until` in the scope of `args`, `state_i`, index, published `body.outputs` just completed
   body.
5. If `until == true`, successfully finish the loop with the result of this body.
6. If `maxIterations` reached, apply `onLimit`.
7. Otherwise compute all `state.<name>.next` in the same scope of completed body, check values and
   **simultaneously** replace state slots; start iteration `i+1`.

`until` sees the state with which the current iteration was executed, and receives processing
results from `body.outputs`. It does not see partially updated `state`. All `next` see one old
snapshot; their order of writing in YAML does not affect update. `next` is not computed after
successful condition or on the last allowed iteration. Missing value of required `next` is
forbidden. Error of `until`, required `next` or state checks terminates the loop with error.

Each iteration has a separate graph, values and sandbox of its nodes. A file can pass to the next
iteration through a published artifact, state slot and `with`; arbitrary temporary files of previous
environment are not inherited.

Derived outputs of the loop:

- ports with the same name from the last successful body with the same types and requiredness;
- `iterations`: mandatory integer number of successful executions of the body, from `1` to
  `maxIterations`;
- `termination`: mandatory string of `condition` or `limit`.

Therefore the body cannot declare outputs `iterations` and `termination`. Slots of `state` are not
exported automatically: required state should be passed to body output.

When `until == true`, the loop publishes `termination: condition`, even if condition reached on the
last allowed iteration. When the limit is exhausted and `until` is false:

- `onLimit: fail` or absence of `onLimit` — limit error, without loop outputs for parents;
- `onLimit: return_last` — success with outputs of last body, `iterations: maxIterations`,
  `termination: limit`.

`return_last` applies only to the number of iterations. It does not catch body errors, cancellation,
unknown outcome of external action, common deadline, or exhaustion of other limits. If the last
iteration is erroneous, results from earlier iterations do not automatically replace it.

## 16. `pipeline`: calling another document

`pipeline.file` refers to document `Pipeline` in a fixed package. Resolution of the relative path
follows package rules. Contents of all called documents and their files are fixed before run;
loading a changed document over the network during execution is forbidden.

The input object of the child run is `args` of this node. Names must match inputs of the child
pipeline; undeclared are forbidden. Missing optional parent `args` remain missing upon call, after
which own defaults and requiredness of the child graph apply. Values pass both declared contracts.

Outputs of the node are emitted from `spec.outputs` of the child pipeline without renaming, with the
same schemas, artifact kinds, and requiredness. Own `outputs` for `pipeline` is forbidden. If
renaming is needed, a subsequent node or parent graph export sets an ordinary binding.

The child pipeline has its own defaults and resource catalogs. Its powers are limited by mandatory
`pipeline.permissions`, current authority boundary, and EngineProfile. Data transfer does not grant
additional rights. Detailed connection name validation rules are defined [main contract](v1.md).

The child call participates in the common run tree, resource accounting, and cancellation. Its own
limits may further restrict the subtree; they do not reset parent counters. Child error ends parent
node with error; success is published only after successful completion and check of child outputs.

The file call graph is checked before admission. Direct and indirect recursion are forbidden,
including call of the same document via another path chain or condition that is usually false.
Repeated calls of one child document from independent nodes are allowed and have separate instances.
Repetition is expressed `loop` or `foreach` with explicit bounds.

## 17. States, errors, and fail-fast

Normative states of instance:

| State                | Meaning                                                                 |
| -------------------- | ----------------------------------------------------------------------- |
| `pending`            | Dependencies not yet resolved                                           |
| `ready`              | Inputs and condition checked, execution resource expected               |
| `running`            | Attempt executing or active management of nested graph                  |
| `retry_wait`         | Waiting for allowed retry                                               |
| `waiting_human`      | Opened request `human` saved                                            |
| `waiting_resolution` | Must set outcome of external action                                     |
| `succeeded`          | Outputs checked and atomically published                                |
| `skipped`            | Execution not required due to condition or lack of mandatory dependency |
| `failed`             | Error final for instance                                                |
| `cancelled`          | Execution stopped by cancellation                                       |

`pending`, `ready`, `running`, `retry_wait`, `waiting_human` and `waiting_resolution` are not
terminal. `succeeded`, `skipped`, `failed` and `cancelled` are terminal and not rewritten
retroactively. In particular, a successful node remains successful if another graph node later
fails. Retry from terminal state creates separate explicitly requested operation/new run; normal
recovery continues saved state.

v1 uses **global fail-fast**. Final error of any executing node of any nested graph stops planning
of new work of root run, passes cancellation to active nodes and ends run with error after
committing the stopped state. Already started actions may succeed finishing; their outcome is saved
in history. Hidden `continueOnError`, catch-branches or successful partial result does not exist.

Error of attempt with allowed safe retry is not yet final error of node. Skip is not an error.
`coalesce` and optional input do not turn finally erroneous source into missing.

With several nearly simultaneous errors engine saves all detected causes and one cause of run
completion. Order of independent external errors does not determine value of successful result,
because such run does not publish successful outputs of root graph.

## 18. Retries, deadlines, and limits

Effective `execution` is obtained from `spec.defaults.execution` and fields of specific node;
explicitly set node field replaces corresponding default field. Fields `retry` also merge: for
example, node `maxAttempts` may replace global number of attempts, preserving global `backoff`. Then
placement limits and parent run apply. If `retry` is not set at any level, `maxAttempts: 1` is used;
absence of `onUnknownOutcome` means `pause`.

`maxAttempts` includes the first attempt. `backoff` is a fixed pause between attempts; if not
specified in either the node or defaults, `1s` is used. This is not an exponential multiplier.
Backoff does not extend deadlines. For `switch`, `human`, `foreach`, `loop`, and `pipeline` only one
attempt is allowed: repeating the control structure as a whole does not constitute recovery of
nested nodes. If the overall default sets more attempts, these nodes must explicitly override
`maxAttempts: 1`. Policies of individual body nodes continue to apply.

Allowed retry `llm`, `agent`, `code`, or `tool` receives the number of the new attempt and new
temporary results. A fresh attempt `agent`/`code` receives a clean sandbox with the same declared
inputs and package. Files from an unsuccessful attempt are not used implicitly. If a saved
consistent checkpoint exists, the engine may continue the previous attempt; this is a separate
recovery action and does not require re-executing confirmed operations.

`retry.maxAttempts` limits specifically attempts of the node. There are no automatic additional
transport attempts within one attempt of the node; built-in retry SDKs must be disabled. A failure
to send/receive an external operation ends the current attempt or transitions it to resolving an
unknown outcome. A new send is allowed only in the next permitted attempt, taking effect rules into
account. For `tool` all such attempts belong to one logical MCP call with the same arguments and a
stable idempotency key. For `agent` a new model turn after receiving a determined result from an
tool is normal program continuation of the agent, not a transport retry.

`execution.timeout` counts from the node admission after resolving inputs and `when`, including
resource queue, active execution, backoff, nested graphs, and waiting for human/unknown outcome
resolution. This is the overall instance duration, which is not reset by a retry.
`spec.limits.timeout` and the EngineProfile limit are counted from admission of the corresponding
run. The effective deadline is the earliest among applicable ones.

If timeout is absent in the node and defaults, the following apply: `30m` for `llm`, `agent`,
`code`, `tool`; `24h` for `human`; `1m` for `switch`; the remaining duration of the parent run for
`foreach`, `loop`, `pipeline`. Any of these values is limited by an earlier deadline of ancestors. A
local duration does not create permission to continue an already cancelled or error-terminated run.

Accumulated budget costs are monotonic and preserved during recovery. Reservation of an unsent
operation and slot occupancy/release for parallelism are not accumulated costs:

- `maxNodeInstances` limits the total number of materialized instances, including control nodes,
  skips, and instances of nested graphs; retries of one instance do not create a new instance;
- `maxModelCalls` limits physical queries to the model, including retries and queries with unknown
  outcomes;
- `maxToolCalls` limits calls to MCP and sandbox built-in tools, including retries; the initial run
  of command `code` is also counted as one `process.exec`. Service `finish`, artifact transfer, and
  establishing an MCP session are not such calls;
- `maxConcurrentNodes` limits simultaneously active execution attempts; nodes waiting for human
  response and nested graph coordinators do not occupy a slot required by their children.

Before materializing/sending the next work unit, the engine atomically reserves the corresponding
budget. Exhaustion of the overall budget is a final limit error. Subpipeline counters are
additionally accounted within it, but actions also consume ancestor budgets. Pause or network
failure does not return the budget of an already sent request. Only provably unstarted operations
allow atomic release of reservation; lack of worker response is not such proof.

## 19. External Effects and Unknown Outcome

Policy `retry` sets the upper bound on the number of attempts and does not allow duplicating unsafe
external actions. The engine separately classifies an error as retryable and evaluates confirmed
effects of the current attempt. Contract errors, unknown fields/resources, permission violations,
cancellation, and overall limit are not corrected by automatic retry.

Trusted tool policy sets `effect: read`, `write`, or `unknown`. An MCP server annotation cannot
itself increase trust in an operation. Read allows retry on transient error; mutating action
requires proven safe retry, supported idempotency, or confirmation that the action was not
performed. Code and agent commands with arbitrary external capabilities cannot be automatically
considered clean.

At `idempotencyArgument` the engine substitutes a key into the declared JSON Pointer argument before
final input schema validation of the tool. The specified slot belongs to the engine: user arguments
and model do not choose or override this key. One logical call uses the same key in all permitted
attempts of the node `tool`; a new action, another iteration, or another instance receives a new
key. Application of the key implies a trusted guarantee from the external service, not merely the
presence of a string field.

A new agent attempt from a clean state is permitted only until an external tool with effect
`write`/`unknown` or command `process.exec` is executed. After such an action, repeating the entire
attempt is forbidden, including when an idempotency key is present: a new model decision may create
another action. Changes only to own files via `files.write` are not external effects and may be
discarded along with a failed sandbox environment. For `code`, section 10 rule applies: after
process start, automatic repetition of the entire attempt is forbidden regardless of visible program
output.

A consistent checkpoint allows continuing the previous attempt after a confirmed prefix of actions,
preserving original results and files. This does not permit automatically re-sending a request that
ended with an error or unknown outcome. Continuation, new attempt, and resolving the outcome are
distinct operations.

**Unknown outcome** arises when an action may have been accepted by an external system but the
engine did not receive reliable confirmation of completion or absence of effect. Such an action is
considered neither success nor missing value. The engine first uses available verification
mechanisms via stable identifier. Until the outcome is established, automatic sending of a new
unsafe action is forbidden.

- `onUnknownOutcome: pause` preserves state `waiting_resolution`, stops admission of new work for
  the root run, and provides information for establishing the outcome. Already executing independent
  operations may complete; their results are preserved. Deadlines continue to apply.
- `onUnknownOutcome: fail` terminates the node and run with an error indicating unknown outcome;
  this is not a statement that no external action occurred.

Recovery after `pause` must record confirmed resolution and provenance of evidence. If a result is
confirmed, it is checked against the original contract; if absence of execution is confirmed,
continuation/repetition is subject to remaining limits. The YAML contract does not introduce
arbitrary changes to the graph or issuance of new permissions during such a pause.

## 20. Cancellation and Recovery

Cancellation of the root run extends to open child calls, loops, collection elements, human
requests, and active attempts. The engine stops admission of new work, cancels waits, passes
cancellation to supporting providers, and stops sandbox processes. Successfully completed instances
and their published results are preserved.

Cancellation does not roll back external effects, does not delete a document published to an
external system, and does not mean that a remote service has ceased an already accepted request. If
after cancellation the write operation outcome is unknown, this fact remains in history and run
diagnostics. Cancellation of an open human request forbids further acceptance of a response to it.

Publication race with cancellation is resolved by the first committed operation: publication
confirmed before instance cancellation is preserved; after cancellation is committed, successful
outputs of this instance for the graph are not published. A late-arriving external result may be
saved as outcome evidence without transitioning the cancelled instance to `succeeded`.

After restart, the executor restores confirmed states and counters. Saved successful nodes are not
re-executed. Incomplete publication is restored idempotently. For an incomplete external operation,
the outcome is determined or section 19 applies. Presence of a workflow record does not grant right
to assume process memory or sandbox file recovery.

Continuation of the same agent attempt is permitted only with a consistent checkpoint of messages,
completed operations and files. If absent, a new safe attempt per sections 18–19 or stop with
diagnostics is allowed. The executor does not declare recovery successful via silent re-execution of
all actions.
