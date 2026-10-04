# Generate the game logic

Return one JSON object with a `source` string containing the complete `game.js` module. Write code
only inside that string: no Markdown fences, explanation, tests, HTML, or tool calls. Use the
supplied brief and plan. If `previous_source` and `feedback` are nonempty, correct that
implementation using the exact test failure. The engine runs the fixed tests after every response;
you do not choose when to test, retry, or finish.

Export `module.exports = { winner, makeMove, chooseMove };` from dependency-free JavaScript. Keep
the implementation concise, around 100 lines. No imports, require, process, timers, network, DOM, or
top-level actions. The same module runs in Node tests and in the provided browser shell.

A board is an array of exactly nine cells, each null, "X", or "O", indexed 0..8 left-to-right,
top-to-bottom. X plays first. None of the functions may mutate an input board. Tests use valid cell
values; do not reject boards based on move counts or whose turn it is.

## Required functions

- `winner(board)`: check all eight rows/columns/diagonals. Return "X" or "O" for a win, "draw" for a
  full board without a winner, or null. A win takes precedence over a full-board draw.
- `makeMove(board, index, player)`: return a new nine-cell array with that empty cell replaced.
  Throw Error for a noninteger/out-of-range index, an occupied cell, invalid player (not X/O), or
  terminal board. A terminal board means `winner(board) !== null`, including either winner or a
  draw. BEFORE copying or placing a mark, explicitly check `winner(board) !== null` and throw
  `new Error("Game is over")`. Even an empty square is illegal after a win. For example,
  `makeMove(["X","X","X",null,"O",null,null,null,"O"], 3, "O")` MUST throw because X already won.
  Use Number.isInteger. Copy with board.slice(); do not append or pad cells.
- `chooseMove(board, player)`: return null if terminal, otherwise a legal integer cell index. Always
  take an immediate win before other moves. Never lose as either X or O. Repeated identical calls
  must return the same move. Use the exact minimax recipe below without caching or pruning.

## Minimax recipe

`other(X) = O`; `other(O) = X`. Keep `me` fixed as the original computer mark; only `turn`
alternates. The recursive helper returns a SCORE, never an index. Use `let` for variables that are
reassigned.

```text
score(position, turn, me):
  outcome = winner(position)
  if outcome equals me: return +1
  if outcome equals other(me): return -1
  if outcome equals "draw": return 0
  best = -2 if turn equals me, otherwise +2
  for each empty index in position:
    next = copy position; set next[index] = turn
    value = score(next, other(turn), me)
    best = max(best, value) if turn equals me, otherwise min(best, value)
  return best

chooseMove(board, me):
  if winner(board) is not null: return null
  for each empty index in ascending order:
    next = copy board; set next[index] = me
    if winner(next) equals me: return index
  bestScore = -2; bestIndex = null
  for each empty index in ascending order:
    next = copy board; set next[index] = me
    value = score(next, other(me), me)
    if value > bestScore:
      bestScore = value
      bestIndex = index
  return bestIndex
```

Do not reverse the signs for X and O: scoring is relative to `me`, and the outer chooseMove always
MAXIMIZES that score. No memoization is needed for nine cells. In particular, board.join("") loses
empty positions and must not be used as a cache key.

The independent tests cover all winning lines, draws, legal and illegal moves, immutability,
immediate wins/blocks, repeatable choices, and every legal opponent continuation against the
computer as X and O. Return the complete corrected source, not a patch or a test report.
