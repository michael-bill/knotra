# Knotra desktop

Tauri 2 application for authoring Knotra pipelines, running them on the Go engine, reviewing human
requests and managing artifacts. React/TypeScript renders the UI; Rust handles native HTTP/SSE,
SQLite persistence, package dialogs and verified binary downloads. Browser preview uses the same
engine API with a local browser cache.

## Development

Use Bun 1.3.10, Rust from `../rust-toolchain.toml` and the native
[Tauri prerequisites](https://v2.tauri.app/start/prerequisites/).

```sh
cd app
bun install --frozen-lockfile
bun run desktop:dev
```

Browser preview: `bun run dev` at `http://127.0.0.1:1420`. Stop it before starting native
development, since both use the same Vite port.

## Interface language

Choose **Settings → Language** to switch between English and Russian. The initial language follows
the browser or webview language: Russian for `ru`/`ru-*`, English otherwise. The preference is saved
with the local workspace and included in workspace backups. Older workspaces and backups without a
language keep their data and use that initial default.

Translation changes interface labels, help, accessibility text, frontend validation messages and
date/size formatting. Pipeline YAML, names, prompts, model output, engine diagnostics and artifact
contents retain their original text. Starter titles and descriptions are localized once when a draft
is created; later language changes preserve that draft's content.

The catalogs are [en.json](src/locales/en.json) and [ru.json](src/locales/ru.json). Components refer
to stable keys such as `t('settings.language')`; English phrases are catalog values. See
[the contribution guide](../CONTRIBUTING.md#interface-translations) for adding translations.

## Connect a real engine

Prepare PostgreSQL, Temporal, Docker/helper and Ollama using the
[running guide](../docs/running.md). With the bundled Compose infrastructure, start the engine from
the repository root:

```sh
export KNOTRA_DATABASE_URL='postgres://knotra:knotra-development@127.0.0.1:25432/knotra?sslmode=disable'
bin/knotra serve \
  --profile examples/local/profile.yaml \
  --temporal-address 127.0.0.1:27233 \
  --cors-origin http://127.0.0.1:1420
```

In Settings, save `http://127.0.0.1:8787` and connect. Native requests do not need CORS; browser
preview requires the exact allowed origin above. Remote engines require HTTPS and a token;
credentials are kept separate from draft exports.

Open **Pipelines → Hello, model**, or create a copy through **New pipeline → Ready to run**. Choose
**Run**, select profile `local`, and check admission or start. The engine publishes an immutable
package, executes Ollama and Docker, and saves the result. Engine Runs opens a live graph with step
states, active connections, waiting reasons and elapsed times. Click a block to inspect its
input/output, streaming model response, prompt context, agent iterations and tool calls. Nested
graphs and iterations have their own scopes. The inspector includes token counts, first-token
latency, failures and produced artifacts; the waterfall shows overlapping work. A follow toggle
tracks the active step while the run progresses.

Execution history can be rewound to a recorded state without repeating operations. Run comparison
shows changes to prompts, packages, input/output and step timings. Earlier node activity loads from
the engine independently of the bounded live cache. Large previews are explicitly marked as
truncated; unavailable historical telemetry is not invented. Model-hidden reasoning is not shown.

The instance list, timeline and immutable package remain available. Inbox answers saved human
requests; Artifacts uploads, previews and exports registered bytes. A run keeps executing after the
app closes.

The five [starter scenarios](../examples/starter/README.md) range from a small hello-world check to
independent agents comparing source materials, a generated playable game with independent tests, and
a publication with editorial review and human approval. The research scorer selects the eligible
winner in code; the model explains that decision. The game engine runs fixed tests and carries their
feedback through at most four generation attempts before independent verification. All use
`model_main`, useful default inputs, and no MCP or secrets. Python steps use `python_box`; the game
requires `node_box` and the `node:22-alpine` image. The **Pipelines** landing page lists drafts with
search and sorting; opening a card enters its editor. Upgrades add missing starters and remove only
unchanged obsolete examples; edited drafts and execution history are preserved.

The library's **Building blocks** tab keeps the small notation examples, including the original
local greeting. Their declared model, MCP, sandbox and secret resources need a matching
EngineProfile. The engine supports Ollama, OpenAI Responses and Anthropic Messages. Credentials and
provider parameters belong to the engine profile; the client never stores model API keys. **Guided
demo** uses sample data; it does not make model calls. The live boundary is defined by
[the API contract](../docs/api/desktop-v1.md) and [OpenAPI](../docs/api/desktop-v1.openapi.json).

The **Research, verify and approve** starter pauses with downloadable evidence, keeps its human
request across engine restarts and publishes the reviewed bytes with an approval record. Start a
local engine with `knotra quickstart`; Settings explains how to connect.

## Checks and build

Run from `app/`:

```sh
bun run format:check
bun run test
bun run build
bunx playwright install chromium
bun run test:e2e
cargo fmt --manifest-path src-tauri/Cargo.toml -- --check
cargo test --locked --manifest-path src-tauri/Cargo.toml
bun run desktop:build
```

Default browser tests cover authoring, demos, interface language switching and a contract fixture.
Opt-in acceptance tests use a real engine for streamed Ollama output/history replay, a Qwen agent
using file tools, human review and artifact round-trips, including the five bundled starter
packages. The engine must have the bundled `local` profile and allow the preview origin:

```sh
KNOTRA_E2E_ENDPOINT=http://127.0.0.1:8787 bun run test:e2e
```

Native acceptance against the same engine is opt-in:

```sh
KNOTRA_E2E_ENDPOINT=http://127.0.0.1:8787 \
  cargo test --locked --manifest-path src-tauri/Cargo.toml real_engine -- --ignored
```

These tests create their own definitions/runs/artifacts; use a development engine. See
[verification](../docs/verification.md) for what each test establishes.

On macOS, `bun run desktop:build -- --bundles app` creates
`src-tauri/target/release/bundle/macos/Knotra.app`. Signing and notarization are not configured.
Platform builds require that platform's native toolchain.

The source contract lives in [docs](../docs/), [schemas](../schemas/) and
[contracts](../contracts/). Contribution and compatibility rules are in
[CONTRIBUTING](../CONTRIBUTING.md). Knotra is [MIT licensed](../LICENSE).
