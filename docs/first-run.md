# Your first useful workflow

For a developer who wants to check an AI process with artifacts and human involvement. Docker,
Compose, and Ollama are required; the prebuilt CLI includes examples and helpers.

## Get the first file

Unpack the [CLI archive](https://github.com/michael-bill/knotra/releases/latest) and open its
directory:

```sh
./knotra doctor
./knotra quickstart --dir "$PWD/knotra-data"
```

Leave the terminal running. Quickstart prints the engine address and the directory with
`greeting.txt`. The first run loads infrastructure images and, if they are not present, the model.
Repeating the command uses the previous greeting. To build from a repository, use
`make build helper` and `bin/knotra`.

## Generate a playable game

In a second terminal from the same archive directory:

```sh
./knotra run knotra-data/examples/tic-tac-toe/pipeline.yaml --profile local
./knotra runs list
./knotra runs watch RUN_ID
./knotra artifacts list --run RUN_ID
./knotra artifacts download HTML_ARTIFACT_ID --output index.html
```

Open `index.html` in your browser. The model implements the fixed game API; the engine runs tests
for wins, draws, legal moves, immutability, deterministic choices and an unbeatable opponent as both
X and O. A failed test and the candidate source become inputs to the next iteration. The engine
stops when tests pass or fails after four attempts. A separate sandbox verifies the final source
again before packaging the game. A first attempt can pass without a correction.

In desktop, connect the printed address in Settings and select **Build a playable game**. For
browser preview, add quickstart's `--cors-origin http://127.0.0.1:1420` flag. In the run observer,
select a **build · Iteration** using **Graph and iteration**, then inspect **check** under **Inputs
& outputs**. Its `passed` and `feedback` come from executed tests. Open **generate** in the next
iteration to inspect the previous code and feedback in the model's input.

For a visible correction, use the repository's
[repair exercise](../examples/showcase/game-repair/README.md). Its intentionally incorrect initial
module fails the same tests before the model receives it. The optional `initial_source` input also
accepts your own existing game module; leaving it empty keeps generation from scratch.

## Check a service release

Next use the repository's [service fleet release gate](../examples/showcase/release-gate/README.md).
This larger package is separate from the five bundled starters. Its instructions pack a selected
repository directory, upload it and start the pipeline. Six parallel service lanes execute actual
regression tests, compare API schemas and enforce configuration policy. Agents review only the
resulting evidence, and code checks their quotations. A failing gate blocks the run before agent
review or human authorization.

To use your own services, supply their inventory and repository snapshot, and configure the
documented check interfaces for your test runner, API contracts and deployment policy. The workflow
produces an authorized evidence packet.

## Restart and approve

Wait for **release_authorization** to reach `waiting_human` and its request to appear in Inbox. The
overall run can still show Running. Review the dossier and executed checks, then save the request
ID:

```sh
./knotra requests list --run RUN_ID --all --json
```

Stop quickstart via Ctrl-C and repeat **the same command with the same directory**. Reconnect in
Settings if needed, then open the saved request again: its identity and files remain available. Fill
in `approved`, `reviewer`, `change_ticket` and `comments`, or use CLI:

```sh
./knotra requests respond REQUEST_ID \
  --output 'approved=true' \
  --output 'reviewer="Your name"' \
  --output 'change_ticket="CHANGE-1042"' \
  --output 'comments="Reviewed executed gates and risk recommendations."'
./knotra runs watch RUN_ID
./knotra artifacts list --run RUN_ID
./knotra artifacts download ZIP_ARTIFACT_ID --output authorized-release.zip
```

Approval publishes the exact reviewed bytes. The ZIP contains the repository snapshot, check
results, agent reviews, dossier and `authorization.json` with the reviewer, ticket and hashes. To
refuse publication, cancel the run; `approved: false` does not match the schema. Reviewer and ticket
are recorded response fields, not authenticated identities or verified ticket-system records.

[Workflow screenshots](../README.md#workflow-in-pictures) follow the game repair and release gate.
[Implementation Checks](verification.md) record actual executions and their boundaries.
