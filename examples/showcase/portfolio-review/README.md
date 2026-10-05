# Six teams, one evidence portfolio

This showcase evaluates six different help-center launch plans against the same three fictional
offers. Independent agents extract evidence, code checks that evidence against the original files,
and six branches apply different budgets, deadlines and priorities. Paired decision briefs converge
into one dossier for human approval.

The root graph contains 24 executable nodes:

- Three source-reading agents, each granted only `files.read`.
- Three verifiers checking quotations, monthly prices, setup times and control/support codes.
- Six parallel scenario comparisons using the starter workflow's scoring rules.
- Six scenario reports containing the calculated selection, eligibility and next checks.
- Three decision briefs comparing launch feasibility, cost/ownership and support/design priorities.
- One portfolio assembler, one human review and one publication step.

The scenarios include a three-day launch, a lean startup, engineering ownership, managed support,
design control and an impossible combination of budget and deadline. The last case must select no
offer. Scores and eligibility are calculated by code; the agents cannot invent a winner.

## Run

Start the local quickstart engine with Docker, Compose and Ollama. Its `local` profile supplies
`model_main` (`qwen3.5:9b` with tool calling) and the isolated `python_box` sandbox. From the
repository root:

```sh
bin/knotra validate examples/showcase/portfolio-review/pipeline.yaml \
  --profile examples/local/profile.yaml
bin/knotra run examples/showcase/portfolio-review/pipeline.yaml --profile local
```

Pass `--endpoint http://HOST:PORT` if the engine uses a different address. Inspect the run graph and
the agents' saved model/tool activity in the app. Each source agent reads its assigned package file
and submits structured evidence through `knotra_finish`.

When Inbox receives the portfolio review, inspect the dossier and machine-readable comparison. The
request and its files persist across engine restarts. Approve with `approved: true`, your `reviewer`
name and `comments`, or cancel the run if the evidence is insufficient.

Publication preserves the reviewed dossier bytes and adds `review.json` with the reviewer, comments
and reviewed file hashes. The ZIP contains all six scenario reports, three briefs, the original
source files, the dossier and the comparison.

The pipeline caps execution at six concurrent nodes, 128 node instances, 40 model calls, 100 tool
calls and 30 minutes. Only the three extraction nodes call the model; all later analysis and
packaging use Python in isolated Docker sandboxes.

## Check the deterministic stages

```sh
python3 -m unittest discover -s examples/showcase/portfolio-review/tests -v
```

These checks use the actual bundled source files. They reject corrupted quotations and facts,
confirm all six expected selections including the infeasible scenario, and check the portfolio ZIP
and byte-preserving approval record. The sources are exercise data, not current vendor facts.
