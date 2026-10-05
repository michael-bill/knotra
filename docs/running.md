# Running the engine and CLI

The engine is started via a native Go binary and connects to PostgreSQL, Temporal, the API of the
selected model, and Docker. The Compose setup below starts separate infrastructure for development;
the engine itself remains on the host to use local Ollama and Docker/Colima.

## Quickstart from a CLI archive

You need a working Docker Engine, Docker Compose, and Ollama. Download the archive for your OS and
architecture from [Releases](https://github.com/michael-bill/knotra/releases/latest), unpack it, and
keep both Linux helpers next to `knotra`. Then run `./knotra doctor` and `./knotra quickstart`.
Doctor checks dependencies and helper architecture without starting infrastructure. Quickstart
starts PostgreSQL and Temporal with persistent volumes, loads required images and model, starts the
engine, and downloads the first `greeting.txt`. It stays in the terminal until Ctrl-C.

The default directory is `knotra/quickstart` inside the user's system settings directory. The
command prints the exact path. An explicit directory is more convenient for recovery:

```sh
./knotra quickstart --dir "$PWD/knotra-data"
```

Repeat the command with the same `--dir`, `--provider`, `--model`, and ports. Settings, DB password,
first operation ID, profile, and examples are written once; your edits are not overwritten. Services
and volumes remain after Ctrl-C. For another profile use a separate directory and free
`--postgres-port`, `--temporal-port`, `--temporal-ui-port`, `--listen`. `--no-run` starts the engine
without greeting. `--cors-origin http://127.0.0.1:1420` is needed for preview. Engine data lies in
`engine/`, logs in `engine.log`; keep this directory and both volumes together.

The persistent directory must be accessible to Docker. On macOS, choose a path shared with Colima or
Docker Desktop. Quickstart copies the verified sandbox helper into this directory so execution does
not depend on Docker sharing the archive's extraction directory.

For a cloud model specify an ID available to your account. Set the key in the terminal environment;
it is not written to the profile. For example, after setting `OPENAI_API_KEY`:

```sh
./knotra doctor --provider openai --model YOUR_MODEL --dir ./openai-data
./knotra quickstart --provider openai --model YOUR_MODEL --dir ./openai-data
```

For Anthropic use `--provider anthropic` and `ANTHROPIC_API_KEY`. Quickstart with a cloud provider
performs a paid model request for greeting; `--no-run` disables it. Support for a specific model and
parameters depends on its API. These instructions do not promise production Temporal setup or signed
desktop build.

## OpenAI and Anthropic Connections

In your `EngineProfile` replace only the `model_main` connection, keeping sandboxes and limits:

```yaml
secrets:
  model_key: { env: OPENAI_API_KEY }
models:
  model_main:
    provider: openai
    model: YOUR_MODEL
    auth: { key: { secretRef: model_key } }
    parameters: { max_output_tokens: 4096 }
```

This fragment is inside `spec`. For Anthropic replace env with `ANTHROPIC_API_KEY`, provider with
`anthropic`, and parameter with `max_tokens`. Default addresses: `https://api.openai.com/v1` and
`https://api.anthropic.com/v1`. Do not copy Ollama parameters `think`/`num_predict` into a cloud
profile. [Full list and limits](../internal/adapters/README.md).

Admission checks access to the selected model ID and records the allowed identifier. Cloud catalogs
do not provide verifiable weight digest or common capability contract: text/tools means protocol
support by the adapter. The model must support function calling; its parameters and actual responses
are checked by the API. `imageInput` is currently rejected by cloud adapters.

## Preparation

You need the Go version from `go.mod`, Docker Engine, and Make. On macOS Docker can run through
Colima. The local examples use the installed Ollama model `qwen3.5:9b`.

```sh
go version
docker info
ollama list
make build helper firewall
docker pull python:3.13-alpine
docker pull node:22-alpine
```

`make helper` defines the Docker daemon architecture. If unavailable during build, set
`HELPER_ARCH=arm64` or `HELPER_ARCH=amd64` explicitly. The CLI binary appears in `bin/knotra`,
Linux-helper in `.knotra/bin/sandbox-helper`. A sandbox profile may choose another image: helper
does not require Python or shell, but the pipeline program itself must be present in the chosen
image.

## Variant A: Existing PostgreSQL and Temporal

Use a separate Knotra database, not an application's foreign database. Tables are created by the
engine at startup. Temporal namespace must already exist.

```sh
export KNOTRA_DATABASE_URL='postgres://127.0.0.1:5432/knotra?sslmode=disable'
bin/knotra serve \
  --profile examples/local/profile.yaml \
  --temporal-address 127.0.0.1:7233
```

PostgreSQL user and password are defined by your installation. Model tokens and MCP are set via
engine process environment variables referenced by `EngineProfile`; the client does not receive
them. A local profile example does not require an Ollama token. The chosen Docker context is used
automatically, or you can set `DOCKER_HOST` / `--docker-host` with a URL of a local Unix socket.

## Variant B: Infrastructure in Compose

```sh
docker compose up -d --wait
export KNOTRA_DATABASE_URL='postgres://knotra:knotra-development@127.0.0.1:25432/knotra?sslmode=disable'
bin/knotra serve \
  --profile examples/local/profile.yaml \
  --temporal-address 127.0.0.1:27233
```

If Homebrew installed a separate `docker-compose`, use this name instead of `docker compose`; file
format is identical. Compose publishes ports only on loopback: PostgreSQL — `25432`, Temporal gRPC —
`27233`, Temporal UI — `28233`. Variables `KNOTRA_PG_PORT`, `KNOTRA_TEMPORAL_PORT`,
`KNOTRA_TEMPORAL_UI_PORT` override them. `KNOTRA_DEV_DB_PASSWORD` changes password on first database
creation; update the connection string accordingly.

Compose uses pinned digest of official PostgreSQL 18.6 and Temporal CLI 1.9.1. PostgreSQL volume is
mounted in `/var/lib/postgresql`, as required by official PostgreSQL 18 image.
[Image documentation](https://hub.docker.com/_/postgres)

Temporal here is **development server with SQLite persistence in named volume**. It saves history
between container restarts but is not intended for production. Mode properties and `--db-filename`
are described in [Temporal documentation](https://docs.temporal.io/cli/command-reference/server).

```sh
docker compose stop
docker compose start
```

Stop preserves data. `docker compose down` removes containers, keeping named volumes.
`docker compose down --volumes` also removes history and database; this command is needed only for
intentional development environment reset.

## First Run

In another terminal, from repository root:

```sh
bin/knotra validate examples/starter/hello/pipeline.yaml
bin/knotra validate examples/starter/hello/pipeline.yaml --remote --profile local
bin/knotra run examples/starter/hello/pipeline.yaml --profile local --wait
bin/knotra runs list
```

The model creates a greeting for login `name`, the code node writes it to `greeting.txt`, the engine
publishes an artifact. This small scenario checks connection and result saving. Use the ID from the
result:

```sh
bin/knotra runs get RUN_ID --json
bin/knotra artifacts download ARTIFACT_ID --output greeting.txt
```

CLI checks size and SHA-256 before writing the file. `--force` allows replacing an already existing
file. `--json` is intended for scripts; the command reference is available via `bin/knotra --help`
and in [CLI documentation](../internal/cli/README.md).

Other [starter workflows](../examples/starter/README.md) compare three offers with independent
agents and deterministic scoring, build a playable game with fixed tests, and demonstrate editorial
review and research approval. All five use profile `local` and `model_main`; Python steps need
`python_box`, the game needs `node_box` with image `node:22-alpine`. MCP servers and secrets are not
needed.

For `human`-nodes:

```sh
bin/knotra requests list --run RUN_ID --all --json
bin/knotra requests respond REQUEST_ID --outputs response.json
bin/knotra runs watch RUN_ID
```

The response is addressed to a specific request ID. If the connection is lost after sending the
command, `operations list` will show its saved ID, and `operations retry ID` will re-verify the
original command with the previous idempotency key.

## Connecting desktop and browser preview

The engine by default listens on `http://127.0.0.1:8787`. Run the application via
[desktop README](../app/README.md), specify this address in Settings, and connect. On the
**Pipelines** page select **Hello, model**, **Research dossier from source materials**, **Build a
playable game**, **Research, verify and approve**, or **From brief to reviewed publication**. Create
a new copy from the starter workflows library; select profile `local` when running it. The library
also contains individual node examples and a guided demo with sample results. On submission, the
engine checks admission and records the package; editing the draft does not change an already
accepted run.

For browser preview add at engine startup:

```sh
bin/knotra serve \
  --profile examples/local/profile.yaml \
  --temporal-address 127.0.0.1:27233 \
  --cors-origin http://127.0.0.1:1420
```

This address must exactly match the browser origin. Native desktop uses a Rust HTTP client and does
not require CORS. Both clients read the same API and SSE; human requests are available in Inbox,
verified files — in Artifacts. Closing the application does not cancel the run. The built-in guided
demo uses local data samples.

## Data, recovery, and deployment boundaries

PostgreSQL stores definitions, accepted runs, projections, requests to humans, outbox, and logs of
external operations. Temporal stores execution history. Catalog `--data-dir` stores artifact files
and offloaded payload data from Temporal, as well as sandbox temporary materials. Recovery requires
**all three storages**. A single PostgreSQL copy is insufficient. Do not delete `.knotra` while
history is needed.

The current release is one API/worker process on one host. If the worker moves, it needs the same
saved data, profile references, and compatible helpers/images. Multiple hosts with independent local
`data-dir` do not form a correct shared engine. A shared PostgreSQL alone does not solve this
problem. For production, separately prepared Temporal, PostgreSQL, file backups, and a chosen shared
storage model are required; Compose is not a production recipe.

After engine restart, completed external responses are read from the log. If the record outcome is
unknown or the working environment of an unfinished agent is lost, the engine requires explicit
state resolution instead of re-executing the command. `runs resolve` accepts proven outcomes and
verified outputs; `resume` does not bypass this check.

Containers have their own watchdog: after an abnormal termination of the engine process, heartbeat
stoppage stops them approximately 45 seconds later. Agent/code are additionally limited by attempt
deadline. Upon next startup the engine cleans remaining containers and temporary directories only
for its own engine ID.

The engine implements **Ollama**, **OpenAI Responses** and **Anthropic Messages**. An unknown
`provider` is rejected at admission. MCP supports Streamable HTTP and stdio within an isolated
container. Details and limits of the Docker backend, including network filtering by fixed IP, are
described in [adapter documentation](../internal/adapters/README.md).

For listening to the external interface, the engine requires `KNOTRA_TOKEN`, `--tls-cert`, and
`--tls-key`. Remote CLI requires HTTPS and the same token. `--cors-origin` allows exactly one
browser origin. The current bearer token sets one trusted principal; this is not a multi-user RBAC
system.

## Observability and operational limits

Server logs are JSON in stderr. `--debug` includes additional messages. HTTP request traces and node
attempts, HTTP request counter and their duration can be sent via OTLP/HTTP by setting
`OTEL_EXPORTER_OTLP_ENDPOINT` or separate `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` /
`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. Without an explicit endpoint export is disabled.
`OTEL_SDK_DISABLED=true` disables it. Prompts, request bodies, input values, and credentials are not
included in attributes.

One engine ID is served by one process: ownership is secured by a separate PostgreSQL connection.
This is a deployment limitation, not parallel run capability. The engine simultaneously executes
multiple pipelines and multiple instances of one YAML with independent limits and sandbox. The
Temporal task queue name is automatically appended with the engine ID so that different engine
databases do not pick up each other's tasks.

Lists have cursors and are limited by record count and size. CLI flag `--all` bypasses all pages.
Input artifacts are visible after upload; those created by a node — after verification and
publication of its outputs. For resolving an unknown outcome, artifact output is set by a registered
ID (or an array of IDs for a collection).

Storage files and history are currently saved without automatic cleanup. The operator ensures free
space and backups; deleting files manually when saves are in progress is not allowed. The maximum
size of a single artifact is 64 MiB. Network `allowlist` Docker filters pinned addresses, not HTTP
Host/SNI: access to an allowed external proxy implies access to its application functions.

`resources` shows resources available according to installed profiles and issued tools; the absence
of a required secret is marked as `unavailable`. This is the configuration catalog. The current
connection to the service, capabilities of a specific model, MCP schemas, and image availability are
checked by command `validate --remote` and upon acceptance of each run.
