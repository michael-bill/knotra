# Knotra v1: packages, validation and diagnostics

The implementation lives in `internal/contract`, `internal/engine` and `internal/adapters`. Setup is
described in the [running guide](../running.md); verified scenarios and support boundaries are in
the [report](../verification.md).

Status: normative v1 contract, checked against the implementation on 5 October 2026. The structural
JSON Schema and textual semantics apply together. External system requirements are checked at
admission; unsupported providers are rejected before execution.

## 1. Source package

A package has a root directory and one explicitly selected entrypoint YAML Pipeline. The entrypoint
filename is not fixed. It is included automatically; other files are allowed only through
`spec.files`.

Loading computes a transitive closure: the entrypoint Pipeline permits its files; `pipeline.file`
imports must be in the allowed list. After reading an import, its files are added to the manifest.
Every import must have kind Pipeline. All paths, including paths inside imported documents, are
resolved relative to the same package root. Prompt, schema, code and import files must be in this
manifest. Multiple documents may include the same path; two separate archive entries for one path
are prohibited.

The loader accepts only regular files. Symbolic links, hard links, devices, sockets, FIFOs, absolute
paths, root escapes and conflicts between a file and its parent directory are rejected. Names must
also be unique after Unicode NFC and ASCII case folding so the same package cannot change meaning
across filesystems. Path names are not silently normalized: a non-normalized name is an error.
Checking the actual root is required even after JSON Schema validation; regular expressions alone
are insufficient.

Files absent from the manifest are neither transferred nor mounted. There is no implicit inclusion
of the entire working directory, `.env`, home directory or Git contents. Environment variables
inside paths are not substituted. An archive cannot bypass path rules; packaging is transport over
the same manifest.

The manifest has exactly the form
`{entrypoint: Path, files: [{path: Path, size: integer, sha256: string}, ...]}` without additional
fields. `entrypoint` belongs to files; file entries are sorted by the path's UTF-8 bytes. `size` is
the file's byte length; `sha256` is the content hash as 64 lowercase hexadecimal characters.
Snapshot identity is `sha256:` followed by the SHA-256 of the manifest's UTF-8 JCS serialization
(RFC 8785). Sizes are within exact IEEE-754 integers. Comments, spaces and line breaks in a source
file contribute to its hash: changing source bytes or choosing another entrypoint creates a new
snapshot.

The plan separately freezes normalized documents, language/compiler/adapter versions, effective
settings, resolved model IDs, image digests, MCP tool descriptions and schemas, and secret
references. Snapshots contain no secret values. A plan does not make external model responses
deterministic and does not replace the history of performed actions.

## 2. YAML and JSON profile

- One nonempty UTF-8 document using YAML 1.2.2 Core Schema with the additional constraints below.
  YAML 1.1 is unsupported; `yes`, `no`, `on`, `off` remain strings.
- Only a JSON-compatible value tree. All object keys are strings; duplicate keys are prohibited at
  every depth before conversion into an ordinary map.
- YAML anchors, aliases, merge keys `<<`, custom and explicit tags are prohibited. The only allowed
  directive is `%YAML 1.2`. Multiple documents are prohibited even if later ones are empty.
- Unquoted dates do not become timestamps: Core Schema treats them as strings. If a library does
  otherwise, the loader must configure it appropriately or reject the document.
- Numbers permit only JSON lexical forms: `-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`. YAML
  hex/octal, `.inf`, `.nan`, leading `+` and numbers containing `_` are prohibited as numeric
  literals. Quote such text to use it as a string.
- Integer tokens without fractional/exponent parts must fit signed int64. Fractional/exponent tokens
  become finite float64 with IEEE-754 rounding; overflow and nonzero values becoming zero through
  underflow are rejected. JSON Schema `integer` accepts mathematically integral values regardless of
  their spelling, as the standard requires.
