# Knotra client ↔ engine API

Status: **implemented by the Go engine, CLI and desktop client, verified 4 October 2026**. The
boundary originated in the desktop proposal of 1 October; its protocol identifier remains
`knotra.desktop/1`. The desktop client lives in `app/`; browser and native acceptance tests also
connect to the real engine. The machine-readable companion is [OpenAPI](desktop-v1.openapi.json).

This contract follows [architecture](../architecture.md), [execution v1](../notation/execution.md),
[validation](../notation/validation.md) and [EngineProfile](../notation/engine-profile.md).
Engine-owned state and immutable plans are authoritative. SQLite in the desktop app stores local
drafts, settings, cached views, durable events/cursors and command receipts. The CLI uses durable
local journal files. Neither client schedules execution or resolves provider credentials.

## Transport and compatibility

The configured base URL can include a reverse-proxy prefix. All routes below are relative to
`<base>/v1`. The native app sends HTTP from Rust; the webview has no unrestricted networking
permission. Redirects are rejected. Local HTTP is allowed only for loopback IPs or `localhost`.
Remote engines require HTTPS and a bearer access token. The current engine serves one trusted
principal per deployment. Desktop tokens are session-only and discarded on disconnect/exit; the CLI
reads its token from the environment. Neither client includes tokens in command receipts.

`GET /info` returns `{protocol: "knotra.desktop/1", engineId, principalId, version, capabilities}`.
An incompatible protocol or missing engine identity prevents connecting. `engineId` is a durable
deployment identity: replacing the database/deployment must create a new ID. The cache is namespaced
by normalized base URL, engine identity AND authenticated `principalId`; run IDs from different
engines or accounts cannot share cached state or commands.

JSON uses camelCase. IDs are opaque, nonempty, at most 256 characters and cannot contain control
characters. Every identifier is encoded as one URL segment; nested instance addresses are not
interpreted as path traversal. Timestamp fields are RFC 3339. Nullable pagination cursor fields are
explicit. Every JSON response is an object, including command receipts. Packages and artifact bytes
are limited to 64 MiB, packages to 512 files. The engine JSON request limit and CLI response limit
are 256 MiB, allowing encoded packages and values; notation limits still apply separately. The
desktop client retains its 96 MiB JSON response limit. Lists use cursors and a soft page byte
budget; a single large item may exceed that budget.

A browser preview uses the same contract with browser fetch. Its engine must explicitly support CORS
for the preview origin (`http://127.0.0.1:1420` during development), `GET`, `POST`, `OPTIONS` and
`Authorization`, `Content-Type`, `Idempotency-Key`, `Last-Event-ID` headers. Native requests do not
need CORS. Browser settings and command receipts use localStorage, while durable event IDs, cursors,
and bounded event windows use IndexedDB; desktop persistence is SQLite. No browser test service is
launched in a production application.

## Immutable packages and definitions

`POST /packages/validate` receives `{package, profile, inputs, artifacts}` and returns
`{valid, diagnostics}`. This runs authoritative package, semantic, input and admission checks
without executing user tools. The desktop's offline checks do not replace it. Diagnostics have
`severity`, stable `code`, `phase`, `message`, JSON Pointer `path`, and source `file/line/column`
where applicable. No credential values appear in diagnostics.

`package` is `{entrypoint, source, files: [{path, content}]}`. `source` is the exact UTF-8
entrypoint YAML; `files` is the complete manifest, including the entrypoint itself. Each `content`
is base64 for the exact file bytes, including binary files. The entrypoint bytes must match nonempty
`source`. Drafts store entrypoint source separately from supporting files. The desktop adapter
includes its exact bytes in the wire manifest before validation or publication. Paths are relative
NFC package paths; traversal, symlinks, duplicate/case-colliding names and undeclared supporting
files are rejected. Engine source loading, compilation and canonical manifest digest computation
follow notation v1; HTTP JSON serialization is **not** the package digest. Native client checks
basic paths/encoding/limits; the engine validates the full manifest/import closure and document
contracts.

