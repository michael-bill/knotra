# Knotra desktop

Tauri 2 desktop app for authoring Knotra workflows, reviewing runs and managing artifacts. Built with React, TypeScript and Rust; Bun manages frontend dependencies. Includes dark/light themes, native package dialogs and local SQLite storage.

## Development

Requires Bun, Rust and the native [Tauri prerequisites](https://v2.tauri.app/start/prerequisites/).

```sh
cd app
bun install --frozen-lockfile
bun run desktop:dev
```

Browser preview: `bun run dev` at `http://127.0.0.1:1420`. Stop it before starting desktop development.

## Engine

Connect an engine in Settings. The client targets the [proposed desktop API](../docs/api/desktop-v1.md) and [OpenAPI schema](../docs/api/desktop-v1.openapi.json). The Go execution engine is not implemented yet; integration tests use a fixture. The guided demo runs locally with sample data.

## Checks and build

Run from `app/`:

```sh
bun run build
bun run test
bunx playwright install chromium
bun run test:e2e
cargo test --manifest-path src-tauri/Cargo.toml
bun run desktop:build
```

On macOS, `bun run desktop:build -- --bundles app` creates `src-tauri/target/release/bundle/macos/Knotra.app`. Signing and notarization are not configured.

Workflow specifications live in [docs](../docs/), [schemas](../schemas/) and [contracts](../contracts/).
