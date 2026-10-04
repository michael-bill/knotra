# Local starter pipelines

Four packages demonstrate increasingly substantial work with
[the local profile](../local/profile.yaml). They use Ollama `qwen3.5:9b` through `model_main`, with
a 4096-token response budget. Python steps use `python_box`; the game uses `node_box`. No MCP
servers, secrets, external search, or package installations are required by these pipelines. Prepare
PostgreSQL, Temporal, Docker/helper and Ollama with the [running guide](../../docs/running.md),
including both `python:3.13-alpine` and `node:22-alpine` images.

| Package                                            | What happens                                                                                                                                                               | Deliverables                                                                       |
| -------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- |
| [hello](hello/pipeline.yaml)                       | A short model response is saved by a code step. This is the first connectivity check.                                                                                      | A personal greeting and `greeting.txt`.                                            |
| [research-dossier](research-dossier/pipeline.yaml) | Three independent agents extract quoted evidence; code verifies facts, calculates scores and selects an eligible offer; a model explains the supplied decision.            | `dossier.md`, `comparison.json`, and `dossier.zip` including the original sources. |
| [tic-tac-toe](tic-tac-toe/pipeline.yaml)           | A model plans and generates game logic; the engine runs fixed tests and carries failures into a bounded correction loop; another sandbox verifies and packages the result. | Playable `index.html`, `game.js`, and `test-report.json`.                          |
| [publication](publication/pipeline.yaml)           | A writer drafts an announcement; a separate editor checks it against the brief; a person approves and provides feedback; the writer revises it.                            | `publication.md` and `review-history.json` recording the drafts and reviews.       |

Every input has a useful default. In the application, open a card on **Pipelines**, or use **New
pipeline → Ready to run** to create another copy. Connect in **Settings**, choose **Run**, and
select profile `local`. New workspace titles and descriptions follow the selected interface
language. Prompts, defaults, files and model output remain editable content; language switching does
not rewrite them.

## Research from supplied materials

The sources compare three **fictional sample offers** for launching a help center: a hosted service,
the team's existing CMS, and a custom documentation site. This is research over supplied materials,
not live web research or independent verification of vendor claims.

Each `foreach` iteration gives a separate agent one source and only `files.read`. Concurrency is
bounded at one to suit a local model; iterations retain independent contexts and history. Agents
return exact quotations, extracted costs and setup estimates, and limitations. The comparison step
checks quotations and the numerical/categorical facts against the original files before ranking. It
selects the highest-scoring eligible offer, or no offer when none meets both constraints. The model
explains this supplied decision and evidence gaps. The dossier prints the selected source and
eligible/ineligible source lists directly from code, independently of model prose.

Change `monthly_budget`, `launch_days`, and the `cost`, `speed`, `control`, `support` weights to
explore the decision. All criteria receive scores from 0 to 5. Cost and speed scores decrease
linearly toward zero at the budget and deadline. Control and support use the explicit rubric in
`scripts/compare.py`. The weighted average is scaled to 100. Budget and deadline are also hard
eligibility constraints, so a high score cannot silently override them. Labor and taxes are excluded
from the supplied prices and called out in the dossier.

Replace files under `sources/` with your own offers while retaining their structured cost, setup,
control and support labels; the deterministic checker relies on those labels. The Markdown dossier
includes the calculated table, scoring method, source filenames, verified quotations and evidence
gaps. The ZIP includes the dossier, JSON comparison and all three complete source files.

## Play the generated game

The model generates `game.js` from the fixed API contract and a short plan. The engine writes this
source into a Node sandbox and runs read-only package tests for move legality, wins/draws,
immutability, determinism and an unbeatable computer as either player. A failed test result and the
previous source become inputs to the next model call. The engine controls this loop: at most four
iterations, stopping on passing tests or failing at the limit. A separate sandbox tests the
collected code again before assembling `index.html` with the supplied interface. Download the HTML
artifact and open it in a browser to play offline. The source and test report are separate artifacts
for inspection.

## Review a publication

The editor receives the original brief and the writer's draft in a separate model call. When the
pipeline pauses, open **Inbox** to inspect the draft and editorial review. Submit `approved: true`
and a `feedback` string, which may be empty when no further changes are needed. The response schema
rejects `approved: false`; cancel the run to stop it. Revision receives the brief, draft, editorial
review and human feedback. Artifacts are produced only after approval and revision.

## CLI and checks

From the repository root after starting the engine:

```sh
bin/knotra validate examples/starter/hello/pipeline.yaml --profile examples/local/profile.yaml
bin/knotra run examples/starter/hello/pipeline.yaml --profile local --wait
bin/knotra run examples/starter/research-dossier/pipeline.yaml --profile local --wait
bin/knotra run examples/starter/tic-tac-toe/pipeline.yaml --profile local --wait
bin/knotra run examples/starter/publication/pipeline.yaml --profile local
```

For publication, find the request with `bin/knotra requests list --run RUN_ID --all --json`, then
submit a `response.json` containing, for example:

```json
{ "approved": true, "feedback": "Use two short paragraphs and a warmer closing invitation." }
```

Use `bin/knotra requests respond REQUEST_ID --outputs response.json`, then
`bin/knotra runs watch RUN_ID`. Download any result with
`bin/knotra artifacts download ARTIFACT_ID --output FILE_NAME`.

`go test ./examples/starter` checks all four packages against the actual bundled profile without
model calls. `PYTHONDONTWRITEBYTECODE=1 python3 examples/starter/research_test.py` checks scoring,
eligibility, and rejection of invented evidence. Frontend contract tests verify packaging, localized
metadata and data bindings. Opt-in browser acceptance against a real engine exercises generated
results, independent tests, human approval and exact downloaded artifact bytes.