`POST /definitions` receives `{package}` and returns `{definition}`. The engine commits an immutable
definition/version with canonical `packageDigest`. Identical package content can reuse the same
version, regardless of draft identity. `GET /definitions?cursor=…` lists immutable versions as
`{items, nextCursor}`; `GET /definitions/{definitionId}` retrieves the exact package. Updating a
draft never changes an admitted version or running plan.

`GET /profiles` lists `{items: [{id,title,revision}]}`. The engine selects and freezes the indicated
effective EngineProfile revision during admission. `GET /resources` returns
`{items: [{id,kind,title,capabilities,status}]}` with canonical model/MCP/sandbox/secret IDs and
availability. It exposes references/capabilities, never secret values, environment variables, OAuth
tokens or raw connection credentials. Pipeline aliases continue to be edited in YAML.

## Runs and actions

`POST /runs` receives `{definitionId,profile,inputs,artifacts}` and returns `{run}` after admission
and durable run acceptance. `inputs` contains JSON port values; omitted defaults are inserted and
checked by the engine, while explicit `null` is a real value. `artifacts` maps artifact input port
names to engine-registered artifact ID(s). Arbitrary JSON cannot forge registered handles. Admission
is repeated even after a separate successful check. The run snapshot contains the frozen package;
the internal plan must additionally fix effective profile, limits, permissions, adapter/image/tool
schema versions as required by the docs.

`GET /runs?cursor=…` returns `{items: Run[], nextCursor: string|null}` in stable server-defined
order; the cursor is opaque. `GET /runs/{runId}` returns `{run}`. A Run has ID, definition/profile
identity, timestamps, immutable package/input values, output values/artifact descriptors, instance
states and diagnostics. Instance IDs identify nested graphs/items/iterations independently from the
reusable YAML `nodeId`; attempts and external operations have their own IDs. Partial results cannot
manufacture a succeeded run. The root run uses the same status vocabulary as instances: `pending`,
`ready`, `running`, `retry_wait`, `waiting_human`, `waiting_resolution`, `succeeded`, `skipped`,
`failed`, `cancelled` (root skipped may be omitted by an engine implementation). Terminal states are
immutable.

New workflow histories additionally expose optional instance metadata: `nodeType`, `graphPath` (the
definition address, such as `/nodes/map/body/nodes/work`), `parentInstanceId`, and a zero-based
`iterationIndex` under the immediate foreach/loop parent. `scope` remains the admitted pipeline
filename; child pipeline definition paths restart at `/nodes/…`. Root instances omit a parent.
`startedAt`, `finishedAt`, `updatedAt` and `reason` explain execution and waiting. Bound `inputs`
and validated `outputs` use `{values,artifacts}` envelopes. Each context preview is limited to 32
KiB and preserves whole port values; `dataTruncated: true` means oversized ports were omitted.
Artifact descriptors omit sandbox paths. These optional fields can be absent in old histories;
clients must not manufacture nested identities or timing when metadata is unavailable.

The engine returns `availableActions` based on authoritative state and permissions. The client
renders only these actions and never changes execution status optimistically:

- `POST /runs/{runId}/cancel`, body `{}`: request cancellation. Receipt `{accepted:true,runId}` does
  not mean all active work has stopped.
- `POST /runs/{runId}/resume`, body `{}`: continue the SAME saved attempt/checkpoint only if the
  engine says it is safe. It is not retry or a new run.
- `POST /runs/{runId}/instances/{instanceId}/resolve`, body `{outcome,evidence,outputs?}`: record
  evidence for `succeeded`, `not_started` or `failed`. The engine checks required verified outputs
  and whether another action is safe. No blind resubmission of an uncertain external operation
  occurs.

The current engine does not advertise `resume`: that route returns `409 UNSAFE_ACTION`. Recovery of
an interrupted workflow is automatic; uncertain external effects require the explicit `resolve`
command. Artifact outputs in a resolution use registered IDs.

A new run is explicit `POST /runs`; selected-place restart and arbitrary automatic retry controls
are outside this contract, since checkpoint reuse and effect authorization need additional engine
contracts.

