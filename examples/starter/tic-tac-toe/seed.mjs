import { readFileSync, writeFileSync } from 'node:fs';

const { source } = JSON.parse(readFileSync(process.env.KNOTRA_INPUT_JSON, 'utf8')).values;
writeFileSync(process.env.KNOTRA_OUTPUT_JSON, JSON.stringify({ source }));
