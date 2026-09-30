# Desktop ↔ engine API proposal

Status: **proposed contract, 1 October 2026**. Implemented by the desktop client in `app/`, not by a Go engine. This closes the previously unspecified client boundary without claiming the engine, Temporal workers, CEL compiler, provider adapters or admission checks exist. Review and adopt this proposal before implementing an engine. The machine-readable companion is [OpenAPI](desktop-v1.openapi.json).

The proposal follows [architecture](../architecture.md), [execution v1](../notation/execution.md), [validation](../notation/validation.md) and [EngineProfile](../notation/engine-profile.md). Engine-owned state and immutable plans are authoritative. SQLite in the desktop app stores local drafts, settings, cached views, durable events/cursors and command receipts. It never schedules execution or resolves provider credentials.

## Transport and compatibility

The configured base URL can include a reverse-proxy prefix. All routes below are relative to `<base>/v1`. The native app sends HTTP from Rust; the webview has no unrestricted networking permission. Redirects are rejected. Local HTTP is allowed only for loopback IPs or `localhost`. Remote engines require HTTPS and a bearer access token. Tokens are session-only, are never included in workspace backups or command receipts, and are discarded on disconnect/exit. This bearer mechanism is part of the **proposal**, not an already agreed project authentication standard.

`GET /info` returns `{protocol: "knotra.desktop/1", engineId, principalId, version, capabilities}`. An incompatible protocol or missing engine identity prevents connecting. `engineId` is a durable deployment identity: replacing the database/deployment must create a new ID. The cache is namespaced by normalized base URL, engine identity AND authenticated `principalId`; run IDs from different engines or accounts cannot share cached state or commands.

JSON uses camelCase. IDs are opaque, nonempty, at most 256 characters and cannot contain control characters. Every identifier is encoded as one URL segment; nested instance addresses are not interpreted as path traversal. Timestamp fields are RFC 3339. Nullable pagination cursor fields are explicit. Every JSON response is an object, including command receipts. JSON requests/responses are bounded; packages and artifact bytes are limited to 64 MiB, packages to 512 files, and the desktop JSON response limit is 96 MiB.

A browser preview uses the same contract with browser fetch. Its engine must explicitly support CORS for the preview origin (`http://127.0.0.1:1420` during development), `GET`, `POST`, `OPTIONS` and `Authorization`, `Content-Type`, `Idempotency-Key`, `Last-Event-ID` headers. Native requests do not need CORS. Browser persistence remains localStorage; desktop persistence is SQLite. No browser test service is launched in a production application.

## Immutable packages and definitions

`POST /packages/validate` receives `{package, profile, inputs, artifacts}` and returns `{valid, diagnostics}`. This runs authoritative package, semantic, input and admission checks without executing user tools. The desktop's offline checks do not replace it. Diagnostics have `severity`, stable `code`, `phase`, `message`, JSON Pointer `path`, and source `file/line/column` where applicable. No credential values appear in diagnostics.

`package` is `{entrypoint, source, files: [{path, content}]}`. `source` is the exact UTF-8 entrypoint YAML; `content` is base64 for the exact supporting bytes, including binary files. Paths are relative NFC package paths; traversal, symlinks, duplicate/case-colliding names and undeclared supporting files are rejected. Engine source loading, compilation and canonical manifest digest computation follow notation v1; HTTP JSON serialization is **not** the package digest. Native client checks basic paths/encoding/limits; the engine validates the full manifest/import closure and document contracts.

`POST /definitions` receives `{package}` and returns `{definition}`. The engine commits an immutable definition/version with canonical `packageDigest`. Identical package content can reuse the same version, regardless of draft identity. `GET /definitions` lists immutable versions; `GET /definitions/{definitionId}` retrieves the exact package. Updating a draft never changes an admitted version or running plan.

`GET /profiles` lists `{items: [{id,title,revision}]}`. The engine selects and freezes the indicated effective EngineProfile revision during admission. `GET /resources` returns `{items: [{id,kind,title,capabilities,status}]}` with canonical model/MCP/sandbox/secret IDs and availability. It exposes references/capabilities, never secret values, environment variables, OAuth tokens or raw connection credentials. Pipeline aliases continue to be edited in YAML.

## Runs and actions

`POST /runs` receives `{definitionId,profile,inputs,artifacts}` and returns `{run}` after admission and durable run acceptance. `inputs` contains JSON port values; omitted defaults are inserted and checked by the engine, while explicit `null` is a real value. `artifacts` maps artifact input port names to engine-registered artifact ID(s). Arbitrary JSON cannot forge registered handles. Admission is repeated even after a separate successful check. The run snapshot contains the frozen package; the internal plan must additionally fix effective profile, limits, permissions, adapter/image/tool schema versions as required by the docs.

`GET /runs?cursor=…` returns `{items: Run[], nextCursor: string|null}` in stable server-defined order; the cursor is opaque. `GET /runs/{runId}` returns `{run}`. A Run has ID, definition/profile identity, timestamps, immutable package/input values, output values/artifact descriptors, instance states and diagnostics. Instance IDs identify nested graphs/items/iterations independently from the reusable YAML `nodeId`; attempts and external operations have their own IDs. Partial results cannot manufacture a succeeded run. The root run uses the same proposed status vocabulary as instances: `pending`, `ready`, `running`, `retry_wait`, `waiting_human`, `waiting_resolution`, `succeeded`, `skipped`, `failed`, `cancelled` (root skipped may be omitted by an engine implementation). Terminal states are immutable.

The engine returns `availableActions` based on authoritative state and permissions. The client renders only these actions and never changes execution status optimistically:

