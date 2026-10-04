# Execution adapters

`Runner` executes one compiled leaf node. The graph scheduler remains in `engine`; transactional
budgets, artifacts and operation journals are supplied through `Hooks`. Use `WithHooks` for a
request-scoped journal/budget binding while retaining run-scoped MCP sessions. Call `ReleaseRun`
after a run becomes terminal and `Close` on server shutdown.

The initial model adapter is Ollama's native `/api/chat` API. Admission checks installed models and
capabilities, and stores model digests. A changed model digest is rejected before the next
generation request. Supported parameters are `temperature`, `top_k`, `top_p`, `min_p`, `seed`,
`num_predict`, `num_ctx`, `repeat_penalty`, `repeat_last_n`, `stop`, `think`, and `keep_alive`.
Prompts and typed input envelopes are separate messages. Provider tool names map unambiguously to
grants; `knotra_finish` is reserved for the runtime.

Ollama replies are streamed. Visible text is coalesced every 100 ms or 1 KiB, with an
immediate first fragment and a final flush. The adapter also accepts a single complete JSON
response. Content, tool calls and provider-private context are assembled before the existing
operation journal commits the complete response. An incomplete or malformed stream cannot
publish a successful completion; replaying a recorded response never starts another generation.

Hooks may implement `ExecutionObserver` to receive `model.started`, `model.delta`,
`model.completed`, `model.failed`, `agent.iteration`, `tool.started`, `tool.completed`, `output.validating`,
and `output.completed`. Events carry one-based steps and operation identities, plus bounded
previews, model timing and token counts when supplied by the provider. Missing telemetry cannot
change execution decisions or retry side effects; later events mark `observationIncomplete`.
Each execution delivers observations through a 64-event FIFO with nonblocking enqueue and a
250 ms write deadline. Overflow or failed writes mark later events as incomplete. After executable
work and operation journaling finish, delivery drains for at most 250 ms (50 ms when already
cancelled), then stops. Observer implementations must honor the supplied context deadline.
Previews are limited to 48 KiB per event and 16 KiB per string, with explicit truncation markers.
Numbers outside the clients' supported range are shown as explicit text markers containing their
original JSON spelling; they cannot invalidate the surrounding event stream or history page.
They redact resolved engine credentials and host sandbox paths, and omit
provider-hidden reasoning. Redaction spans fragment boundaries. The complete operation response
remains internal so the next model turn can receive the context required by the
[Ollama streaming tool protocol](https://docs.ollama.com/capabilities/tool-calling#tool-calling-with-streaming).

MCP uses the official Go SDK, with transport reconnection disabled. Both Streamable HTTP and
sandboxed stdio are supported. Admission snapshots schemas, and runtime validates arguments and
declared structured results. An idempotency field must be explicitly describable through
`properties`; schemas whose conditionals or references prevent safe projection are rejected. No
local host command is used as an MCP server. The Docker CLI only attaches to its already isolated
process.

## Agent cycle

An agent attempt owns one sandbox for its entire conversation. The runtime sends instructions, the
prompt, typed inputs and granted tool definitions to the model. It validates each requested tool and
its arguments, executes calls sequentially, appends their results to the conversation, and calls the
model again. Files created by earlier steps remain available within that attempt. Invalid tool
arguments are returned as feedback; a request for an ungranted tool fails the attempt.

Only a solitary `knotra_finish` call can complete the node. The runtime validates its JSON outputs
and collects the declared artifact paths before publishing success. Invalid completion receives
feedback while steps remain; a plain text answer is prompted to finish explicitly. `maxSteps` bounds
model turns, including completion, while the node deadline and enclosing model/tool budgets also
apply.

The full attempt is one Temporal activity. Individual external calls and the final validated result
use durable operation journals. A completed attempt can return its recorded outputs; an interrupted
attempt with a lost workspace cannot resume the in-memory conversation or automatically repeat its
effects. See recovery below. The real integration scenario exercises file reads, MCP, file writes,
Python execution and completion with three concurrent agents and independent artifacts.

## Docker backend

Build the helper for the daemon's architecture, e.g. on an Apple Silicon Colima VM:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o .knotra/bin/sandbox-helper ./internal/adapters/sandboxhelper
docker build -t knotra-firewall:dev internal/adapters/firewall
docker pull python:3.13-alpine
```

The usual repository setup is `make helper firewall`; it detects the Docker daemon architecture. See
[running](../../docs/running.md) for the complete local setup. CLI and desktop runs use these same
engine-owned adapters.

Configure `HelperPath`, `WorkDir`, and optionally `DockerHost` and `FirewallImage`. The work
directory must be readable by the Docker daemon; for Colima, use a path under a shared `/Users`
directory. No source repository or Docker socket is mounted inside the workload. Only the fixed
package, declared input files and trusted helper are bound read-only. Workload UID/GID is 65532, all
capabilities are dropped, rootfs is read-only, and privilege escalation is disabled. CPU, RAM and
process limits are enforced by Docker. Writable tmpfs allocations sum to the profile disk limit:
approximately 90% workspace, 5% ABI output, 5% temporary files, and a 4 KiB shared memory directory.
Tmpfs also counts toward the memory limit.

Each container carries the durable engine identity. The service acquires exclusive ownership of that
engine before `CleanupOwned` removes its orphaned containers and temporary package directories.
Cleanup never selects another engine's containers. The host renews a read-only lease every 5
seconds; PID 1 exits when that lease is 45 seconds old, even if the engine was killed without
running cleanup. All remaining processes stop; a stopped container retains at most 16 KiB of
supervisor diagnostics until normal cleanup or the next engine startup removes it. Code and agent
containers also enforce their activity deadline independently of lease renewal. Run-scoped MCP
services retain the host lease until the run closes. The supervisor disables same-UID process
tracing so workloads cannot alter its watchdog.

A transient failed read or stale shared-filesystem inode cannot revoke an already verified lease.
The supervisor retains its last valid expiry and the immutable hard deadline while the host retries
renewal writes. Retrying a write does not suspend expiry enforcement or restart a stopped container.
Lease timestamps cross the host/VM boundary as UTC, so the daemon and host clocks must be
synchronized; verified deadlines then use Go's monotonic clock locally.

The helper is PID 1 and reaps orphaned children. Each process call stops all remaining untrusted
processes before returning. Filesystem tools run only after quiescence; path components cannot
contain symlinks, and artifact collection rejects hard links. File tools support up to 1 MiB,
collected artifacts up to 64 MiB. The command API never constructs a shell command from argv; a
shell must be an explicit executable.

`network: none` uses Docker's network isolation. `any` uses its bridge without publishing ports.
`allowlist` resolves exact hosts during admission and freezes their IP addresses in the plan. A
separate trusted container installs an IPv4/IPv6 firewall before untrusted work starts, then exits.
The workload has no NET_ADMIN. DNS packets are blocked; `/etc/hosts` provides the frozen name
resolution. Only allowed addresses, loopback and replies to established connections are permitted.

This backend implements **address-based host filtering**. Names sharing a permitted address share
its network access; HTTP Host/SNI identity and routing inside an explicitly allowed proxy are not
inspected. New DNS addresses require a new run. The firewall helper image and execution helper
digest are fixed at admission.

## Recovery

Every external send has a journal identity. MCP idempotency keys identify logical calls across
attempts; delivery journal identities include the attempt. Replaying completed responses consumes no
new budget. An interrupted send is not silently reissued in the same attempt. Read operations and
explicitly idempotent calls may use the graph's retry policy; unknown writes require an operator
decision.

Code and agent attempts also persist final validated outputs. An unfinished attempt whose workspace
was lost cannot replay a process into a fresh workspace. A lost run-scoped MCP session likewise
cannot silently become a new session. No claim of exactly-once execution is made for arbitrary
external APIs.

## Tests

`go test ./internal/adapters/...` uses local HTTP servers and the real MCP SDK. Optional integration
tests use Docker and the explicitly selected Ollama endpoint:

```sh
KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
KNOTRA_TEST_WORKDIR="$PWD/.knotra/test-work" \
KNOTRA_TEST_FIREWALL_IMAGE=knotra-firewall:dev \
KNOTRA_TEST_OLLAMA=http://127.0.0.1:11434 \
go test ./internal/adapters/... -count=1
```

The Ollama test uses `qwen3.5:9b`. Tests remove only containers they create.
