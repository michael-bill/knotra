# Service fleet release gate

A release owner needs more than an AI opinion: executed checks against one repository version, clear
blockers, reviewable risks and authorization tied to the exact files. This package runs that process
through isolated sandboxes and saved human requests.

The supplied commerce snapshot contains six small executable Python services: orders, billing,
identity, inventory, notifications and search. Their regression tests exercise prices, rounding,
authorization expiry, stock reservations, duplicate delivery and Unicode search normalization. Every
result shown in the screenshots comes from executing the supplied snapshot.

## What executes

The default graph has 30 nodes and six parallel service lanes. Each lane runs three checks:

- Python `unittest` regression tests, including a minimum executed-test count and source-byte
  checks.
- Request/response compatibility against the declared baseline API contract.
- Nine rules on the declared deployment configuration: non-root UID, read-only root, no privilege or
  host networking, dropped capabilities, pinned image, memory limit, bounded canary and pinned
  rollback image.

The matrix checks complete service coverage and the snapshot hash on every result. Any failed check
blocks the run **before agents, human authorization or publication**. Check artifacts remain
available to diagnose the failure. Unsupported API schema constructs fail rather than reporting a
pass.

Three independent agents review security configuration, API compatibility and operational readiness.
Each can only read its scoped evidence and the release policy. They return recommendations with
exact citations; code rejects invented citations. Recommendations remain model advice, separately
labeled from executed checks. A human then authorizes the packet with a reviewer, change ticket and
comments.

Publication preserves the reviewed bytes and packages the snapshot, executed results, cited reviews
and `authorization.json` with their SHA-256 hashes.

## Run the supplied snapshot

Start `knotra quickstart` with Docker and Ollama, then open another terminal at the repository root.
Use `bin/knotra` after `make build helper`, or replace it with the absolute path to a downloaded
CLI. Python 3 is needed on the host only for the snapshot utilities; sandbox checks use
`python_box`.

```sh
mkdir -p .knotra/release-gate
python3 examples/showcase/release-gate/scripts/pack_snapshot.py \
  examples/showcase/release-gate/sample \
  --output .knotra/release-gate/repository.zip
bin/knotra validate examples/showcase/release-gate/pipeline.yaml \
  --profile examples/local/profile.yaml
bin/knotra artifacts upload .knotra/release-gate/repository.zip \
  --media-type application/zip --json
```

Copy the returned `artifact.id`:

```sh
bin/knotra run examples/showcase/release-gate/pipeline.yaml \
  --profile local --artifact repository=ARTIFACT_ID
bin/knotra runs watch RUN_ID
```

Pass `--endpoint http://HOST:PORT` if you chose another engine address. The same pipeline uses an
OpenAI or Anthropic connection when its engine profile supplies `model_main` and `python_box`. See
[model profiles](../../../docs/running.md); no model name is embedded in the pipeline.

Open the actual run in desktop. Select a service test node to inspect its execution and metrics;
select an agent to inspect its evidence reads and model iterations. In Inbox, inspect the dossier
and raw checks, then submit:

```json
{
  "approved": true,
  "reviewer": "Release owner",
  "change_ticket": "CHG-1042",
  "comments": "Reviewed executed gates, canary monitoring and rollback ownership."
}
```

While waiting, stop quickstart and restart with the same directory. The request and reviewed files
remain available without repeating completed checks. Cancel to withhold authorization. Download
`authorized-release.zip` after approval.

## Use your own service inventory

Prepare an explicit snapshot directory with a `release.json` manifest and the referenced source,
tests, baseline/candidate API contracts and deployment files. The
[sample manifest](sample/release.json) is the input contract. No working-directory files are
implicitly sent to the engine. Inspect the snapshot before uploading; archive paths, duplicate
files, links and size limits are checked.

Generate lanes for another inventory, then pack and upload that snapshot:

```sh
python3 examples/showcase/release-gate/scripts/build_pipeline.py \
  /path/to/release-snapshot/release.json \
  --output examples/showcase/release-gate/pipeline.json
python3 examples/showcase/release-gate/scripts/pack_snapshot.py \
  /path/to/release-snapshot --output .knotra/release-gate/repository.zip
bin/knotra run examples/showcase/release-gate/pipeline.json \
  --profile YOUR_PROFILE --artifact repository=ARTIFACT_ID
```

Upload the new ZIP first and use its new ID. The generated JSON is valid YAML 1.2 and stays in the
same package directory as the adapters and policy. The snapshot's service set must match the
compiled lanes exactly; adding a service cannot silently leave it unchecked. The manifest supports
1–50 services. Concurrency remains capped at six; each service adds three executable nodes.

The regression adapter discovers `test_*.py` with Python `unittest`. Supply the project dependencies
in a trusted, pinned sandbox image; this package does not install dependencies during a run. For
other languages or CI systems, replace that adapter while retaining the evidence IDs, snapshot
identity, nonzero executed-test count and explicit failure result. API contracts use `type`,
`properties`, `required`, `enum` and `items`. Deployment configuration uses the JSON structure in
the [sample](sample/orders/deployment.json).

This is a reusable release evidence gate for controlled production and enterprise experiments. Its
workflow combines actual test execution, deterministic blockers, scoped model analysis, durable
review and immutable files. Authenticated approval identities and organization-specific policy
remain separate integration work. A typed reviewer string is not an identity verification or digital
signature.

## Verify and exercise a blocker

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s examples/showcase/release-gate/tests -v
```

The checks execute all 36 sample regression tests, reject incompatible contracts and unsafe
configurations, check archive paths and service coverage, reject invented agent citations and verify
byte-preserving authorization. To try a blocked run, make a copy of the sample snapshot, add a new
value to an existing **response** enum or set `privileged` to `true`, pack it again and upload it as
a new artifact. The release gate fails and no authorization request is created.
