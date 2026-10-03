import { test, expect, type Page } from '@playwright/test';
import { createServer, type Server, type IncomingMessage, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import type { EngineRun, EngineRequest } from '../../src/lib/engine/types';

let uploaded: { descriptor: any; bytes: Buffer } | undefined;
let principal = 'test-user';
let server: Server;
let address = '';
let runs: EngineRun[] = [];
let requests: EngineRequest[] = [];
let writes: { path: string; key: string; body: any }[] = [];
let accepted = new Map<string, any>();
let streams: ServerResponse[] = [];
let reconnectHeaders: string[] = [];
let loseStart = false;
let badHash = false;
let artifactBytes = Buffer.from([0, 255, 128, 10]);
let lastPackage: any;
let eventNumber = 0;
const descriptor = () => ({
  id: 'artifact-1',
  name: 'result.bin',
  mediaType: 'application/octet-stream',
  size: artifactBytes.length,
  sha256: badHash ? '0'.repeat(64) : createHash('sha256').update(artifactBytes).digest('hex'),
  origin: { runId: 'run-1', instanceId: 'root/review', attemptId: 'attempt-1' },
});

function json(response: ServerResponse, value: unknown, status = 200) {
  response.writeHead(status, {
    'Content-Type': 'application/json',
    'Access-Control-Allow-Origin': '*',
    'Access-Control-Expose-Headers': 'Content-Type',
  });
  response.end(JSON.stringify(value));
}

async function read(request: IncomingMessage) {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  return chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : {};
}

function emit(message: string) {
  const event = {
    id: `event-${++eventNumber}`,
    runId: 'run-1',
    at: new Date().toISOString(),
    type: 'run.updated',
    message,
    instanceId: 'root/review',
  };
  for (const stream of streams)
    if (!stream.destroyed) {
      stream.write(`id: ${event.id}\ndata: ${JSON.stringify(event)}\n\n`);
      stream.write(`id: ${event.id}\ndata: ${JSON.stringify(event)}\n\n`);
    }
  return event;
}

async function handle(request: IncomingMessage, response: ServerResponse) {
  const path = request.url!.split('?')[0];
  if (request.method === 'OPTIONS') {
    response.writeHead(204, {
      'Access-Control-Allow-Origin': '*',
      'Access-Control-Allow-Methods': 'GET,POST,OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type,Idempotency-Key,Last-Event-ID,Authorization',
    });
    response.end();
    return;
  }
  if (path === '/v1/info')
    return json(response, {
      protocol: 'knotra.desktop/1',
      engineId: 'test-engine',
      principalId: principal,
      version: 'contract-fixture',
      capabilities: ['definitions', 'runs', 'requests', 'artifacts', 'events', 'resolution'],
    });
  if (path === '/v1/profiles')
    return json(response, {
      items: [{ id: 'local', title: 'Local profile', revision: 'sha256:profile1' }],
    });
  if (path === '/v1/resources')
    return json(response, {
      items: [
        {
          id: 'm1',
          kind: 'model',
          title: 'Test model',
          capabilities: ['structured'],
          status: 'available',
        },
      ],
    });
  if (path === '/v1/runs/run-1/events') {
    response.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Access-Control-Allow-Origin': '*',
      'Cache-Control': 'no-cache',
    });
    response.write(': heartbeat\n\n');
    streams.push(response);
    reconnectHeaders.push(String(request.headers['last-event-id'] ?? ''));
    const timer = setInterval(() => response.write(': ping\n\n'), 1000);
    response.on('close', () => clearInterval(timer));
    return;
  }
  if (request.method === 'GET') {
    if (path === '/v1/runs') {
      const cursor = new URL(request.url!, address).searchParams.get('cursor');
      return json(
        response,
        cursor
          ? { items: runs.slice(1), nextCursor: null }
          : { items: runs.slice(0, 1), nextCursor: runs.length > 1 ? 'page-2' : null },
      );
    }
    if (path === '/v1/requests') return json(response, { items: requests, nextCursor: null });
    if (path === '/v1/artifacts')
      return json(response, {
        items: [...(runs.length ? [descriptor()] : []), ...(uploaded ? [uploaded.descriptor] : [])],
        nextCursor: null,
      });
    if (path === '/v1/artifacts/uploaded-1')
      return json(response, { artifact: uploaded!.descriptor });
    if (path === '/v1/artifacts/uploaded-1/content') {
      response.writeHead(200, {
        'Content-Type': 'application/octet-stream',
        'Access-Control-Allow-Origin': '*',
      });
      response.end(uploaded!.bytes);
      return;
    }
    if (path === '/v1/artifacts/artifact-1') return json(response, { artifact: descriptor() });
    if (path === '/v1/artifacts/artifact-1/content') {
      response.writeHead(200, {
        'Content-Type': 'application/octet-stream',
        'Access-Control-Allow-Origin': '*',
      });
      response.end(artifactBytes);
      return;
    }
    if (path === '/v1/runs/run-1') return json(response, { run: runs[0] });
    return json(response, { code: 'NOT_FOUND', message: 'Not found.' }, 404);
  }
  const body = await read(request);
  const key = String(request.headers['idempotency-key'] ?? '');
  if (path !== '/v1/packages/validate') {
    writes.push({ path, key, body });
    if (!key)
      return json(
        response,
        { code: 'IDEMPOTENCY_REQUIRED', message: 'Missing operation ID.' },
        400,
      );
    if (accepted.has(key)) return json(response, accepted.get(key));
  }
  if (path === '/v1/packages/validate') {
    if (body.inputs.reject)
      return json(response, {
        valid: false,
        diagnostics: [
          {
            severity: 'error',
            code: 'INPUT_INVALID',
            phase: 'input',
            message: 'Rejected input.',
            path: '/inputs',
            file: 'pipeline.yaml',
            line: 1,
            column: 1,
          },
        ],
      });
    return json(response, { valid: true, diagnostics: [] });
  }
  if (path === '/v1/definitions') {
    lastPackage = body.package;
    const result = { definition: { id: 'definition-1' } };
    accepted.set(key, result);
    return json(response, result);
  }
  if (path === '/v1/runs') {
    const run: EngineRun = {
      id: 'run-1',
      definitionId: body.definitionId,
      title: 'Future engine workflow',
      status: 'waiting_human',
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      profile: body.profile,
      package: lastPackage,
      inputs: body.inputs,
      inputArtifacts: body.artifacts,
      outputs: {},
      artifacts: [],
      instances: [
        {
          id: 'root/review',
          nodeId: 'review',
          scope: '',
          status: 'waiting_human',
          attemptId: 'attempt-1',
        },
      ],
      diagnostics: [],
      availableActions: ['cancel'],
    };
    runs = [run];
    requests = [
      {
        id: 'request-1',
        runId: run.id,
        instanceId: 'root/review',
        attemptId: 'attempt-1',
        status: 'open',
        prompt: 'Confirm this actual saved request.',
        createdAt: run.createdAt,
        deadline: '2030-01-01T00:00:00Z',
        inputs: { values: body.inputs, artifacts: {} },
        responseSchema: {
          type: 'object',
          required: ['feedback'],
          properties: { feedback: { type: 'string', minLength: 1 } },
          additionalProperties: false,
        },
      },
    ];
    const result = { run };
    accepted.set(key, result);
    if (loseStart) {
      loseStart = false;
      response.writeHead(200, {
        'Content-Type': 'application/json',
        'Access-Control-Allow-Origin': '*',
      });
      response.end('{');
      return;
    }
    return json(response, result);
  }
  if (path === '/v1/requests/request-1/response') {
    if (!requests.some((request) => request.id === 'request-1' && request.status === 'open'))
      return json(response, { code: 'REQUEST_CLOSED', message: 'Request already answered.' }, 409);
    requests[0].status = 'answered';
    runs[0].status = 'succeeded';
    runs[0].instances[0].status = 'succeeded';
    runs[0].outputs = { feedback: body.outputs.feedback };
    runs[0].artifacts = [descriptor()];
    runs[0].availableActions = [];
    emit('Review accepted.');
    const result = { accepted: true, requestId: 'request-1' };
    accepted.set(key, result);
    return json(response, result);
  }
  if (path === '/v1/runs/run-1/cancel') {
    runs[0].status = 'cancelled';
    runs[0].availableActions = [];
    const result = { accepted: true, runId: 'run-1' };
    accepted.set(key, result);
    return json(response, result);
  }
  if (path.endsWith('/resolve') || path.endsWith('/resume')) {
    runs[0].status = 'running';
    runs[0].instances[0].status = 'running';
    runs[0].availableActions = ['cancel'];
    const result = { accepted: true, runId: 'run-1' };
    accepted.set(key, result);
    return json(response, result);
  }
  if (path === '/v1/artifacts') {
    const bytes = Buffer.from(body.content, 'base64');
    uploaded = {
      bytes,
      descriptor: {
        ...descriptor(),
        id: 'uploaded-1',
        name: body.name,
        mediaType: body.mediaType,
        size: bytes.length,
        sha256: createHash('sha256').update(bytes).digest('hex'),
        origin: {},
      },
    };
    const result = { artifact: uploaded.descriptor };
    accepted.set(key, result);
    return json(response, result);
  }
  json(response, { code: 'NOT_FOUND', message: 'Not found.' }, 404);
}

