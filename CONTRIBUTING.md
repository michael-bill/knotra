# Contributing to Knotra

Knotra's YAML contract lives in [`docs/notation`](docs/notation/v1.md); the schema and prose
together define behavior. Start by reading the relevant contract and the existing tests before
changing execution semantics. A specification change must update its schema, diagnostics, fixtures,
runtime behavior and documentation together. New syntax must not silently change already persisted
runs.

## Development environment

Use the Go version in `go.mod`. The [running guide](docs/running.md) describes local Docker/Colima,
PostgreSQL, Temporal and Ollama setup. For fast development:

```sh
go mod download
make build
make check
```

`make check` runs formatting checks, `go vet`, unit tests and the race detector. Unit tests may open
loopback listeners. They require no paid model calls, installed Ollama, PostgreSQL or Docker daemon;
integration tests skip unless explicitly enabled.

## Package boundaries

| Package                                | Responsibility                                                                           |
| -------------------------------------- | ---------------------------------------------------------------------------------------- |
| `internal/contract`                    | Package loading, strict parsing, semantic validation, CEL and immutable plan compilation |
| `internal/engine`                      | Deterministic graph execution and Temporal workflow behavior                             |
| `internal/adapters`                    | Model, MCP and isolated command execution behind injected durable hooks                  |
| `internal/store`                       | PostgreSQL transactions, projections, durable operations and artifact metadata           |
| `internal/api`, `internal/protocol`    | Versioned HTTP/SSE boundary and transport DTOs                                           |
| `internal/client`, `internal/cli`      | Client mutation journals, event observation and command presentation                     |
| `internal/app`                         | Dependency wiring and service lifecycle                                                  |
| `app/src/lib/engine`, native `backend` | Engine API, command receipts, SSE and verified bytes                                     |
| `app/src/components`                   | UI grouped by authoring, engine runs, review and artifacts                               |

Keep external I/O out of workflow code and expression evaluation. Pass `context.Context` through
blocking work, bound reads and concurrency, preserve typed errors, and make cleanup ownership clear.
Configuration and credentials are engine concerns; provider responses and pipeline content are
untrusted data. Avoid global mutable state and broad interfaces added for hypothetical future use.

Use ordinary Go conventions: `gofmt`, small cohesive functions, explicit errors, table-driven tests
where they clarify behavior, and comments explaining invariants or non-obvious decisions. Tests
should demonstrate contract behavior or prevent a specific failure, not mirror implementation line
by line. Do not log secrets or provider-hidden reasoning. Never test arbitrary pipeline commands on
the host.

## Readability and client development

Review every touched source file, test and configuration for logical blocks, clear names and bounded
responsibilities. Wrap argument lists and object literals when that makes their structure easier to
follow. Keep SQL, prompts, embedded programs and fixture bytes intact unless the change explicitly
targets them. Generated files, assets and dependency locks are not hand formatted. Negative notation
fixtures may rely on their exact malformed syntax.

Use `gofmt` for Go, the root Prettier configuration for TypeScript/CSS/JSON/YAML and Markdown,
rustfmt for Rust, and Black 25.1.0 for Python. Avoid mixing behavior changes into mechanical edits
without explaining and testing them.

Use Bun 1.3.10 and the Rust version pinned in `rust-toolchain.toml`:

```sh
cd app
bun install --frozen-lockfile
bunx prettier --check .. --ignore-path ../.prettierignore
bun run test
bun run build
bunx playwright install chromium
bun run test:e2e
cargo fmt --manifest-path src-tauri/Cargo.toml -- --check
cargo test --locked --manifest-path src-tauri/Cargo.toml
```

`bun run format` formats app sources. `bunx prettier --write .. --ignore-path ../.prettierignore`
also formats repository documentation and supported configs; `.prettierignore` excludes the notation
fixtures and generated content. Use `black --check scripts examples/integration examples/starter`
for the Python sources. The structural fixture script has reproducible
[setup instructions](contracts/v1/fixtures/README.md).

The desktop CI workflow runs frontend and contract-script checks on Linux and native tests on macOS.
Browser screenshots and failure traces are written to per-test directories in `app/test-results/`;
tests must use Playwright output paths rather than OS-specific temporary paths. Browser acceptance
against a real engine is opt-in. Start an engine with `examples/local/profile.yaml` and the exact
browser origin as described in [app/README](app/README.md), then run from `app/`:

```sh
KNOTRA_E2E_ENDPOINT=http://127.0.0.1:8787 bun run test:e2e
KNOTRA_E2E_ENDPOINT=http://127.0.0.1:8787 \
  cargo test --locked --manifest-path src-tauri/Cargo.toml real_engine -- --ignored
```

## Interface translations

