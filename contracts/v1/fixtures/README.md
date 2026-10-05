# Knotra v1 contract fixtures

This suite records expected document acceptance and rejection under the
[v1 contract](../../../docs/notation/v1.md). Machine-readable expectations are in
[manifest.json](manifest.json); the structural schema is
[knotra-v1.schema.json](../../../schemas/knotra-v1.schema.json).

Fixtures check the format implementation. They cover all nine node types, engine profiles, nested
graphs, package files, data schemas, references, permissions and invalid documents. The Go test
`TestContractFixtures` checks parse, structural and semantic expectations for all 39 documents.

## Suite structure

| Directory   | Purpose                                                               |
| ----------- | --------------------------------------------------------------------- |
| `positive/` | Documents that must pass static contract validation                   |
| `negative/` | Documents with an intentional error at the manifest's specified phase |

Documents in `negative/` have two kinds of expectations: some fail JSON Schema validation; others
pass structural validation and must fail semantic validation. For example, dependency cycles and
references from a body to its parent graph require knowledge of relationships and scopes.

Positive documents describe:

- `llm` with prompt and data schema from package files, `$defs`, local `$ref` and `schemaRef`;
- `agent` with a model, MCP, tool permissions and explicit secret delivery to a sandbox;
- `code` with a single output file and a permitted empty argument after the program name;
- input artifact transfer through an explicit `mount`;
- `tool` with a structured MCP response;
- `switch` with conditional branches and `coalesce` to merge their results;
- `human` with a typed response;
- `foreach` with separate body inputs, `iteration.item`, `iteration.index` and a result array;
- `loop` with state, `body.outputs`, an iteration limit and `iterations`/`termination` outputs;
- `pipeline` with a child document and permissions using EngineProfile resource identifiers;
- `EngineProfile` with models, remote and stdio MCP, sandbox, limits and secret sources.

## Manifest

All paths in the manifest are relative to this directory. `packageRoot` determines the file package
root for a particular case. Files listed in `spec.files` belong to the package; additional fixture
materials are not included automatically.

| Field                 | Meaning                                                                      |
| --------------------- | ---------------------------------------------------------------------------- |
| `id`                  | Stable check identifier                                                      |
| `document`            | YAML document to validate                                                    |
| `packageRoot`         | Package root when checking its materials                                     |
| `engineProfile`       | Profile for checking logical connections and permissions                     |
| `expected.parse`      | YAML parsing expectation                                                     |
| `expected.structural` | Document JSON Schema validation expectation                                  |
| `expected.semantic`   | Reference, graph, context, data schema and permission validation expectation |
| `covers`              | Rules checked by the document                                                |
| `reason`              | Reason for intentional rejection                                             |

`accept` means expected acceptance, `reject` expected rejection, and `not_applicable` means the
phase does not run because an earlier phase failed. These are **expectations**, not saved results
from a validation run.

## Running checks

From the repository root, using Python 3.10 or newer:

```sh
python3 -m venv /tmp/knotra-contract-venv
/tmp/knotra-contract-venv/bin/python -m pip install -r scripts/requirements-contract.txt
/tmp/knotra-contract-venv/bin/python scripts/check_contract.py
```

The [validation script](../../../scripts/check_contract.py) reads the manifest, checks the
metaschema, YAML parsing and structural expectations, and checks declared files in positive packages
and external data schema structure. It returns a nonzero exit code on disagreement and explicitly
prints `Semantic: NOT RUN`. This is an auxiliary fixture check, not Knotra's parser implementation:
it does not enforce every normative YAML and packaging restriction.

The primary check is implemented in `internal/contract` and runs from the repository root:

```sh
go test -mod=readonly ./internal/contract -run TestContractFixtures -count=1
```

It includes these phases:

1. Parse YAML under Knotra v1 parser rules.
2. Validate the structural schema itself as JSON Schema Draft 2020-12.
3. Compare each document's structural result with `expected.structural`.
4. Compare the result with `expected.semantic` for structurally valid documents.
5. For package cases, check declared files and child Pipelines in their scope. Compile DataSchema
   separately and validate declared defaults.

YAML parsing and structural expectations were checked with `ruamel.yaml` and `jsonschema` when
preparing the suite. Semantic expectations are checked by the Go compiler. These checks do not
execute CEL, models, MCP, commands or containers and do not prove a working pipeline run.

Positive code fixtures provide the required JSON result through `KNOTRA_OUTPUT_JSON`: programs with
only file outputs write `{}`. The validation script reads commands as data and never runs them.

The profile contains the placeholder model ID `fixture-model`, an address in the reserved `.invalid`
domain and environment variable names without values. External service and secret availability
checks are outside this suite. Separate [integration checks](../../../docs/verification.md) use real
connections. Intentionally invalid fixtures are not automatically formatted: whitespace, types and
syntax may be part of the error being checked.