- CEL receives integer tokens as `int` and fractional/exponent tokens as `double`; conversion rules
  do not depend on the YAML library. Subsequent runtime serialization preserves this type choice;
  storage must distinguish `1` from `1.0` even though both satisfy JSON Schema integer.
- Strings are preserved after standard YAML decoding; textual templates are not evaluated. Unpaired
  UTF-16 surrogates and invalid UTF-8 are prohibited.

User JSON API data and node results follow the same numeric, duplicate-key and Unicode constraints.
Schema files may be JSON or YAML, selected by `.json`, `.yaml`, `.yml` extension; other schema
extensions are rejected. A JSON file contains exactly one DataSchema value.

## 3. v1 size limits

The following values are mandatory maxima. Exceeding a limit fails before the corresponding
computation. A deployment may impose stricter resource quotas and announce them to the client, but
cannot accept values exceeding these maxima under the same v1 profile.

| Object                                            | Limit                                            |
| ------------------------------------------------- | ------------------------------------------------ |
| One YAML Pipeline/EngineProfile                   | 8 MiB UTF-8                                      |
| Entire unpacked package                           | 64 MiB, 512 files including the entrypoint       |
| One prompt file                                   | 1 MiB                                            |
| One standalone DataSchema                         | 256 KiB in compact JSON                          |
| YAML/JSON, DataSchema or Binding tree depth       | 64 container levels                              |
| Nested Graph and import depth                     | 32                                               |
| Total static Node declarations in package closure | 10000                                            |
| One CEL expression                                | 8192 UTF-8 bytes, 4096 AST nodes                 |
| One CEL expression evaluation                     | 100000 CEL cost units, defined below             |
| One node or Graph JSON input/output object        | 16 MiB in compact JSON, excluding artifact bytes |
| One Duration                                      | At most 365 days                                 |

Dynamic instances, calls, processes, sandbox size and time are additionally bounded by
EngineProfile. An individual artifact's size is bounded by available sandbox disk and the published
storage quota; artifact bytes are not automatically inserted into model JSON context.

## 4. CEL profile

Expressions use CEL semantics: immutable values, no I/O and no evaluation of user code.
JSON-representable `null`, `bool`, `int`, `double`, `string`, lists and maps with string keys are
supported. Artifact handles are not CEL maps. An artifact value is passed through Binding.from,
including iteration.item, rather than through an expression.

Allowed:

- Literals of the listed types, data field/index access, `in`, `size`.
- CEL arithmetic, comparison and logical operators, and the ternary operator.
- Standard `int`, `double`, `string`, `bool` conversions in overloads accepted by CEL; `type`.
- String `contains`, `startsWith`, `endsWith`, `matches`.
- Standard macros `has`, `all`, `exists`, `exists_one`, `map`, `filter`.

`uint`, `bytes`, CEL timestamp/duration values, optional types, proto objects, string/list/set/math
extensions, custom functions, randomness, current time, network, files and env are unavailable.
Knotra Duration fields are a separate lexical type, not a CEL expression. Regular expressions in
`matches` follow RE2. Numbers are checked for overflow/non-finiteness; output maps with non-string
keys are rejected. Boolean conditions must return bool; there is no truthiness or
string-to-condition conversion.

The complete overload set is fixed by the saved checked AST and CEL implementation version in the
plan. Library extensions cannot be enabled merely because the installed package contains them.
Values with a user JSON Schema may be checked as dyn when their precise CEL type cannot be inferred;
this does not remove actual-value validation at port boundaries. Full static compatibility of
arbitrary JSON Schemas cannot be promised.

Map traversal in macros is deterministic: string keys are sorted lexicographically by UTF-8 bytes.
Allowed temporary CEL maps with heterogeneous keys are traversed first by type (`bool`, `int`,
`string`), then by value; `false` precedes `true`, integers compare numerically. A dynamic key of
another type is rejected when the map is created even if the expression never reads it. Output JSON
maps still require string keys. List element order is preserved. Sorting does not change CEL's
standard cost table; JSON-value and AST limits bound map size, and standard cost accounting counts
the iterations. Saved plans and replay use the same order.