The frontend keeps English and Russian interface text in `app/src/locales/en.json` and
`app/src/locales/ru.json`. Add the same stable semantic key to both catalogs, then render it with
`t('settings.language')` from `useI18n()`. Use named parameters, for example
`t('shell.countPendingReviews', { count })`, and preserve their names in every translation. Enum
label maps refer to these keys while keeping protocol values unchanged. CodeMirror phrase lookups
retain the library's original English tokens and map them to catalog keys.

Only interface text is translated. Render user-defined names, YAML, prompts, model responses, engine
diagnostics and artifact bytes verbatim. The `message()` adapter is reserved for messages from
frontend validators; it must not translate engine or user content. Dates and sizes use the selected
interface locale.

Run frontend unit and browser tests after changing translations. They check catalog coverage,
matching placeholders, legacy workspace compatibility, persistence, accessibility, editor search and
preservation of authoring data, demo history and the engine cache during language switching.

## Integration tests

Start development infrastructure and build sandbox dependencies:

```sh
docker compose up -d --wait
make helper firewall
docker pull python:3.13-alpine
export KNOTRA_TEST_DATABASE_URL='postgres://knotra:knotra-development@127.0.0.1:25432/knotra?sslmode=disable'
export KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper"
export KNOTRA_TEST_WORKDIR="$PWD/.knotra/test-work"
export KNOTRA_TEST_FIREWALL_IMAGE=knotra-firewall:dev
go test -mod=readonly -race -count=1 -timeout=15m ./internal/store ./internal/api ./internal/adapters/...
```

Use a development database. Database tests create isolated schemas; Docker tests remove their own
containers. No blanket database reset or Docker prune is needed. The regular GitHub workflow runs
these persistence, HTTP, sandbox and MCP checks without downloading a model.

To include real local model calls, install `qwen3.5:9b` in Ollama and set:

```sh
export KNOTRA_TEST_OLLAMA=http://127.0.0.1:11434
export KNOTRA_TEST_TEMPORAL=127.0.0.1:27233
export KNOTRA_TEST_REAL=1
go test -mod=readonly -race -count=1 -timeout=30m ./internal/adapters/... ./internal/integration
```

These tests intentionally run actual models and isolated processes. They are separate from default
CI so a contributor does not need model weights to submit a change. Report the exact integration
command and environment used in a pull request.

## Pull requests and compatibility

Keep changes focused. Explain the trigger or problem, resulting behavior, and checks performed.
Include a reproducible example for a bug and a regression test when appropriate. Keep schema errors,
runtime failures and uncertain external outcomes distinct; do not turn an ambiguous write into an
automatic retry.

New adapters must validate capabilities before admission, fix resolved resources in the plan, count
every outbound call, and document retry/idempotency behavior. Persistence changes must preserve
existing histories and deployed data; add explicit migrations instead of resetting tables. Workflow
changes need replay-compatible versioning when they would alter already recorded execution
decisions.

Knotra is MIT licensed. Contributions are made under the same [license](LICENSE). Follow the
[community conduct](CODE_OF_CONDUCT.md) and report vulnerabilities privately according to
[SECURITY](SECURITY.md).

## Cloud adapters and release bundles

Default adapter tests use literal Responses/Messages fixtures, including fragmented SSE, malformed
responses, auth/status handling, tool ID correlation, budgets and operation replay. Real Docker
agent checks use `KNOTRA_TEST_HELPER` and `KNOTRA_TEST_WORKDIR`. The recovery suite includes both
fixture providers against real PostgreSQL/Temporal when `KNOTRA_TEST_RECOVERY=1`; the generation
count must remain one across process restart.

Paid smoke tests require explicit `KNOTRA_TEST_CLOUD=1`, a provider key and
`KNOTRA_TEST_OPENAI_MODEL` or `KNOTRA_TEST_ANTHROPIC_MODEL`. See the
[adapter guide](internal/adapters/README.md); credentials alone never enable paid tests.

`scripts/package-release.sh VERSION` builds four macOS/Linux `.tar.gz` archives and two Windows
`.zip` archives (amd64 and arm64), each containing both Linux helpers, license and startup
instructions. Packaging needs Go, Bash, tar, zip and shasum and can run on macOS or Linux without
Docker. `CGO_ENABLED=0` allows cross-compiling the Windows CLI on a Mac; it does not let macOS
execute the resulting `.exe`.

The Windows CI workflow runs native Go tests, including HTTP over a local named pipe, and CLI smoke
checks on an amd64 Windows runner. ARM64 Windows is cross-compiled during packaging. The Linux-only
sandbox helper is excluded from native Windows tests and built for Linux for every bundle. The Tauri
desktop is built separately and is not included in CLI bundles.

`SHA256SUMS` covers all six archives. A new `v*` tag runs Go checks on Linux and Windows, builds
these bundles and publishes a GitHub release. Publish new commits/tags; never move an existing
release tag to different source. Smoke-test `doctor` from an extracted archive outside the source
checkout before tagging.
