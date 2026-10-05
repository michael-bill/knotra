# Repair a playable game

Start here before the [service fleet release gate](../release-gate/README.md). This exercise uses
the existing [game starter](../../starter/tic-tac-toe/pipeline.yaml), with a deliberately incorrect
module supplied through `initial_source`. Its opponent chooses the first empty square and can lose.
The engine tests this module first, supplies the actual failure and source to the model, then tests
the correction. The tests are fixed package files; the model cannot edit them.

From the repository root, with quickstart running:

```sh
bin/knotra run examples/starter/tic-tac-toe/pipeline.yaml \
  --profile local --inputs examples/showcase/game-repair/inputs.json
bin/knotra runs watch RUN_ID
bin/knotra artifacts list --run RUN_ID
bin/knotra artifacts download HTML_ARTIFACT_ID --output index.html
```

Use `--endpoint` if quickstart prints a different address. Open the downloaded HTML to play offline.
The independent final verifier also publishes `test-report.json`; `game.js` is retained separately.
There are at most four loop iterations, including the initial candidate. The run fails at the limit
and publishes no playable game if the verifier cannot establish the contract.

In desktop, inspect **build · Iteration 1 → check** for the failure and **Iteration 2 → generate**
for the supplied feedback. Then inspect the passing check and final verifier. A model may need more
than one correction. With no `initial_source`, the same pipeline generates from scratch, and can
pass in one iteration. This exercise does not portray its seeded mistake as a model-generated error.

To repair your own module, provide a JSON file with an `initial_source` string instead of the sample
inputs. The exact CommonJS API and test expectations are in
[game-contract.md](../../starter/tic-tac-toe/game-contract.md).
