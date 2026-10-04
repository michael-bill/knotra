import { readFileSync, writeFileSync } from 'node:fs';
import { verifyGame } from './game.test.mjs';

const source = readFileSync('incoming/game.js', 'utf8');
const report = verifyGame(source);
const shell = readFileSync(new URL('./shell.html', import.meta.url), 'utf8');
const html = shell
  .replace('__GAME_BASE64__', Buffer.from(source).toString('base64'))
  .replace('__REPORT_BASE64__', Buffer.from(JSON.stringify(report)).toString('base64'));
writeFileSync('index.html', html);
writeFileSync('test-report.json', JSON.stringify(report, null, 2) + '\n');
writeFileSync(
  process.env.KNOTRA_OUTPUT_JSON,
  JSON.stringify({
    summary: `Game verified: ${report.checks} checks across ${report.positions} positions. Download index.html and open it in a browser to play offline.`,
  }),
);