test.beforeEach(async ({ page }) => {
  uploaded = undefined;
  principal = 'test-user';
  runs = [];
  requests = [];
  writes = [];
  accepted = new Map();
  streams = [];
  reconnectHeaders = [];
  loseStart = false;
  badHash = false;
  eventNumber = 0;
  server = createServer((request, response) => {
    void handle(request, response).catch((error) => {
      response.destroy(error);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  address = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
  await page.goto('/');
});

test.afterEach(async () => {
  for (const stream of streams) stream.destroy();
  server.closeAllConnections();
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

async function nav(page: Page, name: string) {
  await page
    .getByRole('navigation', { name: 'Main navigation' })
    .getByRole('button', { name, exact: true })
    .click();
}

async function connect(page: Page) {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('textbox', { name: 'Engine base URL', exact: true }).fill(address);
  await page.getByRole('button', { name: 'Save address' }).click();
  await page.getByRole('button', { name: 'Connect engine' }).click();
  await expect(page.getByText('test-engine · knotra.desktop/1')).toBeVisible();
  await expect(page.getByText('1 profiles · 1 resources')).toBeVisible();
}

async function start(page: Page, values = '{"topic":"Backend verification"}') {
  await nav(page, 'Pipelines');
  await page.getByRole('button', { name: 'Run', exact: true }).click();
  await page.getByRole('textbox', { name: 'Workflow input values' }).fill(values);
  await page.getByRole('button', { name: 'Start run', exact: true }).click();
}

test('engine admission, immutable package, live events, request response and binary artifact export', async ({
  page,
}) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await connect(page);
  await nav(page, 'Resources');
  await expect(page.getByText('Test model', { exact: true })).toBeVisible();
  await start(page, '{"reject":true}');
  await expect(page.getByRole('dialog')).toContainText('INPUT_INVALID');
  expect(writes).toHaveLength(0);
  await page
    .getByRole('textbox', { name: 'Workflow input values' })
    .fill('{"topic":"Backend verification"}');
  await page.getByRole('button', { name: 'Check with engine' }).click();
  await expect(page.getByRole('dialog')).toContainText('Engine admission checks passed');
  await page.getByRole('button', { name: 'Start run', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Future engine workflow' })).toBeVisible();
  expect(writes.map((write) => write.path)).toEqual(['/v1/definitions', '/v1/runs']);
  expect(writes.every((write) => write.key)).toBe(true);
  expect(lastPackage.source).toContain('kind: Pipeline');
  await page.getByRole('button', { name: 'Timeline', exact: true }).click();
  await expect.poll(() => streams.filter((stream) => !stream.destroyed).length).toBe(1);
  emit('Once, despite duplicate delivery.');
  await expect(page.getByText('Once, despite duplicate delivery.', { exact: true })).toHaveCount(1);
  const before = reconnectHeaders.length;
  streams
    .filter((stream) => !stream.destroyed)
    .at(-1)!
    .end();
  await expect.poll(() => reconnectHeaders.length).toBeGreaterThan(before);
  expect(reconnectHeaders.at(-1)).toBe('event-1');
  await page.getByRole('button', { name: 'Snapshot', exact: true }).click();
  await expect(page.getByRole('textbox', { name: 'Engine immutable snapshot' })).toContainText(
    'kind: Pipeline',
  );
  await page.getByRole('button', { name: 'Review request', exact: true }).click();
  await expect(page.getByText('Confirm this actual saved request.')).toBeVisible();
  await page.getByRole('textbox', { name: 'Engine review response' }).fill('{}');
  await page.getByRole('button', { name: 'Submit response' }).click();
  await expect(page.getByRole('alert')).toContainText('JSON schema');
  expect(writes.filter((write) => write.path.includes('/response'))).toHaveLength(0);
  await page
    .getByRole('textbox', { name: 'Engine review response' })
    .fill('{"feedback":"Verified"}');
  await page.getByRole('button', { name: 'Submit response' }).click();
  await expect(page.getByRole('heading', { name: 'No open engine requests' })).toBeVisible();
  expect(writes.at(-1)?.path).toBe('/v1/requests/request-1/response');
  expect(writes.at(-1)?.body).toEqual({ outputs: { feedback: 'Verified' } });
  await nav(page, 'Artifacts');
  await page.getByRole('button', { name: /result.bin/ }).click();
  await expect(page.locator('.artifact-text')).toContainText('Binary artifact');
  const result = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const exported = await result;
  expect(exported.suggestedFilename()).toBe('result.bin');
  const chunks: Buffer[] = [];
  for await (const chunk of (await exported.createReadStream())!) chunks.push(chunk);
  expect(Buffer.concat(chunks)).toEqual(artifactBytes);
  for (const theme of ['Light', 'Dark']) {
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page.getByRole('radio', { name: theme, exact: true }).click();
    await page.setViewportSize({ width: 1000, height: 680 });
    for (const section of ['Runs', 'Inbox', 'Artifacts']) {
      await nav(page, section);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(
        true,
      );
      await page.screenshot({ path: `/private/tmp/knotra-engine-${section}-${theme}.png` });
    }
  }
  expect(errors).toEqual([]);
});

test('lost start response recovers the exact operation after a browser restart', async ({
  page,
}) => {
  await connect(page);
  loseStart = true;
  await start(page);
  await expect(page.getByRole('dialog')).toContainText('saved operation ID');
  await page.getByRole('button', { name: 'Close dialog' }).click();
  const first = writes.find((write) => write.path === '/v1/runs')!;
  expect(runs).toHaveLength(1);
  await page.reload();
  await connect(page);
  await nav(page, 'Runs');
  await expect(page.getByText(/command\(s\) awaiting confirmation/)).toBeVisible();
  await page.getByRole('button', { name: /^Reconcile start/ }).click();
  await expect(page.getByText(/command\(s\) awaiting confirmation/)).toHaveCount(0);
  const retried = writes.filter((write) => write.path === '/v1/runs');
  expect(retried).toHaveLength(2);
  expect(retried[1]).toEqual(first);
  expect(runs).toHaveLength(1);
});

test('cancellation, continuation and unknown-outcome resolution remain different commands', async ({
  page,
}) => {
  await connect(page);
  await start(page);
  await expect(page.getByRole('heading', { name: 'Future engine workflow' })).toBeVisible();
  await page.getByRole('button', { name: 'Request cancellation' }).click();
  await expect(page.locator('.page-heading .status')).toContainText('Cancelled');
  runs[0].status = 'waiting_resolution';
  runs[0].instances[0].status = 'waiting_resolution';
  runs[0].availableActions = ['resolve'];
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await page.getByRole('button', { name: 'Resolve unknown outcome' }).click();
  await page.getByRole('combobox', { name: 'Confirmed outcome' }).selectOption('not_started');
  await page
    .getByRole('textbox', { name: 'Resolution evidence' })
    .fill('External service confirmed no operation was submitted.');
  await page.getByRole('button', { name: 'Submit resolution' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  expect(writes.at(-1)?.path).toBe('/v1/runs/run-1/instances/root%2Freview/resolve');
  expect(writes.at(-1)?.body.outcome).toBe('not_started');
  runs[0].availableActions = ['resume'];
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await page.getByRole('button', { name: 'Resume saved attempt' }).click();
  await expect.poll(() => writes.at(-1)?.path).toBe('/v1/runs/run-1/resume');
});

test('corrupt artifact bytes cannot be previewed or exported', async ({ page }) => {
  await connect(page);
  await start(page);
  await expect(page.getByRole('heading', { name: 'Future engine workflow' })).toBeVisible();
  badHash = true;
  await nav(page, 'Artifacts');
  await page.getByRole('button', { name: /result.bin/ }).click();
  await expect(page.getByRole('alert')).toContainText('SHA-256');
  expect(await page.locator('.artifact-text').textContent()).not.toContain('Binary artifact');
});

test('remote transport must be authenticated HTTPS; tokens never reach workspace persistence', async ({
  page,
}) => {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('textbox', { name: 'Engine base URL' }).fill('http://engine.example');
  await page.getByRole('textbox', { name: 'Engine access token' }).fill('never-store-this');
  await page.getByRole('button', { name: 'Connect engine' }).click();
  await expect(page.getByRole('alert')).toContainText('HTTPS');
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('never-store-this');
  await expect(page.getByRole('textbox', { name: 'Engine access token' })).toHaveValue('');
});

test('workspace backup restore replaces drafts only after review and leaves execution independent', async ({
  page,
}) => {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const backupEvent = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export backup' }).click();
  const backup = await backupEvent;
  const stream = await backup.createReadStream();
  const chunks: Buffer[] = [];
  for await (const chunk of stream!) chunks.push(chunk);
  const bytes = Buffer.concat(chunks);
  const before = JSON.parse(bytes.toString());
  before.workspaces = before.workspaces.slice(0, 1);
  before.workspaces[0].source = before.workspaces[0].source.replace(
    'Research brief',
    'Restored workflow',
  );
  before.theme = 'light';
  await page.getByLabel('Restore workspace backup').setInputFiles({
    name: 'backup.json',
    mimeType: 'application/json',
    buffer: Buffer.from(JSON.stringify(before)),
  });
  await expect(page.getByRole('dialog')).toContainText('1 pipeline drafts');
  await expect
    .poll(() =>
      page.evaluate(
        () => JSON.parse(localStorage.getItem('knotra.workspace.v1') ?? '{}').workspaces?.length,
      ),
    )
    .toBe(3);
  await page.getByRole('button', { name: 'Restore backup', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Restored workflow', exact: true })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Restored workflow', exact: true })).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
});

test('an engine account switch cannot reuse another account command journal', async ({ page }) => {
  await connect(page);
  loseStart = true;
  await start(page);
  await expect(page.getByRole('dialog')).toContainText('saved operation ID');
  await page.getByRole('button', { name: 'Close dialog' }).click();
  principal = 'another-user';
  runs = [];
  requests = [];
  await connect(page);
  await nav(page, 'Runs');
  await expect(page.getByText(/command\(s\) awaiting confirmation/)).toHaveCount(0);
  principal = 'test-user';
  await connect(page);
  await nav(page, 'Runs');
  await expect(page.getByText(/command\(s\) awaiting confirmation/)).toBeVisible();
  expect(writes.filter((write) => write.path === '/v1/runs')).toHaveLength(1);
});

test('input artifact upload registers exact bytes and can be exported without text conversion', async ({
  page,
}) => {
  await connect(page);
  await nav(page, 'Artifacts');
  const bytes = Buffer.from([0, 1, 255, 128, 0, 13, 10]);
  await page
    .getByLabel('Upload engine artifact')
    .setInputFiles({ name: 'input.bin', mimeType: 'application/octet-stream', buffer: bytes });
  await expect(page.getByRole('button', { name: /input.bin/ })).toBeVisible();
  expect(writes.at(-1)?.path).toBe('/v1/artifacts');
  expect(Buffer.from(writes.at(-1)?.body.content, 'base64')).toEqual(bytes);
  expect(writes.at(-1)?.key).toBeTruthy();
  await page.getByRole('button', { name: /input.bin/ }).click();
  await expect(page.locator('.artifact-text')).toContainText('Binary artifact');
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const exported = await download;
  const chunks: Buffer[] = [];
  for await (const chunk of (await exported.createReadStream())!) chunks.push(chunk);
  expect(Buffer.concat(chunks)).toEqual(bytes);
});
