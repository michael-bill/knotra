#!/usr/bin/env bash
set -euo pipefail

version="${1:?Usage: scripts/package-release.sh VERSION [OUTPUT_DIR]}"
output="${2:-bin/releases}"
if [[ ! "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
  echo 'Version must contain only letters, numbers, dots, underscores and hyphens.' >&2
  exit 1
fi
root="$(cd "$(dirname "$0")/.." && pwd)"
command -v zip >/dev/null || { echo 'Install zip to package Windows bundles.' >&2; exit 1; }
mkdir -p "$output"
output="$(cd "$output" && pwd)"
build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT
cd "$root"

for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -mod=readonly -trimpath \
    -o "$build_dir/sandbox-helper-linux-$arch" ./internal/adapters/sandboxhelper
done

for platform in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 windows-amd64 windows-arm64; do
  os="${platform%-*}"
  arch="${platform#*-}"
  name="knotra-${version}-${platform}"
  bundle="$build_dir/$name"
  mkdir -p "$bundle"
  binary=knotra
  launch=./knotra
  if [[ "$os" == windows ]]; then
    binary=knotra.exe
    launch='.\knotra.exe'
  fi
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -mod=readonly -trimpath \
    -ldflags "-X main.version=$version" -o "$bundle/$binary" ./cmd/knotra
  cp "$build_dir/sandbox-helper-linux-amd64" "$build_dir/sandbox-helper-linux-arm64" "$bundle/"
  cp LICENSE "$bundle/"
  cat > "$bundle/README.txt" <<GUIDE
Knotra CLI bundle ($platform)

Start Docker first. For the default local-model path, start Ollama too.
On Windows, use Docker Desktop with Linux containers (WSL 2 backend).
Then run from this extracted directory:

  $launch doctor
  $launch quickstart

The CLI downloads qwen3.5:9b if needed. Initial downloads may take time.
Go, a source checkout and separately installed PostgreSQL/Temporal are not needed.
Keep this directory intact: both Linux sandbox helpers are shipped beside the CLI.

For a cloud model, select its exact ID and set the key in your environment:

  $launch quickstart --provider openai --model YOUR_MODEL --dir ./openai-data
  $launch quickstart --provider anthropic --model YOUR_MODEL --dir ./anthropic-data

Set OPENAI_API_KEY or ANTHROPIC_API_KEY before the respective cloud command.
Ctrl-C stops the engine. Repeating the same command preserves runs and artifacts.
The bundled Temporal configuration is for development and evaluation.

Project and full instructions: https://github.com/michael-bill/knotra
GUIDE
  if [[ "$os" == windows ]]; then
    rm -f "$output/$name.zip"
    (cd "$build_dir" && zip -Xqr "$output/$name.zip" "$name")
  else
    COPYFILE_DISABLE=1 tar -czf "$output/$name.tar.gz" -C "$build_dir" "$name"
  fi
done

(cd "$output" && shasum -a 256 "knotra-${version}-"*.tar.gz "knotra-${version}-"*.zip > SHA256SUMS)
printf 'Bundles and SHA256SUMS: %s\n' "$output"