Static checking knows the scopes from the [main contract](v1.md). In namespaces `nodes`, `inputs`,
`args`, `state`, `body.outputs`, the first identifier must be statically known. Access to an entire
control namespace as a value, iteration over nodes and computed port names are prohibited. Dynamic
indexing is allowed only beneath a declared JSON port. An absent optional `args` can be checked with
`has(args.x)`; an absent referenced graph source is handled before CEL according to missing rules
and cannot be hidden by has.

Evaluation has a CEL cost limit, not just a wall-clock limit. The first implementation uses standard
cel-go runtime cost accounting without custom cost overrides, with a limit of 100000. The library
version is fixed in the engine and plan; changing cost tables requires a compatibility check before
upgrading. Exceeding the limit produces `EXPRESSION_LIMIT`, not false or missing. v1 does not
promise identical cost-unit counts for arbitrary alternative CEL implementations: they must
reproduce the selected cost model or declare incompatibility.

## 5. DataSchema validation

Each standalone DataSchema is first checked against the allowed subset, then compiled using Draft
2020-12. `$ref` resolves relative to its own root: an inline port schema cannot refer to a
neighboring port, and schemaRef is not a JSON Pointer. All local targets must exist and be schemas;
reference cycles are rejected. Input port default values are validated during preparation.

For `pattern` and `patternProperties` keys, the common RE2 and ECMA-262 syntax is allowed: literals,
`.`, classes and ranges, groups without special flags, alternation, `* + ? {m,n}`, anchors `^ $`,
escaped metacharacters, `\d \D \s \S \w \W`, `\t \r \n`. Backreferences, lookaround, named groups,
inline flags, Unicode properties and mode-dependent extensions are prohibited. Matching is not
implicitly anchored; `\d`/`\w` use ASCII, `\s` uses ASCII whitespace; `.` excludes LF and `$`
matches only the end of the string, not before a trailing LF. This explicitly narrows the data
profile for consistent implementation in Go and other environments. The validating engine must apply
these rules separately from its library when regexp defaults differ.

Property names, required and every other constraint are checked using JSON Schema semantics.
Contradictory constraints that accept no values do not themselves make a schema syntactically
invalid: boolean false is valid. An incompatible default is rejected, however, and a provably
impossible binding between two ports is diagnosed before execution. Uncertain compatibility is not
proven compatibility; actual values are always checked.

## 6. Validation sequence

| Phase        | Required checks                                                                                                   | When                            |
| ------------ | ----------------------------------------------------------------------------------------------------------------- | ------------------------------- |
| `parse`      | Sizes, encoding, single document, YAML profile, keys and numbers                                                  | When reading each file          |
| `structural` | Knotra JSON Schema: fields, types, discriminators, required fields, Binding forms                                 | Before resource resolution      |
| `package`    | Manifest, path validity, prompt/schema/import files, file types, import cycle/depth                               | Locally, without external calls |
| `semantic`   | Data schemas, defaults, scope, references, derived ports, DAG, CEL, type-specific rules, retry/defaults           | After package loading           |
| `admission`  | Profile, adapter/tool/sandbox capabilities, permissions, limits, image/credential availability, freezing the plan | On the engine before starting   |
| `input`      | Input values, defaults, JSON schemas, registered handles                                                          | Before starting a Graph/node    |
| `runtime`    | Results, effects, budgets, timeout, cancellation, artifacts                                                       | During execution                |

Offline validation reports which admission checks remain unperformed. An absent engine or connection
cannot become fictitious confirmation that execution is possible. Admission discovery may establish
connections and read tool catalogs, but cannot call user tools merely to check them.

### Minimum semantic rules

1. All local/canonical resources and schemaRef names exist. Access is checked for every declaration,
   including branches that will not execute.
2. All port references exist, including switch/foreach/loop/pipeline derived outputs. Invalid scopes
   are rejected.
