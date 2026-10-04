import { expect, test, type Page, type TestInfo } from '@playwright/test';
import { createHash, randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { unzipSync } from 'fflate';
import type { EngineArtifact, EngineEvent, EngineRun } from '../../src/lib/engine/types';
import { record } from '../../src/lib/types';

const endpoint = process.env.KNOTRA_E2E_ENDPOINT?.replace(/\/$/, '');

test.skip(
  !endpoint,
  'Set KNOTRA_E2E_ENDPOINT to a real engine with the bundled local Qwen profile.',
);
test.describe.configure({ mode: 'serial' });
test.setTimeout(360_000);
test.use({ locale: 'en-US' });

let activeRunId: string | undefined;
let pageErrors: string[];

async function navigate(page: Page, name: string) {
  await page
    .getByRole('navigation', { name: 'Main navigation' })
    .getByRole('button', { name, exact: true })
    .click();
}

test.beforeEach(async ({ page }) => {
  activeRunId = undefined;
  pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.goto('/');
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('textbox', { name: 'Engine base URL', exact: true }).fill(endpoint!);
  await page.getByRole('button', { name: 'Save address' }).click();
  await page.getByRole('button', { name: 'Connect engine' }).click();
  await expect(page.getByText(/ · knotra.desktop\/1$/)).toBeVisible();
  await expect(page.getByText(/\d+ profiles · \d+ resources/)).toBeVisible();
});

test.afterEach(async ({ page }, testInfo) => {
  if (activeRunId) {
    const run = await readRun(page, activeRunId);
    await testInfo.attach('starter-run.json', {
      body: JSON.stringify(run, null, 2),
      contentType: 'application/json',
    });
    if (!['succeeded', 'failed', 'cancelled'].includes(run.status)) {
      // A failed browser assertion must not leave its model consuming resources
      // while the next test starts. This only cancels the run this test created.
      await page.request.post(`${endpoint}/v1/runs/${activeRunId}/cancel`, {
        headers: { 'Idempotency-Key': randomUUID() },
        data: {},
      });
    }
  }
  expect(pageErrors).toEqual([]);
});

async function startStarter(page: Page, starterTitle: string) {
  await page
    .locator('.app-sidebar')
    .getByRole('button', { name: starterTitle, exact: true })
    .click();
  await expect(page.getByRole('heading', { name: starterTitle, exact: true })).toBeVisible();
  const title = `${starterTitle} ${randomUUID().slice(0, 8)}`;
  await page.getByRole('button', { name: 'Workflow', exact: true }).click();
  await page.getByRole('textbox', { name: 'Workflow title', exact: true }).fill(title);
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await page.getByRole('button', { name: 'Run', exact: true }).click();
  await page.getByRole('combobox', { name: 'Engine profile' }).selectOption('local');
  await page.getByRole('button', { name: 'Check with engine' }).click();
  await expect(page.getByRole('dialog')).toContainText('Engine admission checks passed');
  const started = page.waitForResponse(
    (response) =>
      response.url() === `${endpoint}/v1/runs` && response.request().method() === 'POST',
  );
  await page.getByRole('button', { name: 'Start run', exact: true }).click();
  const response = await started;
  expect(response.ok()).toBe(true);
  const { run } = (await response.json()) as { run: EngineRun };
  activeRunId = run.id;
  console.info(`Started ${starterTitle}: ${run.id}`);
  await expect(page.getByRole('dialog')).toHaveCount(0);
  return { id: run.id, title };
}

async function readRun(page: Page, runId: string): Promise<EngineRun> {
  const response = await page.request.get(`${endpoint}/v1/runs/${encodeURIComponent(runId)}`);
  expect(response.ok()).toBe(true);
  return (await response.json()).run;
}

async function completedRun(page: Page, runId: string) {
  await expect(page.locator('.page-heading .status')).toHaveText(/^(Completed|Failed|Cancelled)$/, {
    timeout: 300_000,
  });
  const run = await readRun(page, runId);
  expect(run.status, JSON.stringify(run.diagnostics)).toBe('succeeded');
  expect(run.instances.every((instance) => instance.status === 'succeeded')).toBe(true);
  expect(run.diagnostics.filter((diagnostic) => diagnostic.severity === 'error')).toEqual([]);
  return run;
}

async function historyFor(page: Page, run: EngineRun, instanceId: string): Promise<EngineEvent[]> {
  const events: EngineEvent[] = [];
  const cursors = new Set<string>();
  let cursor: string | null = null;
  do {
    const query = new URLSearchParams({ instanceId });
    if (cursor) query.set('cursor', cursor);
    const response = await page.request.get(`${endpoint}/v1/runs/${run.id}/history?${query}`);
    expect(response.ok()).toBe(true);
    const pageData = await response.json();
    events.push(...pageData.items);
    cursor = pageData.nextCursor;
    if (cursor) {
      expect(cursors.has(cursor)).toBe(false);
      cursors.add(cursor);
    }
  } while (cursor);
  return events;
}

async function exportedArtifact(page: Page, artifact: EngineArtifact, expected: Buffer) {
  await navigate(page, 'Artifacts');
  await page.getByRole('textbox', { name: 'Search engine artifacts' }).fill(artifact.id);
  await page.locator('.artifact-card').click();
  if (artifact.mediaType === 'application/zip')
    await expect(page.locator('.artifact-text')).toContainText('Binary artifact');
  else await expect(page.locator('.artifact-text')).toHaveText(expected.toString('utf8'));
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const result = await download;
  expect(result.suggestedFilename()).toBe(artifact.name);
  const bytes = readFileSync((await result.path())!);
  expect(bytes).toEqual(expected);
  expect(bytes.length).toBe(artifact.size);
  expect(createHash('sha256').update(bytes).digest('hex')).toBe(artifact.sha256);
}

async function attachHistory(testInfo: TestInfo, events: EngineEvent[]) {
  await testInfo.attach('starter-history.json', {
    body: JSON.stringify(events, null, 2),
    contentType: 'application/json',
  });
}

test('hello starter streams a real response and exports its exact text bytes', async ({
  page,
}, testInfo) => {
  const started = await startStarter(page, 'Hello, model');
  const run = await completedRun(page, started.id);
  expect(run.inputs.name).toBe('Knotra');
  const greeting = run.outputs.greeting;
  expect(typeof greeting).toBe('string');
  expect(greeting).toContain('Knotra');
  const instance = run.instances.find((item) => item.nodeId === 'greet')!;
  const history = await historyFor(page, run, instance.id);
  await attachHistory(testInfo, history);
  const delta = history.findIndex((event) => event.type === 'model.delta');
  const completed = history.findIndex((event) => event.type === 'model.completed');
  expect(delta).toBeGreaterThan(-1);
  expect(completed).toBeGreaterThan(delta);
  expect(record(history[completed].data).model).toBe('qwen3.5:9b');
  await page.locator('.react-flow__node[data-id="greet"]').click();
  await expect(page.locator('.execution-response pre')).toContainText(greeting as string);
  const artifact = run.artifacts.find((item) => item.name === 'greeting.txt')!;
  expect(artifact?.mediaType).toBe('text/plain');
  await page.screenshot({ path: testInfo.outputPath('starter-hello.png'), fullPage: true });
  await exportedArtifact(page, artifact, Buffer.from(`${greeting}\n`));
});

test('research starter compares independent quoted evidence and exports a reproducible dossier', async ({
  page,
}, testInfo) => {
  const started = await startStarter(page, 'Research dossier from source materials');
  const run = await completedRun(page, started.id);
  const investigators = run.instances.filter((item) => item.nodeId === 'extract');
  expect(investigators).toHaveLength(3);
  expect(new Set(investigators.map((item) => item.id)).size).toBe(3);
  expect(investigators.map((item) => item.iterationIndex).sort()).toEqual([0, 1, 2]);
  const foreach = run.instances.find((item) => item.nodeId === 'evidence')!;
  expect(investigators.every((item) => item.parentInstanceId === foreach.id)).toBe(true);
  const sources = new Map(
    run.package.files
      .filter((file) => file.path.startsWith('sources/'))
      .map((file) => [file.path, Buffer.from(file.content, 'base64')]),
  );
  expect(sources.size).toBe(3);
  const allHistory: EngineEvent[] = [];
  const normalize = (text: string) => text.replace(/\s+/g, ' ').trim();
  const seenSources = new Set<string>();
  for (const investigator of investigators) {
    const evidence = record(investigator.outputs!.values.evidence);
    const path = evidence.source as string;
    expect(sources.has(path)).toBe(true);
    seenSources.add(path);
    const source = sources.get(path)!.toString('utf8');
    for (const quote of evidence.quotes as string[])
      expect(normalize(source)).toContain(normalize(quote));
    const history = await historyFor(page, run, investigator.id);
    allHistory.push(...history);
    const read = history.find((event) => {
      const data = record(event.data);
      return (
        event.type === 'tool.started' &&
        data.name === 'files.read' &&
        record(data.arguments).path === path &&
        record(data.arguments).root === 'package'
      );
    });
    expect(read, `Agent must actually read ${path}`).toBeDefined();
    const result = history.find(
      (event) => event.type === 'tool.completed' && event.operationId === read!.operationId,
    );
    expect(result).toBeDefined();
    expect(record(result!.data).isError).not.toBe(true);
    expect(JSON.stringify(record(result!.data).result)).toContain('Monthly cost:');
    expect(
      history.filter((event) => event.type === 'agent.iteration').length,
    ).toBeGreaterThanOrEqual(2);
  }
  expect(seenSources.size).toBe(3);
  await attachHistory(testInfo, allHistory);
  const comparison = record(
    run.instances.find((item) => item.nodeId === 'compare')!.outputs!.values.comparison,
  );
  const ranked = comparison.ranked as Record<string, unknown>[];
  expect(ranked.map((item) => item.source)).toEqual([
    'sources/hosted.md',
    'sources/cms.md',
    'sources/custom.md',
  ]);
  expect(ranked.map((item) => item.eligible)).toEqual([true, true, false]);
  expect(ranked[0].weighted_score).toBeCloseTo(60.62, 2);
  expect(ranked[1].weighted_score).toBeCloseTo(59.12, 2);
  expect(ranked[2].weighted_score).toBeCloseTo(49.38, 2);
  const decision = record(comparison.decision);
  expect(decision.selected_source).toBe('sources/hosted.md');
  expect(decision.eligible_sources).toEqual(['sources/hosted.md', 'sources/cms.md']);
  expect(decision.ineligible_sources).toEqual(['sources/custom.md']);
  const recommendation = run.outputs.recommendation as string;
  expect(recommendation.length).toBeGreaterThan(100);
  await page.locator('.react-flow__node[data-id="synthesize"]').click();
  await expect
    .poll(async () => {
      const content = await page.locator('.execution-response pre').textContent();
      try {
        return JSON.parse(content ?? '{}').recommendation;
      } catch {
        return undefined;
      }
    })
    .toBe(recommendation);
  await page.screenshot({
    path: testInfo.outputPath('starter-research-dossier.png'),
    fullPage: true,
  });
  const artifacts = new Map(run.artifacts.map((artifact) => [artifact.name, artifact]));
  expect([...artifacts.keys()].sort()).toEqual(['comparison.json', 'dossier.md', 'dossier.zip']);
  const bytes = new Map<string, Buffer>();
  for (const name of ['dossier.md', 'comparison.json', 'dossier.zip']) {
    const artifact = artifacts.get(name)!;
    const response = await page.request.get(`${endpoint}/v1/artifacts/${artifact.id}/content`);
    expect(response.ok()).toBe(true);
    bytes.set(name, await response.body());
    await exportedArtifact(page, artifact, bytes.get(name)!);
  }
  const dossier = bytes.get('dossier.md')!.toString('utf8');
  expect(dossier).toContain(recommendation);
  expect(dossier).toContain('Fictional bundled sample offers');
  expect(dossier).toContain('Selected source: `sources/hosted.md`');
  expect(dossier).toContain('Eligible sources: `sources/hosted.md`, `sources/cms.md`.');
  for (const path of sources.keys()) expect(dossier).toContain(path);
  expect(JSON.parse(bytes.get('comparison.json')!.toString('utf8'))).toEqual(comparison);
  const bundle = unzipSync(bytes.get('dossier.zip')!);
  expect(Object.keys(bundle).sort()).toEqual([
    'comparison.json',
    'dossier.md',
    ...[...sources.keys()].sort(),
  ]);
  for (const [path, original] of sources) expect(Buffer.from(bundle[path])).toEqual(original);
  expect(Buffer.from(bundle['dossier.md'])).toEqual(bytes.get('dossier.md'));
  expect(Buffer.from(bundle['comparison.json'])).toEqual(bytes.get('comparison.json'));
});
