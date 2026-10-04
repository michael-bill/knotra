import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import vm from 'node:vm';

const lines = [
  [0, 1, 2],
  [3, 4, 5],
  [6, 7, 8],
  [0, 3, 6],
  [1, 4, 7],
  [2, 5, 8],
  [0, 4, 8],
  [2, 4, 6],
];
const empty = () => Array(9).fill(null);
const other = (player) => (player === 'X' ? 'O' : 'X');
function result(board) {
  for (const [a, b, c] of lines)
    if (board[a] && board[a] === board[b] && board[a] === board[c]) return board[a];
  return board.includes(null) ? null : 'draw';
}

export function verifyGame(source) {
  assert(Buffer.byteLength(source) <= 64 * 1024, 'game.js must be no larger than 64 KiB');
  const context = vm.createContext({}, { codeGeneration: { strings: false, wasm: false } });
  vm.runInContext('var module = { exports: {} }; var exports = module.exports;', context);
  new vm.Script(source, { filename: 'game.js' }).runInContext(context, { timeout: 2000 });
  for (const name of ['winner', 'makeMove', 'chooseMove'])
    assert.equal(
      vm.runInContext(`typeof module.exports.${name}`, context),
      'function',
      `${name} export`,
    );

  let checks = 0;
  let positions = 0;
  let completedGames = 0;
  const tests = [];
  function call(name, ...args) {
    // Arguments and return values cross as JSON, not host objects or host functions.
    const value = vm.runInContext(
      `(() => {
      const args = ${JSON.stringify(args)};
      const before = JSON.stringify(args[0]);
      const value = module.exports.${name}(...args);
      if (before !== JSON.stringify(args[0])) throw new Error('${name} mutated its input board');
      if ('${name}' === 'makeMove' && value === args[0]) throw new Error('makeMove must return a new array');
      return JSON.stringify(value);
    })()`,
      context,
      { timeout: name === 'chooseMove' ? 5000 : 500 },
    );
    checks++;
    assert.notEqual(value, undefined, `${name} returned undefined`);
    return JSON.parse(value);
  }
  function test(name, check) {
    const before = checks;
    try {
      check();
    } catch (error) {
      throw new Error(`${name}: ${String(error?.message ?? error)}`, { cause: error });
    }
    tests.push({ name, status: 'passed', checks: checks - before });
  }
  test('Recognizes all eight winning lines for both players', () => {
    for (const player of ['X', 'O'])
      for (const line of lines) {
        const board = empty();
        for (const index of line) board[index] = player;
        assert.equal(call('winner', board), player, `winning line ${line} for ${player}`);
      }
  });
  test('Distinguishes empty boards, unfinished games, draws, and full-board wins', () => {
    assert.equal(call('winner', empty()), null);
    assert.equal(call('winner', ['X', null, null, null, 'O', null, null, null, null]), null);
    assert.equal(call('winner', ['X', 'O', 'X', 'X', 'O', 'O', 'O', 'X', 'X']), 'draw');
    assert.equal(call('winner', ['X', 'O', 'X', 'O', 'X', 'O', 'O', 'X', 'X']), 'X');
  });
  test('Creates immutable legal moves and rejects invalid moves', () => {
    for (const player of ['X', 'O'])
      for (let index = 0; index < 9; index++) {
        const expected = empty();
        expected[index] = player;
        const actual = call('makeMove', empty(), index, player);
        assert(
          Array.isArray(actual) && actual.length === 9,
          'makeMove must return exactly nine cells',
        );
        assert.deepEqual(actual, expected, `makeMove must change only square ${index}`);
      }
    for (const [board, index, player] of [
      [empty(), -1, 'X'],
      [empty(), 9, 'X'],
      [empty(), 1.5, 'X'],
      [empty(), '1', 'X'],
      [empty(), 0, 'Z'],
      [['X', null, null, null, null, null, null, null, null], 0, 'O'],
      [['X', 'X', 'X', null, 'O', null, null, null, 'O'], 3, 'O'],
    ]) {
      assert.throws(
        () => call('makeMove', board, index, player),
        `makeMove(board=${JSON.stringify(board)}, index=${JSON.stringify(index)}, player=${JSON.stringify(player)}) must throw. Board result=${JSON.stringify(result(board))}. ${result(board) !== null ? 'The game is already over: reject every move when winner(board) !== null, even on an empty square.' : 'Reject an invalid index/player or an occupied square.'}`,
      );
      checks++;
    }
  });
  test('Returns null for terminal positions and takes wins before blocking', () => {
    for (const player of ['X', 'O']) {
      assert.equal(
        call('chooseMove', ['X', 'X', 'X', null, 'O', null, null, null, 'O'], player),
        null,
      );
      assert.equal(call('chooseMove', ['X', 'O', 'X', 'X', 'O', 'O', 'O', 'X', 'X'], player), null);
      const opponent = other(player);
      assert.equal(
        call(
          'chooseMove',
          [player, player, null, opponent, opponent, null, null, null, null],
          player,
        ),
        2,
      );
      assert.equal(
        call(
          'chooseMove',
          [opponent, opponent, null, player, null, null, null, player, null],
          player,
        ),
        2,
      );
    }
  });
  for (const computer of ['X', 'O'])
    test(`Never loses as ${computer} against any legal opponent continuation`, () => {
      const visited = new Set();
      function play(board, turn) {
        const key = board.map((cell) => cell ?? '-').join('') + turn;
        if (visited.has(key)) return;
        visited.add(key);
        positions++;
        const outcome = result(board);
        assert.equal(call('winner', board), outcome, `winner for ${key}`);
        if (outcome !== null) {
          assert.notEqual(
            outcome,
            other(computer),
            `Computer ${computer} lost: board=${JSON.stringify(board)}, winner=${outcome}`,
          );
          completedGames++;
          return;
        }
        const moves = board.flatMap((cell, index) => (cell === null ? [index] : []));
        if (turn === computer) {
          const move = call('chooseMove', board, computer);
          assert(
            Number.isInteger(move) && moves.includes(move),
            `illegal computer move ${move} on ${key}`,
          );
          assert.equal(
            call('chooseMove', board, computer),
            move,
            `nondeterministic choice on ${key}`,
          );
          moves.splice(0, moves.length, move);
        }
        for (const move of moves) {
          const next = [...board];
          next[move] = turn;
          assert.deepEqual(call('makeMove', board, move, turn), next);
          play(next, other(turn));
        }
      }
      play(empty(), 'X');
    });
  return {
    passed: true,
    tests,
    checks,
    positions,
    completedGames,
    sourceSha256: createHash('sha256').update(source).digest('hex'),
    sourceBytes: Buffer.byteLength(source),
  };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    assert(process.argv[2], 'Usage: node /package/game.test.mjs /workspace/game.js');
    console.log(JSON.stringify(verifyGame(readFileSync(process.argv[2], 'utf8')), null, 2));
  } catch (error) {
    console.error(`GAME TEST FAILED: ${String(error?.message ?? error)}`);
    process.exitCode = 1;
  }
}
