import { expect, test, type APIResponse, type Page } from '@playwright/test';
import { createHash, randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import type { EngineArtifact, EngineRequest, EngineRun } from '../../src/lib/engine/types';

const endpoint = process.env.KNOTRA_E2E_ENDPOINT?.replace(/\/$/, '');
test.skip(
  !endpoint,
  'Set KNOTRA_E2E_ENDPOINT to run the publication starter on a real local engine.',
);
test.use({ locale: 'en-US' });
test.setTimeout(300_000);

async function navigate(page: Page, name: string) {
  await page
    .getByRole('navigation', { name: 'Main navigation' })
    .getByRole('button', { name, exact: true })
    .click();
}

async function runSnapshot(page: Page, id: string): Promise<EngineRun> {
  const response = await page.request.get(`${endpoint}/v1/runs/${encodeURIComponent(id)}`);
  expect(response.ok()).toBe(true);
  return (await response.json()).run;
}

async function requestsFor(page: Page, runId: string): Promise<EngineRequest[]> {
  const result: EngineRequest[] = [];
  let cursor: string | null = null;
  for (let count = 0; count < 100; count++) {
    const query: string = cursor ? `?cursor=${encodeURIComponent(cursor)}` : '';
    const response: APIResponse = await page.request.get(`${endpoint}/v1/requests${query}`);
    expect(response.ok()).toBe(true);
    const data: { items: EngineRequest[]; nextCursor: string | null } = await response.json();
    result.push(...data.items.filter((request: EngineRequest) => request.runId === runId));
    cursor = data.nextCursor;
    if (!cursor) return result;
  }
  throw new Error('Request pagination exceeded the acceptance test limit.');
}

async function exportBytes(page: Page, artifact: EngineArtifact, expected: Buffer) {
  await navigate(page, 'Artifacts');
  await page.getByRole('textbox', { name: 'Search engine artifacts' }).fill(artifact.id);
  await page.locator('.artifact-card').click();
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const exported = await download;
  const bytes = readFileSync((await exported.path())!);
  expect(bytes).toEqual(expected);
  expect(bytes.byteLength).toBe(artifact.size);
  expect(createHash('sha256').update(bytes).digest('hex')).toBe(artifact.sha256);
}

test('publication waits for approval, preserves editing feedback, and exports the revision with both reviews', async ({
  page,
}, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  let runId: string | undefined;
  try {
    await page.goto('/');
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page.getByRole('textbox', { name: 'Engine base URL', exact: true }).fill(endpoint!);
    await page.getByRole('button', { name: 'Save address' }).click();
    await page.getByRole('button', { name: 'Connect engine', exact: true }).click();
    await expect(page.getByText(/ · knotra.desktop\/1$/)).toBeVisible();
    await page
      .locator('.sidebar-pipelines > button')
      .filter({ hasText: 'From brief to reviewed publication' })
      .click();
    const title = `Publication acceptance ${randomUUID().slice(0, 8)}`;
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
    const accepted = await started;
    expect(accepted.ok()).toBe(true);
    runId = (await accepted.json()).run.id;
    await expect
      .poll(
        async () => {
          const run = await runSnapshot(page, runId!);
          if (['failed', 'cancelled'].includes(run.status)) return run.status;
          return run.instances.find((instance) => instance.nodeId === 'approve')?.status;
        },
        {
          timeout: 180_000,
          intervals: [1000],
        },
      )
      .toMatch(/^(waiting_human|failed|cancelled)$/);
    const waiting = await runSnapshot(page, runId!);
    expect(
      waiting.instances.find((instance) => instance.nodeId === 'approve')?.status,
      JSON.stringify(waiting.diagnostics),
    ).toBe('waiting_human');
    expect(waiting.artifacts).toEqual([]);
    expect(waiting.instances.find((instance) => instance.nodeId === 'revise')?.status).toBe(
      'pending',
    );
    const request = (await requestsFor(page, runId!)).find((request) => request.status === 'open')!;
    expect(request).toBeDefined();
    expect(request.inputs.values.brief).toBe(waiting.inputs.brief);
    expect(typeof request.inputs.values.draft).toBe('string');
    expect(request.inputs.values.editorial_review).toMatchObject({
      verdict: expect.stringMatching(/^(ready|needs_revision)$/),
      findings: expect.any(Array),
      corrections: expect.any(Array),
    });

    // The engine, not only the browser form, must enforce the publication gate.
    const denied = await page.request.post(
      `${endpoint}/v1/requests/${encodeURIComponent(request.id)}/response`,
      {
        headers: { 'Idempotency-Key': randomUUID() },
        data: { outputs: { approved: false, feedback: 'Do not export this draft.' } },
      },
    );
    expect(denied.status()).toBe(422);
    expect((await requestsFor(page, runId!)).find((item) => item.id === request.id)?.status).toBe(
      'open',
    );
    expect((await runSnapshot(page, runId!)).artifacts).toEqual([]);

    await navigate(page, 'Inbox');
    await page.locator('.request-list > button').filter({ hasText: runId! }).click();
    await expect(page.locator('.review-card .json-view').first()).toContainText('editorial_review');
    const feedback =
      'Use the exact heading "A clearer path to answers". End with the exact sentence "Tell us which guide helped you most." Preserve the verified launch facts and apply the editor corrections.';
    const answer = JSON.stringify({ approved: true, feedback });
    expect(String(request.inputs.values.draft)).not.toContain(
      'Tell us which guide helped you most.',
    );
    await page.getByRole('textbox', { name: 'Engine review response' }).fill(answer);
    await navigate(page, 'Pipelines');
    await expect(page.locator('.pipelines-page')).toBeVisible();
    await navigate(page, 'Inbox');
    await page.locator('.request-list > button').filter({ hasText: runId! }).click();
    await expect(page.getByRole('textbox', { name: 'Engine review response' })).toHaveValue(answer);
    await page.screenshot({
      path: testInfo.outputPath('publication-independent-review.png'),
      fullPage: true,
    });
    await page.getByRole('button', { name: 'Submit response', exact: true }).click();
    await expect
      .poll(async () => (await runSnapshot(page, runId!)).status, {
        timeout: 150_000,
        intervals: [1000],
      })
      .toMatch(/^(succeeded|failed|cancelled)$/);
    const run = await runSnapshot(page, runId!);
    await testInfo.attach('publication-run.json', {
      body: JSON.stringify(run, null, 2),
      contentType: 'application/json',
    });
    expect(run.status, JSON.stringify(run.diagnostics)).toBe('succeeded');
    expect(run.instances.every((instance) => instance.status === 'succeeded')).toBe(true);
    const text = String(run.outputs.text);
    expect(text).toContain('A clearer path to answers');
    expect(text).toContain('Tell us which guide helped you most.');
    expect(text).not.toBe(request.inputs.values.draft);
    const document = run.artifacts.find((artifact) => artifact.mediaType === 'text/markdown')!;
    const history = run.artifacts.find((artifact) => artifact.mediaType === 'application/json')!;
    expect(document).toBeDefined();
    expect(history).toBeDefined();
    const historyResponse = await page.request.get(
      `${endpoint}/v1/artifacts/${encodeURIComponent(history.id)}/content`,
    );
    expect(historyResponse.ok()).toBe(true);
    const historyBytes = await historyResponse.body();
    expect(JSON.parse(historyBytes.toString('utf8'))).toEqual({
      brief: run.inputs.brief,
      draft: request.inputs.values.draft,
      editorial_review: request.inputs.values.editorial_review,
      human_review: { approved: true, feedback },
      final_text: run.outputs.text,
    });
    await exportBytes(page, document, Buffer.from(text.trimEnd() + '\n'));
    await exportBytes(page, history, historyBytes);
    expect(errors).toEqual([]);
  } finally {
    if (runId) {
      const snapshot = await runSnapshot(page, runId);
      if (!['succeeded', 'failed', 'cancelled'].includes(snapshot.status)) {
        await page.request.post(`${endpoint}/v1/runs/${encodeURIComponent(runId)}/cancel`, {
          headers: { 'Idempotency-Key': randomUUID() },
          data: {},
        });
      }
    }
  }
});
