import { test, expect, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { randomUUID } from 'node:crypto';
import { zipSync } from 'fflate';

const endpoint = process.env.KNOTRA_E2E_ENDPOINT;

test.skip(!endpoint, 'Set KNOTRA_E2E_ENDPOINT to a real Go engine with the local profile.');

test.setTimeout(180_000);

async function navigate(page: Page, name: string) {
  await page
    .getByRole('navigation', { name: 'Main navigation' })
    .getByRole('button', { name, exact: true })
    .click();
}

async function connect(page: Page) {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('textbox', { name: 'Engine base URL', exact: true }).fill(endpoint!);
  await page.getByRole('button', { name: 'Save address' }).click();
  await page.getByRole('button', { name: 'Connect engine' }).click();
  await expect(page.getByText(/ · knotra.desktop\/1$/)).toBeVisible();
  await expect(page.getByText(/\d+ profiles · \d+ resources/)).toBeVisible();
}

async function importPackage(page: Page, files: Record<string, string>, title: string) {
  await navigate(page, 'Pipelines');
  await page.getByLabel('Import pipeline package', { exact: true }).setInputFiles({
    name: 'workflow.zip',
    mimeType: 'application/zip',
    buffer: Buffer.from(
      zipSync(
        Object.fromEntries(
          Object.entries(files).map(([path, text]) => [path, new TextEncoder().encode(text)]),
        ),
      ),
    ),
  });
  const chooser = page.getByRole('dialog').filter({ hasText: 'Choose the entrypoint.' });
  if (Object.keys(files).filter((path) => path.endsWith('.yaml')).length > 1) {
    await chooser.getByRole('combobox').selectOption('pipeline.yaml');
    await chooser.getByRole('button', { name: 'Open package', exact: true }).click();
  }
  await expect(page.getByRole('heading', { name: title, exact: true })).toBeVisible();
}

async function start(page: Page) {
  await page.getByRole('button', { name: 'Run', exact: true }).click();
  await page.getByRole('combobox', { name: 'Engine profile' }).selectOption('local');
  await page.getByRole('button', { name: 'Check with engine' }).click();
  await expect(page.getByRole('dialog')).toContainText('Engine admission checks passed');
  const started = page.waitForResponse(
    (response) =>
      response.url() === `${endpoint}/v1/runs` && response.request().method() === 'POST',
  );
  await page.getByRole('button', { name: 'Start run', exact: true }).click();
  const { run } = await (await started).json();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  return run;
}

test.beforeEach(async ({ page }) => {
  await page.goto('/');
  await connect(page);
});

test('real Ollama run publishes an artifact and replays history on a fresh client', async ({
  page,
  browser,
}, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const title = `Desktop greeting ${randomUUID().slice(0, 8)}`;
  await navigate(page, 'Pipelines');
  await page
    .locator('.app-sidebar')
    .getByRole('button', { name: 'New pipeline', exact: true })
    .click();
  await page
    .getByRole('dialog')
    .getByRole('button', { name: 'Building blocks', exact: true })
    .click();
  await page
    .getByRole('dialog')
    .getByRole('button', { name: /Local Ollama greeting/ })
    .click();
  await page.getByRole('button', { name: 'Workflow', exact: true }).click();
  await page.getByRole('textbox', { name: 'Workflow title', exact: true }).fill(title);
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByRole('heading', { name: title, exact: true })).toBeVisible();
  const startedRun = await start(page);
  await expect(page.locator('.page-heading .status')).toHaveText('Completed', { timeout: 120_000 });
  await page.getByRole('button', { name: 'Outputs', exact: true }).click();
  await expect(page.locator('.json-view').first()).toContainText('greeting');
  await expect(page.locator('.json-view').last()).toContainText('sha256');

  const { run } = await (await page.request.get(`${endpoint}/v1/runs/${startedRun.id}`)).json();
  expect(run.outputs.greeting).toEqual(expect.any(String));
  await page.getByRole('button', { name: 'Live graph', exact: true }).click();
  await page.locator('.react-flow__node[data-id="greet"]').click();
  await expect(page.locator('.execution-response pre')).toContainText(run.outputs.greeting);
  await expect(page.locator('.execution-usage')).toContainText('Tokens');
  const instance = run.instances.find((value: { nodeId: string }) => value.nodeId === 'greet');
  const historyResponse = await page.request.get(
    `${endpoint}/v1/runs/${run.id}/history?instanceId=${encodeURIComponent(instance.id)}`,
  );
  const history = (await historyResponse.json()).items;
  const firstDelta = history.findIndex((event: { type: string }) => event.type === 'model.delta');
  const completed = history.findIndex(
    (event: { type: string }) => event.type === 'model.completed',
  );
  expect(firstDelta).toBeGreaterThan(-1);
  expect(completed).toBeGreaterThan(firstDelta);
  await page.screenshot({ path: testInfo.outputPath('real-qwen-live-graph.png'), fullPage: true });
  await navigate(page, 'Artifacts');
  await page.getByRole('textbox', { name: 'Search engine artifacts' }).fill(run.artifacts[0].id);
  await page.locator('.artifact-card').click();
  await expect(page.locator('.artifact-text')).toContainText(run.outputs.greeting);
  const downloaded = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const exported = await downloaded;
  expect(exported.suggestedFilename()).toBe('greeting.txt');
  expect(readFileSync((await exported.path())!)).toEqual(Buffer.from(`${run.outputs.greeting}\n`));

  // A separate context has neither localStorage metadata nor IndexedDB event IDs/cursor.
  const freshContext = await browser.newContext({ locale: 'en-US' });
  try {
    const fresh = await freshContext.newPage();
    fresh.on('pageerror', (error) => errors.push(error.message));
    await fresh.goto(new URL('/', page.url()).toString());
    await connect(fresh);
    await navigate(fresh, 'Runs');
    await fresh.locator('.table-row').filter({ hasText: title }).click();
    await fresh.getByRole('button', { name: 'Timeline', exact: true }).click();
    await expect.poll(() => fresh.locator('.timeline-event').count()).toBeGreaterThan(4);
    await expect(fresh.locator('.timeline')).toContainText('succeeded');
  } finally {
    await freshContext.close();
  }
  expect(errors).toEqual([]);
});