3. `needs` dependencies and all static `nodes` reads form a DAG in each scope; self-references and
   cycles are prohibited. Graph exports do not create backward dependencies for their nodes.
4. CEL is parsed and checked in its exact context; conditions return boolean. Lazy expression
   branches cannot hide graph dependencies.
5. JSON and artifacts are not mixed. Artifact bindings permit only from/coalesce; path on an
   artifact is prohibited. llm does not accept artifacts. mount is permitted only on agent/code and
   required for their artifact inputs; paths do not overlap.
6. foreach.over refers to an array/collection; body exports are required and artifact exports are
   single files. with matches body.inputs; bindings/defaults cover required inputs.
7. loop.state.initial and next satisfy state port contracts; the body does not export
   iterations/termination. Unknown state names, invalid initial/next scopes and missing next values
   are errors, not state resets.
8. switch cases and default have unique names; defaults do not create implicit branches.
9. Subpipelines do not recurse; inputs/outputs are derived from the child. permissions use canonical
   names; every requested permission and capability fits ancestor constraints.
10. Effective retry.maxAttempts is 1 for switch/human/foreach/loop/pipeline. Node type and external
    effects constrain retries independently of this numeric field.
11. Env does not override KNOTRA\_\*; argv contains no NUL. collect is a single file and its MIME
    type is allowed; foreach builds collections.
12. URLs, HTTP headers, host allowlists, secrets, capabilities, idempotencyArgument and provider
    parameters satisfy the [profile contract](engine-profile.md).

## 7. Diagnostics

Every error has a stable `code`, `phase`, human-readable `message`, and logical `path` in JSON
Pointer format. For source errors, `file` (package path), `line` and `column` (starting at 1) are
required. A missing field points to the containing object; EOF points to the EOF position. `related`
is an optional list of additional locations, such as a second key or cycle edge. During execution,
run/node instance/attempt/operation IDs are added when they exist. Error text and context contain no
secret values.

The following are base codes. An adapter may add `details` with its own machine code without
replacing the base meaning:

| Codes                                                                                 | Meaning                                                                |
| ------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `YAML_INVALID`, `DUPLICATE_KEY`, `DOCUMENT_LIMIT`                                     | Parsing error or document limit                                        |
| `SCHEMA_INVALID`, `VERSION_UNSUPPORTED`, `FEATURE_UNSUPPORTED`                        | Unsupported structure, version or declared feature                     |
| `PACKAGE_INVALID`, `IMPORT_CYCLE`                                                     | Files, paths, manifest or import                                       |
| `REFERENCE_INVALID`, `SCOPE_INVALID`, `GRAPH_CYCLE`                                   | Name/scope/dependencies                                                |
| `DATA_SCHEMA_INVALID`, `DEFAULT_INVALID`, `TYPE_MISMATCH`                             | Data schema, default or proven type conflict                           |
| `EXPRESSION_INVALID`, `EXPRESSION_FAILED`, `EXPRESSION_LIMIT`                         | CEL parsing/type checking, evaluation or cost                          |
| `CONFIG_INVALID`, `PERMISSION_DENIED`, `CAPABILITY_UNSUPPORTED`, `SECRET_UNAVAILABLE` | Profile, permissions, capabilities and credentials                     |
| `INPUT_INVALID`, `INPUT_UNAVAILABLE`, `OUTPUT_INVALID`, `OUTPUT_UNAVAILABLE`          | Data at Graph/node boundaries                                          |
| `MODEL_FAILED`, `TOOL_FAILED`, `PROCESS_FAILED`, `ARTIFACT_INVALID`                   | Executor/result/file                                                   |
| `TIMEOUT`, `CANCELLED`, `BUDGET_EXCEEDED`, `STEP_LIMIT`, `ITERATION_LIMIT`            | Execution stop                                                         |
| `OUTCOME_UNKNOWN`                                                                     | An external operation may have executed, but no reliable result exists |

