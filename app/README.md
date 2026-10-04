# Knotra desktop

Tauri 2 application for authoring Knotra pipelines, running them on the Go engine, reviewing human
requests and managing artifacts. React/TypeScript renders the UI; Rust handles native HTTP/SSE,
SQLite persistence, package dialogs and verified binary downloads. Browser preview uses the same
engine API with localStorage.

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

Choose **Settings → Language** (**Настройки → Язык**) to switch between English and Russian. The
initial language follows the browser or webview language: Russian for `ru`/`ru-*`, English
otherwise. The preference is saved with the local workspace and included in workspace backups. Older
workspaces and backups without a language keep their data and use that initial default.

Translation changes interface labels, help, accessibility text, frontend validation messages and
date/size formatting. Pipeline YAML, names, prompts, model output, engine diagnostics and artifact
contents retain their original text.

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

Open **Library → Local Ollama greeting**, or import `examples/local/pipeline.yaml`, choose **Run**,
select profile `local`, and check admission or start. The engine publishes an immutable package,
executes Ollama and Docker, and saves the result. Engine Runs shows the instance list, current
attempt IDs, diagnostics, timeline, run inputs/outputs and immutable package snapshot; Inbox answers
saved human requests; Artifacts uploads, previews and exports registered bytes. The status graph
currently belongs to guided demo runs. A run keeps executing after the app closes.

Other library examples are authoring templates. Their logical models, MCP tools and sandbox names
need a matching EngineProfile. The implemented model adapter is Ollama; other provider names are
rejected during admission. **Guided demo** uses sample data; it does not make model calls. The live
boundary is defined by [the API contract](../docs/api/desktop-v1.md) and
[OpenAPI](../docs/api/desktop-v1.openapi.json).

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
Two extra acceptance tests use a real engine: Ollama output/history replay, and a human review
followed by a child pipeline and binary artifact round-trip. The engine must have the bundled
`local` profile and allow the preview origin:

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