test('real human review, child package and binary artifact round-trip use the same contract', async ({
  page,
}) => {
  const title = `Desktop review ${randomUUID().slice(0, 8)}`;
  const bytes = Buffer.from([0, 255, 128, 13, 10, 0]);
  await navigate(page, 'Artifacts');
  const name = `input-${randomUUID().slice(0, 8)}.bin`;
  await page
    .getByLabel('Upload engine artifact')
    .setInputFiles({ name, mimeType: 'application/octet-stream', buffer: bytes });
  await expect(page.getByRole('button', { name: new RegExp(name) })).toBeVisible();

  await importPackage(
    page,
    {
      'pipeline.yaml': `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: desktop-review, title: ${title}}
spec:
  files: [children/copy.yaml]
  inputs:
    source: {artifact: {mediaTypes: [application/octet-stream]}}
  nodes:
    review:
      type: human
      inputs:
        source: {artifact: {mediaTypes: [application/octet-stream]}, bind: {from: inputs.source}}
      human: {prompt: {text: Approve the binary copy.}}
      outputs:
        approved: {schema: {type: boolean, const: true}}
    copy:
      type: pipeline
      inputs:
        approved: {schema: {type: boolean}, bind: {from: nodes.review.outputs.approved}}
        source: {artifact: {mediaTypes: [application/octet-stream]}, bind: {from: inputs.source}}
      pipeline:
        file: children/copy.yaml
        permissions: {models: [], mcp: {}, sandboxes: [python_box], secrets: []}
  outputs:
    file: {artifact: {mediaTypes: [application/octet-stream]}, bind: {from: nodes.copy.outputs.file}}
`,
      'children/copy.yaml': `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: copy-binary}
spec:
  files: [scripts/copy.py]
  sandboxes: {work: {profile: python_box}}
  inputs:
    approved: {schema: {type: boolean, const: true}}
    source: {artifact: {mediaTypes: [application/octet-stream]}}
  nodes:
    write:
      type: code
      sandbox: work
      inputs:
        source: {artifact: {mediaTypes: [application/octet-stream]}, mount: source.bin, bind: {from: inputs.source}}
      code: {command: [python3, /package/scripts/copy.py]}
      outputs:
        file: {artifact: {mediaTypes: [application/octet-stream]}, collect: {path: copy.bin, mediaType: application/octet-stream}}
  outputs:
    file: {artifact: {mediaTypes: [application/octet-stream]}, bind: {from: nodes.write.outputs.file}}
`,
      'scripts/copy.py': `import json, os
from pathlib import Path
context = json.loads(Path(os.environ['KNOTRA_INPUT_JSON']).read_text())
Path('copy.bin').write_bytes(Path(context['artifacts']['source']['path']).read_bytes())
Path(os.environ['KNOTRA_OUTPUT_JSON']).write_text('{}')
`,
    },
    title,
  );
  await page.getByRole('button', { name: 'Run', exact: true }).click();
  await page.getByRole('combobox', { name: 'Engine profile' }).selectOption('local');
  const artifacts = page.getByRole('combobox', { name: 'Artifact handle for source' });
  const artifactId = await artifacts
    .locator('option')
    .filter({ hasText: name })
    .getAttribute('value');
  await artifacts.selectOption(artifactId!);
  const started = page.waitForResponse(
    (response) =>
      response.url() === `${endpoint}/v1/runs` && response.request().method() === 'POST',
  );
  await page.getByRole('button', { name: 'Start run', exact: true }).click();
  const { run: startedRun } = await (await started).json();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Review request', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Review request', exact: true }).click();
  await expect(page.getByText('Approve the binary copy.', { exact: true })).toBeVisible();
  await expect(page.locator('.review-card .json-view').first()).toContainText('sha256');
  await page.getByRole('textbox', { name: 'Engine review response' }).fill('{"approved":false}');
  await page.getByRole('button', { name: 'Submit response' }).click();
  await expect(page.getByRole('alert')).toContainText('JSON schema');
  await page.getByRole('textbox', { name: 'Engine review response' }).fill('{"approved":true}');
  await page.getByRole('button', { name: 'Submit response' }).click();
  await expect(page.getByRole('heading', { name: 'No open engine requests' })).toBeVisible();
  await navigate(page, 'Runs');
  await page.locator('.table-row').filter({ hasText: title }).click();
  await expect(page.locator('.page-heading .status')).toHaveText('Completed', { timeout: 30_000 });
  const { run } = await (await page.request.get(`${endpoint}/v1/runs/${startedRun.id}`)).json();
  await page.getByRole('button', { name: 'Live graph', exact: true }).click();
  const scope = page.locator('.execution-scope-picker select');
  await expect(scope.locator('option')).toHaveCount(2);
  await scope.selectOption({ label: 'copy' });
  await page.locator('.react-flow__node[data-id="write"]').click();
  const inspector = page.locator('.execution-inspector');
  await expect(inspector.getByRole('heading', { name: 'write', exact: true })).toBeVisible();
  await expect(inspector.locator('.status')).toHaveText('Completed');
  await expect(inspector.locator('.execution-parent-link')).toHaveText('copy');
  await inspector.getByRole('button', { name: 'Inputs & outputs', exact: true }).click();
  await expect(inspector).toContainText(artifactId!);
  await expect(inspector).toContainText(run.artifacts[0].id);
  await navigate(page, 'Artifacts');
  await page.getByRole('textbox', { name: 'Search engine artifacts' }).fill(run.artifacts[0].id);
  await page.locator('.artifact-card').click();
  await expect(page.locator('.artifact-text')).toContainText('Binary artifact');
  const downloaded = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const exported = await downloaded;
  expect(exported.suggestedFilename()).toBe('copy.bin');
  expect(readFileSync((await exported.path())!)).toEqual(bytes);
});