A required node input missing because its source was skipped causes a normal skip with reason
`dependency_missing`, not `INPUT_UNAVAILABLE`. The latter applies when a required Graph/run or loop
state cannot start. Skip reasons and error codes are distinct.

A validator may collect several independent errors. It does not perform dependent checks on known
damaged objects or obscure the root cause with a cascade of unrelated errors. Response format and
HTTP status are described in the [API](../api/desktop-v1.md); CLI exit codes are in the
[CLI reference](../../internal/cli/README.md).

## 8. Execution acceptance scenarios

This table defines execution expectations checked by compiler, workflow and integration tests. JSON
Schema does not execute them. Reproducible commands and boundaries of real checks are in the
[report](../verification.md).

| Scenario                                                 | Expected behavior                                                                |
| -------------------------------------------------------- | -------------------------------------------------------------------------------- |
| Required input with default absent / explicitly null     | Insert default in the first case; validate null against the schema in the second |
| when=false → consumer of a required output               | Source and then consumer are skipped; no external call                           |
| coalesce of two branches, first missing, second null     | Result is null after sources finish                                              |
| Later coalesce branch fails, first already returned data | Run fails; coalesce does not hide the error                                      |
| Two foreach items finish in reverse order                | Result array preserves original order                                            |
| Empty foreach                                            | Empty arrays/collections for all derived ports; body does not start              |
| loop.until=true on first iteration                       | iterations=1, termination=condition; next is not evaluated                       |
| until=false at maxIterations with return_last            | Last body is published, termination=limit; next is not evaluated                 |
| Two loop.state.next bindings read different old values   | Both see old state; replacement is then atomic                                   |
| Subpipeline attempts to reset model/tool budgets         | Operations also count toward parent counters                                     |
| Code exits with 0 but file is absent or JSON invalid     | OUTPUT_INVALID/ARTIFACT_INVALID; no partial success                              |
| Agent calls finish on last allowed turn                  | Valid result is accepted; other tools no longer execute                          |
| Retry of a write tool without reliable idempotency       | Pause/failure after unknown outcome, not blind retry                             |
| Worker loss after successful publication                 | Confirmed node does not execute again                                            |
| Cancellation concurrent with a late result               | First committed operation wins under execution rules                             |
| Model secret specified in a connection                   | Available to the trusted adapter, not agent env or plan snapshot                 |

## 9. Implementation boundaries and code clarity

Implementation changes preserve the sequence: source loader with positions → structural validator →
reference/CEL/graph compiler → immutable plan → runtime. Defaults normalize once and are not mixed
with scheduling. Common ports, Binding, scopes and errors are implemented consistently for every
node type.

Model, MCP, sandbox and storage adapters do not independently decide skip, retry, limit or
publication rules. Runtime uses a typed plan rather than reading arbitrary YAML fields at every
step. New capabilities must not require implicit deep merges or hidden global variables. Tests check
observable behavior and invariants instead of reproducing internal functions for coverage.

Go fixture tests check parse, structural and semantic expectations for all 39 documents. Runtime
checks live in `internal/engine`, `internal/adapters`, `internal/api` and `internal/integration`.
The auxiliary Python script checks only structure and explicitly prints `Semantic: NOT RUN`.

## 10. References

- [YAML 1.2.2](https://yaml.org/spec/1.2.2/): base syntax and Core Schema.
- [JSON Schema Draft 2020-12](https://json-schema.org/draft/2020-12/json-schema-core): structure and
  local reference resolution.
- [CEL language definition](https://github.com/cel-expr/cel-spec/blob/master/doc/langdef.md):
  expressions; [cel-go](https://github.com/google/cel-go): selected implementation and cost model.
- [RFC 6901](https://www.rfc-editor.org/rfc/rfc6901): JSON Pointer.
- [RFC 8785](https://www.rfc-editor.org/rfc/rfc8785): manifest canonicalization for hashing.

The restrictions of these standards listed above are part of the Knotra contract.