## Idempotent commands and uncertain outcomes

Every mutation except validation requires `Idempotency-Key`, a nonempty `[A-Za-z0-9_-]{1,128}`
operation ID. The engine atomically binds `(authenticated principal, engine identity, operation ID)`
to the route, payload and final receipt. Reusing it with different content yields
`409 OPERATION_CONFLICT`; same content returns the original receipt without creating an extra run,
accepting a second human response, repeating an upload or creating another acceptance event.
Concurrent deliveries serialize at the engine. Store receipts for at least the lifetime of related
runs/requests and clearly reject an expired key; do not silently treat an old key as a new
operation.

The native app journals the complete command before sending and commits the result after receiving
it. Transport loss, timeout, malformed responses and ambiguous server errors leave it pending,
including after restart. The UI offers **Reconcile** using the original ID and unchanged payload; it
does not generate a new ID automatically. Definitive 4xx rejection other than 408/429 is retained as
a rejected receipt and can be corrected with a new operation. The engine must use those statuses
only when rejection is definitive. Mutations are never automatically retried by HTTP middleware.

Error bodies are `{code,message,diagnostics:[]}`. Typical statuses: 400 invalid transport input;
401/403 authentication/permission; 404 missing object; 409 conflict/closed request/unsafe action;
422 validation failure; 429 throttled; 503 unavailable. Clients should not assume a failed transport
means an operation never began. API authorization applies to every request, artifact and run
operation.

## Human requests

`GET /requests?cursor=…` returns `{items: HumanRequest[], nextCursor}`. Each request has stable
request/run/instance/attempt IDs, prompt, creation/deadline, status, `{values,artifacts}` input
context and `responseSchema` for the object of named output ports. Input artifact handles contain
`id`, `mediaType`, `size` and `sha256`, without sandbox paths; fetch artifact metadata by ID for
`name` and `origin`. `POST /requests/{requestId}/response` receives `{outputs}` with its own
operation ID and returns `{accepted:true,requestId}`.

The first VALID response is atomically accepted with output publication. A schema-invalid response
returns 422, leaves the same request open, and permits a corrected response with a new operation ID.
A closed, expired or cancelled request rejects conflicting responses; retrying the already accepted
identical operation returns its receipt. Responding to request A can never apply to later request B.
Deadline/cancellation races use first committed engine operation. Client-side schema feedback is a
convenience; the engine performs full normative validation, including registered artifact handles
where declared.

## Durable SSE

`GET /runs/{runId}/events` sends `Content-Type: text/event-stream`, `Cache-Control: no-cache` and
heartbeat comments at least every 30 seconds. Every data frame has a nonempty SSE `id`, equal to
JSON event `id`, and JSON `runId` equal to the stream run. Event JSON includes RFC 3339 `at`,
`type`, human `message`, optional instance/attempt/operation IDs and `data`. Frame maximum is 256
KiB. No provider-hidden reasoning or secrets are required or exposed.

The client persists events and the cursor atomically BEFORE presenting them. On reconnect it sends
`Last-Event-ID`. The engine replays from the next retained event; repeated delivery is permitted and
deduplicated by `(engine identity,runId,eventId)`. Disconnecting stops observation only, not
execution. Clients read fresh run/request/artifact projections after an event and periodically
reconcile, since PostgreSQL projections can lag Temporal. No client-derived state transition
replaces the engine snapshot.

For this contract, events/cursors remain replayable for a retained run. If the run/event history was
purged, the server returns a definitive error rather than silently skipping a gap.
Retention-reset/cursor-expiry negotiation requires a later protocol revision; the client currently
surfaces the reconnect failure and continues to display its last received history. It will not label
missing history as complete. The UI keeps the most recent 1000 detailed events per run; native
SQLite preserves all received IDs/events for replay deduplication. Browser preview commits the exact
opaque event ID, reconnect cursor and a display window (at most 1000 events or approximately 512 KiB
of serialized text) in one IndexedDB transaction before delivery. Deduplication IDs remain available
after a displayed event is evicted or the page is reloaded; IDs are never compared as numbers or
sorted to infer delivery. Their durable storage grows with received events, without expanding the
synchronous localStorage cache. Records are isolated by endpoint, engine, principal and run. Legacy
localStorage events, IDs and cursors migrate atomically before their original keys are removed;
other cached views and pending commands are preserved. An aborted migration or event transaction
cannot advance the cursor or discard a deduplication ID.

