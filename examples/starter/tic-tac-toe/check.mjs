import { readFileSync, writeFileSync } from 'node:fs';
import { verifyGame } from './game.test.mjs';

const source = JSON.parse(readFileSync(process.env.KNOTRA_INPUT_JSON, 'utf8')).values.source;
writeFileSync('game.js', source);
let passed = false;
let feedback;
try {
  const report = verifyGame(source);
  passed = true;
  feedback = `All ${report.checks} checks passed across ${report.positions} positions.`;
} catch (error) {
  // A candidate failing a game test is data for the next loop iteration. I/O or
  // process failures still fail the node; the final verifier independently gates HTML output.
  feedback = `Fixed game tests failed: ${String(error?.message ?? error)}`.slice(0, 2500);
}
writeFileSync(
  process.env.KNOTRA_OUTPUT_JSON,
  JSON.stringify({
    passed,
    feedback,
    source_text: source,
  }),
);
