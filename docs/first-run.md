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

## Research with approval

In the second terminal from the same directory:

```sh
./knotra run knotra-data/examples/research-dossier/review.yaml --profile local
./knotra runs list
./knotra runs watch RUN_ID
```

If you chose another `--listen`, pass client commands `--endpoint http://HOST:PORT`. The process
analyzes three fictional proposals. Each agent receives one source and only `files.read`. The code
checks quotes, prices, and deadlines against sources, calculates estimates, and applies
budget/deadline constraints. The model explains the calculated choice. This is a check of provided
materials, not a search for current market data.

In desktop, connect the printed address in Settings and select **Research, verify and approve**. For
preview, add the quickstart flag `--cors-origin http://127.0.0.1:1420`. Open the run graph: nested
iterations show individual agents, the inspector shows tools and verification results.

## Restart and approve

Wait for the review node to reach `waiting_human` and for its request to appear in Inbox. The
overall run can still show Running. Inbox provides the dossier and verified comparison. Save the
request ID:

```sh
./knotra requests list --run RUN_ID --all --json
```

Stop quickstart via Ctrl-C and repeat **the same command with the same directory**. Reconnect in
Settings if you reopened the client, then open the request again: its ID and files remain the same,
and research is not repeated. In desktop, fill in `approved`, `reviewer`, `comments`, or use CLI:

```sh
./knotra requests respond REQUEST_ID \
  --output 'approved=true' \
  --output 'reviewer="Your name"' \
  --output 'comments="Reviewed evidence and calculated scores."'
./knotra runs watch RUN_ID
./knotra artifacts list --run RUN_ID
./knotra artifacts download ARTIFACT_ID --output approved-dossier.zip
```

Approval publishes already viewed bytes. The ZIP includes source materials, the comparison, the
dossier and `review.json` with the reviewer, comments and hashes of the reviewed files. To refuse
publication, cancel the run. `approved: false` does not match the schema.

## Demonstration in 2–3 Minutes

[Watch the recorded run](media/review-recovery.mp4). It uses actual local services and an automated
acceptance reviewer. The capture starts after model inference; the graph, request, restart and files
are real.

1. Open the real run graph with research completed and publication waiting for review.
2. Inspect the source-reading agents and their saved tool calls.
3. Read the dossier in Inbox and check its calculated recommendation.
4. Stop and restart the engine. Open the same request and verified comparison again.
5. Approve, export the ZIP, and inspect the reviewer and file hashes in `review.json`.

[Implementation Checks](verification.md) separate real services from cloud fixtures.
[Pilot with Five Developers](pilot.md) checks whether this process solves a useful task.