## Execution observations and history

`GET /runs/{runId}/history?cursor=…&instanceId=…` returns `{items: Event[], nextCursor}` in the same
committed order as SSE. Both query parameters are optional; `instanceId` selects one exact instance,
and `cursor` must identify an existing event in the same run. Pages contain at most 100 events. A
full final page may return a cursor followed by an empty page. This read-only route lets an
inspector retrieve earlier execution details independently of the client's 1000-event live display
cache. `history` and `execution-observations` are advertised capabilities.

Activity events use the existing event envelope and instance/attempt/operation identities:

- `model.started`, `model.delta`, `model.completed`, `model.failed` describe model requests,
  streamed visible output and measured provider usage/timing when available.
- `agent.iteration` identifies each agent turn; `tool.started` and `tool.completed` describe tool
  calls with arguments/results and durations; `tool.completed` includes `isError` for tool failures.
- `output.validating` and `output.completed` distinguish a draft response from validated output.

The adapter bounds diagnostic previews to 48 KiB, strings to 16 KiB, arrays to 64 elements, objects
to 128 fields and nesting to 12 levels. `truncated: true` marks a shortened preview;
`observationIncomplete: true` marks a prior observation delivery failure. Storage additionally
limits observation data to 64 KiB per event. Oversized data is replaced by
`{truncated:true,originalBytes:…}` with small step/name/timing fields retained when present. Clients
must show this explicitly instead of treating the record as complete. Observation events are
best-effort diagnostics: a storage outage can leave gaps in the detailed activity trace, but cannot
cause an external operation to repeat or replace the authoritative node outcome. They are stored
separately from workflow projection sequence numbers, while sharing the run lock and durable event
cursor with status events. SSE polls every 250 ms and drains retained full batches immediately.
Provider-hidden reasoning, credentials and attempt-local sandbox paths are excluded from observation
payloads. Model text, pipeline inputs and tool results remain untrusted application content.

## Artifacts

`POST /artifacts` receives `{name,mediaType,content}` (base64) and atomically registers immutable
bytes. Receipt `{artifact}` contains `id,name,mediaType,size,sha256,origin`. Uploaded input
artifacts have empty origin; outputs identify originating run/instance/attempt/operation when
available. `GET /artifacts?cursor=…` lists visible descriptors. `GET /artifacts/{artifactId}`
returns `{artifact}`; `GET /artifacts/{artifactId}/content` returns the exact bytes, never a local
sandbox/engine path or a redirect.

The app checks BOTH size and SHA-256 before preview/export. Corruption or unavailable bytes prevents
using the data. Content type is metadata, not permission to render arbitrary HTML or execute code;
previews are escaped text or binary metadata. Input artifact permission, MIME and port/collection
checks are authoritative on admission. The native app exports only through user-selected native
dialogs and preserves binary bytes.

## Testing and delivery boundary

Rust tests cover persistent drafts/journals/cursors, URL and path restrictions, real HTTP command
delivery, exact idempotency headers and transport loss, incremental SSE, and corrupt artifact
rejection. Browser integration tests use a loopback **contract fixture** to exercise settings →
admission → publish → start → events → saved review → artifact export and mutation recovery. This
fixture is a test service, not a workflow engine; it calls no models, MCP, Docker or Temporal.

The production desktop app launches no engine processes. Start the engine using the
[running guide](../running.md). The Go engine has separate deterministic, persistence, sandbox and
real-service acceptance tests described in [verification](../verification.md). Opt-in browser tests
connect to the real engine for Ollama execution, completed history replay, human review, imported
child files and binary artifacts. Native acceptance checks command delivery, schema rejection, saved
events and binary downloads against that engine. Fixture tests remain separate from these acceptance
tests.
