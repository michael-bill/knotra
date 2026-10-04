import { test, expect, type Page } from '@playwright/test';
import { createServer, type Server, type ServerResponse } from 'node:http';
import type { EngineEvent, EngineRun } from '../../src/lib/engine/types';

test.use({ locale: 'en-US' });
let server: Server;
let endpoint = '';
let mutations: string[] = [];
let streams: ServerResponse[] = [];
let historyRequests: string[] = [];
const at = (seconds: number, previous = false) =>
  new Date(Date.UTC(2026, 9, previous ? 3 : 4, 12, 0, seconds)).toISOString();

function fixtureRun(previous = false): EngineRun {
  const source = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: history-demo}
spec:
  models: {writer: {connection: local_model}}
  nodes:
    writer:
      type: llm
      llm:
        model: writer
        prompt: {text: '${previous ? 'Write a short answer.' : 'Write a detailed answer.'}'}
      outputs:
        answer: {schema: {type: string}}
  outputs:
    answer: {schema: {type: string}, bind: {from: nodes.writer.outputs.answer}}
`;
  return {
    id: previous ? 'earlier-run' : 'current-run',
    definitionId: previous ? 'earlier-definition' : 'current-definition',
    title: previous ? 'Earlier history fixture' : 'Current history fixture',
    profile: 'local',
    status: previous ? 'failed' : 'succeeded',
    createdAt: at(0, previous),
    updatedAt: at(20, previous),
    package: {
      entrypoint: 'pipeline.yaml',
      source,
      files: [{ path: 'pipeline.yaml', content: Buffer.from(source).toString('base64') }],
    },
    inputs: { question: previous ? 'OLD QUESTION' : 'CURRENT QUESTION' },
    inputArtifacts: {},
    outputs: previous ? { answer: 'EARLIER ANSWER' } : { answer: 'FINAL ANSWER' },
    artifacts: [],
    diagnostics: [],
    availableActions: [],
    instances: [
      {
        id: previous ? 'earlier-writer' : 'current-writer',
        nodeId: 'writer',
        scope: 'pipeline.yaml',
        graphPath: '/nodes/writer',
        nodeType: 'llm',
        status: previous ? 'failed' : 'succeeded',
        attemptId: previous ? 'earlier-writer.a1' : 'current-writer.a1',
        startedAt: at(4, previous),
        finishedAt: at(previous ? 9 : 20, previous),
        inputs: {
          values: { question: previous ? 'OLD QUESTION' : 'CURRENT QUESTION' },
          artifacts: {},
        },
        outputs: {
          values: { answer: previous ? 'EARLIER ANSWER' : 'FINAL ANSWER' },
          artifacts: {},
        },
      },
    ],
  };
}
const current = fixtureRun(),
  earlier = fixtureRun(true);
const history: EngineEvent[] = [
  {
    id: '1',
    runId: current.id,
    at: at(1),
    type: 'run',
    message: 'running',
    data: { status: 'running' },
  },
  ...['pending', 'ready', 'running'].map(
    (status, index) =>
      ({
        id: String(index + 2),
        runId: current.id,
        at: at(index + 2),
        type: 'node',
        message: status,
        instanceId: 'current-writer',
        attemptId: 'current-writer.a1',
        data: {
          status,
          nodeId: 'writer',
          scope: 'pipeline.yaml',
          graphPath: '/nodes/writer',
          nodeType: 'llm',
          ...(index > 0
            ? { inputs: { values: { question: 'CURRENT QUESTION' }, artifacts: {} } }
            : {}),
          ...(status === 'running' ? { startedAt: at(4) } : {}),
        },
      }) as EngineEvent,
  ),
  ...Array.from({ length: 101 }, (_, index) => ({
    id: String(index + 5),
    runId: current.id,
    at: at(5),
    type: 'model.delta',
    message: 'model.delta',
    instanceId: 'current-writer',
    data: { text: 'intermediate' },
  })),
  {
    id: '106',
    runId: current.id,
    at: at(20),
    type: 'node',
    message: 'succeeded',
    instanceId: 'current-writer',
    attemptId: 'current-writer.a1',
    data: {
      status: 'succeeded',
      nodeId: 'writer',
      scope: 'pipeline.yaml',
      graphPath: '/nodes/writer',
      startedAt: at(4),
      finishedAt: at(20),
      inputs: { values: { question: 'CURRENT QUESTION' }, artifacts: {} },
      outputs: { values: { answer: 'FINAL ANSWER' }, artifacts: {} },
    },
  },
  {
    id: '107',
    runId: current.id,
    at: at(20),
    type: 'run',
    message: 'succeeded',
    data: { status: 'succeeded' },
  },
];

function json(response: ServerResponse, value: unknown, status = 200) {
  response.writeHead(status, {
    'Content-Type': 'application/json',
    'Access-Control-Allow-Origin': '*',
  });
  response.end(JSON.stringify(value));
}

test.beforeEach(async ({ page }) => {
  mutations = [];
  streams = [];
  historyRequests = [];
  server = createServer((request, response) => {
    if (request.method === 'OPTIONS') {
      response.writeHead(204, {
        'Access-Control-Allow-Origin': '*',
        'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
        'Access-Control-Allow-Headers': 'Content-Type,Idempotency-Key,Last-Event-ID,Authorization',
      });
      response.end();
      return;
    }
    if (request.method !== 'GET') {
      mutations.push(request.url!);
      json(
        response,
        { code: 'READ_ONLY_FIXTURE', message: 'No mutations expected', diagnostics: [] },
        405,
      );
      return;
    }
    const url = new URL(request.url!, endpoint);
    if (url.pathname === '/v1/info')
      return json(response, {
        protocol: 'knotra.desktop/1',
        engineId: 'history-engine',
        principalId: 'history-user',
        version: 'fixture',
        capabilities: ['runs', 'events', 'history'],
      });
    if (url.pathname === '/v1/profiles')
      return json(response, { items: [{ id: 'local', title: 'Local', revision: 'fixture' }] });
    if (url.pathname === '/v1/resources') return json(response, { items: [] });
    if (['/v1/requests', '/v1/artifacts', '/v1/definitions'].includes(url.pathname))
      return json(response, { items: [], nextCursor: null });
    if (url.pathname === '/v1/runs')
      return json(response, { items: [current, earlier], nextCursor: null });
    if (url.pathname === '/v1/runs/current-run') return json(response, { run: current });
    if (url.pathname === '/v1/runs/earlier-run') return json(response, { run: earlier });
    if (url.pathname === '/v1/runs/current-run/history') {
      historyRequests.push(url.searchParams.get('cursor') ?? '');
      const offset = url.searchParams.get('cursor')
        ? history.findIndex((event) => event.id === url.searchParams.get('cursor')) + 1
        : 0;
      const items = history
        .slice(offset)
        .filter(
          (event) =>
            !url.searchParams.get('instanceId') ||
            event.instanceId === url.searchParams.get('instanceId'),
        )
        .slice(0, 100);
      return json(response, { items, nextCursor: items.length === 100 ? items.at(-1)!.id : null });
    }
    if (url.pathname === '/v1/runs/current-run/events') {
      response.writeHead(200, {
        'Content-Type': 'text/event-stream',
        'Access-Control-Allow-Origin': '*',
        'Cache-Control': 'no-cache',
      });
      response.write(': connected\n\n');
      streams.push(response);
      return;
    }
    return json(response, { code: 'NOT_FOUND', message: url.pathname, diagnostics: [] }, 404);
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  endpoint = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
  await page.goto('/');
});

test.afterEach(async () => {
  for (const stream of streams) stream.destroy();
  server.closeAllConnections();
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

async function openRun(page: Page) {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('textbox', { name: 'Engine base URL', exact: true }).fill(endpoint);
  await page.getByRole('button', { name: 'Save address' }).click();
  await page.getByRole('button', { name: 'Connect engine' }).click();
  await expect(page.getByText('history-engine · knotra.desktop/1')).toBeVisible();
  await page
    .getByRole('navigation', { name: 'Main navigation' })
    .getByRole('button', { name: 'Runs', exact: true })
    .click();
  await page.locator('.table-row').filter({ hasText: 'Current history fixture' }).click();
}

test('replays paginated history and hides future outputs when rewound', async ({
  page,
}, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await openRun(page);
  await page
    .locator('.underline-tabs')
    .getByRole('button', { name: 'Replay', exact: true })
    .click();
  await expect(page.getByText('Recorded history loaded', { exact: false })).toBeVisible();
  expect(historyRequests).toContain('100');
  await page.locator('.history-graph .react-flow__node[data-id="writer"]').click();
  const inspector = page.getByRole('complementary', { name: 'Historical node details' });
  await expect(inspector).toContainText('FINAL ANSWER');
  const position = page.getByRole('slider', { name: 'Recorded execution position' });
  await position.focus();
  await position.press('Home');
  for (let index = 0; index < 4; index++) await position.press('ArrowRight');
  await expect(position).toHaveValue('4');
  await expect(inspector).toContainText('CURRENT QUESTION');
  await expect(inspector).not.toContainText('FINAL ANSWER');
  await expect(inspector).toContainText('Running');
  await expect(inspector).toContainText('Not recorded at this point');
  await position.press('Home');
  await expect(inspector).toContainText('No state has been recorded');
  await position.press('End');
  await expect(inspector).toContainText('FINAL ANSWER');
  await page.screenshot({ path: testInfo.outputPath('saved-replay.png'), fullPage: true });
  await page.getByRole('button', { name: 'Return to live view' }).click();
  await expect(page.locator('.execution-observer')).toBeVisible();
  expect(mutations).toEqual([]);
  expect(errors).toEqual([]);
});

test('compares saved prompts, outputs and node statuses without issuing commands', async ({
  page,
}, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await openRun(page);
  await page
    .locator('.underline-tabs')
    .getByRole('button', { name: 'Compare runs', exact: true })
    .click();
  await page.getByLabel('Compare with', { exact: true }).selectOption('earlier-run');
  await expect(page.locator('.history-comparison-heading')).toContainText(
    'Earlier history fixture',
  );
  const prompts = page
    .locator('.history-change-section')
    .filter({ hasText: 'Prompts and instructions' });
  await prompts.locator('.history-value-change > summary').click();
  await expect(prompts).toContainText('Write a short answer.');
  await expect(prompts).toContainText('Write a detailed answer.');
  const outputs = page
    .locator('.history-change-section')
    .filter({ has: page.locator(':scope > summary').filter({ hasText: /^Outputs/ }) });
  await outputs.locator('.history-value-change > summary').click();
  await expect(outputs).toContainText('EARLIER ANSWER');
  await expect(outputs).toContainText('FINAL ANSWER');
  const node = page.locator('.history-node-row').filter({ hasText: 'writer' });
  await expect(node).toContainText('Failed');
  await expect(node).toContainText('Completed');
  await expect(node).toContainText('5.0 s');
  await expect(node).toContainText('16.0 s');
  await page.screenshot({ path: testInfo.outputPath('run-comparison.png'), fullPage: true });
  expect(mutations).toEqual([]);
  expect(errors).toEqual([]);
});
