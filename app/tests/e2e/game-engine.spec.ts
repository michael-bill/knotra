import { expect, test, type Page } from '@playwright/test';
import { createHash, randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import type { EngineArtifact, EngineEvent, EngineRun } from '../../src/lib/engine/types';
import { record } from '../../src/lib/types';

const endpoint = process.env.KNOTRA_E2E_ENDPOINT?.replace(/\/$/, '');
test.skip(
  !endpoint,
  'Set KNOTRA_E2E_ENDPOINT to a real engine with the local Qwen and Node profile.',
);
test.setTimeout(600_000);
test.use({ locale: 'en-US' });

async function navigate(page: Page, name: string) {
  await page
    .getByRole('navigation', { name: 'Main navigation' })
    .getByRole('button', { name, exact: true })
    .click();
}

test('engine code-test loop builds an independently verified game that plays from its exported HTML', async ({
  page,
  browser,
}, testInfo) => {
  let runId: string | undefined;
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  try {
    await page.goto('/');
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page.getByRole('textbox', { name: 'Engine base URL' }).fill(endpoint!);
    await page.getByRole('button', { name: 'Save address' }).click();
    await page.getByRole('button', { name: 'Connect engine' }).click();
    await expect(page.getByText(/ · knotra.desktop\/1$/)).toBeVisible();
    await page
      .locator('.app-sidebar')
      .getByRole('button', { name: 'Build a playable game', exact: true })
      .click();
    await page.getByRole('button', { name: 'Run', exact: true }).click();
    await page.getByRole('combobox', { name: 'Engine profile' }).selectOption('local');
    await page.getByRole('button', { name: 'Check with engine' }).click();
    await expect(page.getByRole('dialog')).toContainText('Engine admission checks passed');
    const started = page.waitForResponse(
      (response) =>
        response.url() === `${endpoint}/v1/runs` && response.request().method() === 'POST',
    );
    await page.getByRole('button', { name: 'Start run', exact: true }).click();
    const { run: startedRun } = await (await started).json();
    runId = startedRun.id;
    console.info(`Started tic-tac-toe: ${runId}`);
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect
      .poll(
        async () => {
          const response = await page.request.get(`${endpoint}/v1/runs/${runId}`);
          expect(response.ok()).toBe(true);
          return (await response.json()).run.status;
        },
        { timeout: 540_000, intervals: [1000, 2000, 5000] },
      )
      .toMatch(/^(succeeded|failed|cancelled)$/);
    const { run }: { run: EngineRun } = await (
      await page.request.get(`${endpoint}/v1/runs/${runId}`)
    ).json();
    await testInfo.attach('game-run.json', {
      body: JSON.stringify(run, null, 2),
      contentType: 'application/json',
    });
    expect(run.status).toBe('succeeded');
    await expect(page.locator('.page-heading .status')).toHaveText('Completed');
    const loop = run.instances.find((instance) => instance.nodeId === 'build')!;
    const verifier = run.instances.find((instance) => instance.nodeId === 'verify')!;
    expect(loop?.nodeType).toBe('loop');
    expect(run.outputs.iterations).toBeGreaterThanOrEqual(1);
    expect(run.outputs.iterations).toBeLessThanOrEqual(4);
    const generations = run.instances.filter(
      (instance) => instance.parentInstanceId === loop.id && instance.nodeId === 'generate',
    );
    const checks = run.instances
      .filter((instance) => instance.parentInstanceId === loop.id && instance.nodeId === 'check')
      .sort((a, b) => (a.iterationIndex ?? 0) - (b.iterationIndex ?? 0));
    expect(generations).toHaveLength(run.outputs.iterations as number);
    expect(checks).toHaveLength(generations.length);
    expect(checks.map((instance) => instance.outputs?.values.passed)).toEqual([
      ...checks.slice(0, -1).map(() => false),
      true,
    ]);
    for (let index = 1; index < generations.length; index++) {
      const generation = generations.find((instance) => instance.iterationIndex === index)!;
      expect(generation.inputs?.values.feedback).toBe(checks[index - 1].outputs?.values.feedback);
      expect(generation.inputs?.values.previous_source).toBe(
        checks[index - 1].outputs?.values.source_text,
      );
    }
    expect(run.instances.every((instance) => instance.status === 'succeeded')).toBe(true);
    const artifacts = new Map(run.artifacts.map((artifact) => [artifact.name, artifact]));
    const finalSource = record(verifier.inputs?.artifacts.source);
    expect(finalSource.id).toEqual(expect.any(String));
    async function bytes(artifact: EngineArtifact) {
      expect(artifact).toBeDefined();
      const response = await page.request.get(`${endpoint}/v1/artifacts/${artifact.id}/content`);
      expect(response.ok()).toBe(true);
      const content = await response.body();
      expect(content.length).toBe(artifact.size);
      expect(createHash('sha256').update(content).digest('hex')).toBe(artifact.sha256);
      return content;
    }
    const finalSourceArtifact = run.artifacts.find((artifact) => artifact.id === finalSource.id)!;
    const source = await bytes(finalSourceArtifact);
    const html = await bytes(artifacts.get('index.html')!);
    const report = JSON.parse((await bytes(artifacts.get('test-report.json')!)).toString('utf8'));
    expect(report.passed).toBe(true);
    expect(report.tests).toHaveLength(6);
    expect(report.tests.every((entry: { status: string }) => entry.status === 'passed')).toBe(true);
    expect(report.checks).toBeGreaterThan(100);
    expect(report.positions).toBeGreaterThan(100);
    expect(report.sourceSha256).toBe(createHash('sha256').update(source).digest('hex'));
    expect(html.toString('utf8')).not.toContain('__GAME_BASE64__');
    const events: EngineEvent[] = [];
    let cursor: string | null = null;
    do {
      const query = new URLSearchParams();
      if (cursor) query.set('cursor', cursor);
      const response = await page.request.get(`${endpoint}/v1/runs/${runId}/history?${query}`);
      expect(response.ok()).toBe(true);
      const history = await response.json();
      events.push(...history.items);
      cursor = history.nextCursor;
    } while (cursor);
    for (const generation of generations)
      expect(
        events.some(
          (event) => event.type === 'model.completed' && event.instanceId === generation.id,
        ),
      ).toBe(true);
    await testInfo.attach('game-history.json', {
      body: JSON.stringify(events, null, 2),
      contentType: 'application/json',
    });
    const scope = page.locator('.execution-scope-picker select');
    await scope.selectOption({ index: (await scope.locator('option').count()) - 1 });
    await page.locator('.react-flow__node[data-id="generate"]').click();
    await expect(page.locator('.execution-inspector')).toContainText('module.exports');
    await page.screenshot({ path: testInfo.outputPath('game-pipeline.png'), fullPage: true });
    const artifact: EngineArtifact = artifacts.get('index.html')!;
    await navigate(page, 'Artifacts');
    await page.getByRole('textbox', { name: 'Search engine artifacts' }).fill(artifact.id);
    await page.locator('.artifact-card').click();
    await expect(page.locator('.artifact-text')).toContainText('<!doctype html>');
    const download = page.waitForEvent('download');
    await page.getByRole('button', { name: 'Export', exact: true }).click();
    const exported = await download;
    expect(exported.suggestedFilename()).toBe('index.html');
    expect(readFileSync((await exported.path())!)).toEqual(html);

    // Generated code runs in an isolated browser context with networking denied.
    const gameContext = await browser.newContext({
      locale: 'en-US',
      viewport: { width: 1100, height: 1000 },
    });
    try {
      await gameContext.route('**/*', (route) =>
        route.request().url() === 'https://game.test/'
          ? route.fulfill({ contentType: 'text/html', body: html })
          : route.abort(),
      );
      const gamePage = await gameContext.newPage();
      gamePage.on('pageerror', (error) => errors.push(error.message));
      await gamePage.goto('https://game.test/');
      await expect(gamePage.locator('#board button')).toHaveCount(9);
      await expect(gamePage.locator('#status')).toHaveText('Your turn · X');
      await gamePage.getByRole('button', { name: 'Square 5: empty', exact: true }).click();
      await expect(gamePage.locator('#board button[data-mark="X"]')).toHaveCount(1);
      await expect(gamePage.locator('#board button[data-mark="O"]')).toHaveCount(1);
      await expect(gamePage.locator('#status')).toHaveText('Your turn · X');
      await gamePage.locator('#restart').click();
      await expect(gamePage.locator('#board button[data-mark=""]')).toHaveCount(9);
      await gamePage.locator('#as-o').click();
      await expect(gamePage.locator('#board button[data-mark="X"]')).toHaveCount(1);
      await expect(gamePage.locator('#status')).toHaveText('Your turn · O');
      await gamePage.locator('#language').selectOption('ru');
      await expect(gamePage.locator('#status')).toHaveText('Ваш ход · O');
      await gamePage.screenshot({
        path: testInfo.outputPath('generated-tic-tac-toe.png'),
        fullPage: true,
      });
    } finally {
      await gameContext.close();
    }
    expect(errors).toEqual([]);
  } finally {
    if (runId) {
      const response = await page.request.get(`${endpoint}/v1/runs/${runId}`);
      if (response.ok()) {
        const { run } = await response.json();
        if (!['succeeded', 'failed', 'cancelled'].includes(run.status))
          await page.request.post(`${endpoint}/v1/runs/${runId}/cancel`, {
            headers: { 'Idempotency-Key': randomUUID() },
            data: {},
          });
      }
    }
  }
});