test('real Qwen agent exposes model iterations, tool results and its produced file', async ({
  page,
}, testInfo) => {
  const title = `Live agent ${randomUUID().slice(0, 8)}`;
  await importPackage(
    page,
    {
      'pipeline.yaml': `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: live-agent-observation, title: ${title}}
spec:
  models: {writer: {connection: model_main}}
  sandboxes: {work: {profile: python_box}}
  nodes:
    researcher:
      type: agent
      sandbox: work
      tools: {sandbox: [files.write, files.read]}
      agent:
        model: writer
        maxSteps: 8
        prompt:
          text: >-
            Use files.write to create note.txt with content "Live agent observation works".
            Use only the relative path note.txt (never /workspace/note.txt).
            Then use files.read with root workspace and path note.txt to read the file.
            Finally call knotra_finish with summary equal to the text you read.
      outputs:
        summary: {schema: {type: string, minLength: 1}}
        note:
          artifact: {mediaTypes: [text/plain]}
          collect: {path: note.txt, mediaType: text/plain}
  outputs:
    summary: {schema: {type: string}, bind: {from: nodes.researcher.outputs.summary}}
    note: {artifact: {mediaTypes: [text/plain]}, bind: {from: nodes.researcher.outputs.note}}
`,
    },
    title,
  );
  const started = await start(page);
  await page.locator('.react-flow__node[data-id="researcher"]').click();
  await expect(page.locator('.execution-inspector')).toContainText('files.write', {
    timeout: 120_000,
  });
  await expect(page.locator('.page-heading .status')).toHaveText('Completed', { timeout: 120_000 });
  await expect(page.locator('.execution-inspector')).toContainText('files.read');
  await expect.poll(() => page.locator('.execution-operation').count()).toBeGreaterThanOrEqual(5);
  const { run } = await (await page.request.get(`${endpoint}/v1/runs/${started.id}`)).json();
  expect(run.outputs.summary).toContain('Live agent observation works');
  expect(run.artifacts.map((artifact: { name: string }) => artifact.name)).toEqual(['note.txt']);
  const instance = run.instances.find((item: { nodeId: string }) => item.nodeId === 'researcher');
  const history = (
    await (
      await page.request.get(`${endpoint}/v1/runs/${run.id}/history?instanceId=${instance.id}`)
    ).json()
  ).items;
  expect(
    history.filter((event: { type: string }) => event.type === 'agent.iteration').length,
  ).toBeGreaterThanOrEqual(3);
  expect(
    history.some(
      (event: { type: string; data?: { name?: string } }) =>
        event.type === 'tool.completed' && event.data?.name === 'files.read',
    ),
  ).toBe(true);
  await page.screenshot({ path: testInfo.outputPath('real-qwen-agent-cycle.png'), fullPage: true });
  await page
    .locator('.execution-inspector')
    .getByRole('button', { name: 'Inputs & outputs', exact: true })
    .click();
  await expect(page.locator('.execution-artifact')).toHaveCount(1);
  await expect(page.locator('.execution-inspector')).toContainText(run.outputs.summary);
});