- `POST /runs/{runId}/cancel`, body `{}`: request cancellation. Receipt `{accepted:true,runId}` does not mean all active work has stopped. Published effects are not rolled back.
- `POST /runs/{runId}/resume`, body `{}`: continue the SAME saved attempt/checkpoint only if the engine says it is safe. It is not retry or a new run.
- `POST /runs/{runId}/instances/{instanceId}/resolve`, body `{outcome,evidence,outputs?}`: record evidence for `succeeded`, `not_started` or `failed`. The engine checks required verified outputs and whether another action is safe. No blind resubmission of an uncertain external operation occurs.

A new run is explicit `POST /runs`; selected-place restart and arbitrary automatic retry controls are outside this proposal, since checkpoint reuse and effect authorization need additional engine contracts.

## Idempotent commands and uncertain outcomes

Every mutation except validation requires `Idempotency-Key`, a nonempty `[A-Za-z0-9_-]{1,128}` operation ID. The engine atomically binds `(authenticated principal, engine identity, operation ID)` to the route, payload and final receipt. Reusing it with different content yields `409 OPERATION_CONFLICT`; same content returns the original receipt without creating an extra run, accepting a second human response, repeating an upload or creating another acceptance event. Concurrent deliveries serialize at the engine. Store receipts for at least the lifetime of related runs/requests and clearly reject an expired key; do not silently treat an old key as a new operation.

The native app journals the complete command before sending and commits the result after receiving it. Transport loss, timeout, malformed responses and ambiguous server errors leave it pending, including after restart. The UI offers **Reconcile** using the original ID and unchanged payload; it does not generate a new ID automatically. Definitive 4xx rejection other than 408/429 is retained as a rejected receipt and can be corrected with a new operation. The engine must use those statuses only when rejection is definitive. Mutations are never automatically retried by HTTP middleware.

Error bodies are `{code,message,diagnostics:[]}`. Typical statuses: 400 invalid transport input; 401/403 authentication/permission; 404 missing object; 409 conflict/closed request/unsafe action; 422 validation failure; 429 throttled; 503 unavailable. Clients should not assume a failed transport means an operation never began. API authorization applies to every request, artifact and run operation.

## Human requests

`GET /requests?cursor=…` returns `{items: HumanRequest[], nextCursor}`. Each request has stable request/run/instance/attempt IDs, prompt, creation/deadline, status, `{values,artifacts}` input context and `responseSchema` for the object of named output ports. Artifact descriptors do not include sandbox paths. `POST /requests/{requestId}/response` receives `{outputs}` with its own operation ID and returns `{accepted:true,requestId}`.

The first VALID response is atomically accepted with output publication. A schema-invalid response returns 422, leaves the same request open, and permits a corrected response with a new operation ID. A closed, expired or cancelled request rejects conflicting responses; retrying the already accepted identical operation returns its receipt. Responding to request A can never apply to later request B. Deadline/cancellation races use first committed engine operation. Client-side schema feedback is a convenience; the engine performs full normative validation, including registered artifact handles where declared.

## Durable SSE

`GET /runs/{runId}/events` sends `Content-Type: text/event-stream`, `Cache-Control: no-cache` and heartbeat comments at least every 30 seconds. Every data frame has a nonempty SSE `id`, equal to JSON event `id`, and JSON `runId` equal to the stream run. Event JSON includes RFC 3339 `at`, `type`, human `message`, optional instance/attempt/operation IDs and `data`. Frame maximum is 256 KiB. No provider-hidden reasoning or secrets are required or exposed.

The client persists events and the cursor atomically BEFORE presenting them. On reconnect it sends `Last-Event-ID`. The engine replays from the next retained event; repeated delivery is permitted and deduplicated by `(engine identity,runId,eventId)`. Disconnecting stops observation only, not execution. Clients read fresh run/request/artifact projections after an event and periodically reconcile, since PostgreSQL projections can lag Temporal. No client-derived state transition replaces the engine snapshot.

For this proposed contract, events/cursors remain replayable for a retained run. If the run/event history was purged, the server returns a definitive error rather than silently skipping a gap. Retention-reset/cursor-expiry negotiation requires a later protocol revision; the client currently surfaces the reconnect failure and continues to display its last received history. It will not label missing history as complete. The UI keeps the most recent 1000 detailed events per run; native SQLite preserves all received IDs/events for replay deduplication. Browser preview stores IDs and a 1000-event display cache.

## Artifacts

`POST /artifacts` receives `{name,mediaType,content}` (base64) and atomically registers immutable bytes. Receipt `{artifact}` contains `id,name,mediaType,size,sha256,origin`. Uploaded input artifacts have empty origin; outputs identify originating run/instance/attempt/operation when available. `GET /artifacts?cursor=…` lists visible descriptors. `GET /artifacts/{artifactId}` returns `{artifact}`; `GET /artifacts/{artifactId}/content` returns the exact bytes, never a local sandbox/engine path or a redirect.

The app checks BOTH size and SHA-256 before preview/export. Corruption or unavailable bytes prevents using the data. Content type is metadata, not permission to render arbitrary HTML or execute code; previews are escaped text or binary metadata. Input artifact permission, MIME and port/collection checks are authoritative on admission. The native app exports only through user-selected native dialogs and preserves binary bytes.

## Testing and delivery boundary

Rust tests cover persistent drafts/journals/cursors, URL and path restrictions, real HTTP command delivery, exact idempotency headers and transport loss, incremental SSE, and corrupt artifact rejection. Browser integration tests use a loopback **contract fixture** to exercise settings → admission → publish → start → events → saved review → artifact export and mutation recovery. This fixture is a test service, not a workflow engine; it calls no models, MCP, Docker or Temporal.

The production app launches no engine processes. Configure an engine after it implements this protocol, or adapt the narrow client routes/DTOs and OpenAPI together if the agreed engine contract differs. The Go engine roadmap and acceptance scenarios remain outstanding.
