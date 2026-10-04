import { activeWorkspace, addResearchDemo, openActiveEditor } from './authoring';
import { test, expect, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import { zipSync, unzipSync } from 'fflate';

const runtimeErrors = new WeakMap<Page, string[]>();

test.beforeEach(async ({ page }) => {
  const errors: string[] = [];
  runtimeErrors.set(page, errors);
  page.on('pageerror', (error) => errors.push(error.message));
  await page.setViewportSize({ width: 1456, height: 767 });
  await page.goto('/');
  await addResearchDemo(page);
  await expect.poll(() => state(page)).not.toBeNull();
});

test.afterEach(async ({ page }) => {
  expect(runtimeErrors.get(page)).toEqual([]);
});

async function state(page: Page) {
  return page.evaluate(() => JSON.parse(localStorage.getItem('knotra.workspace.v1')!));
}

async function current(page: Page) {
  const saved = await state(page);
  return saved.workspaces.find((workspace: { id: string }) => workspace.id === saved.activeId);
}

async function pipeline(page: Page) {
  return parse((await current(page)).source);
}

async function save(page: Page) {
  await page.getByRole('button', { name: 'Save', exact: true }).click();
}

async function nav(page: Page, name: string) {
  await page
    .getByRole('navigation', { name: 'Main navigation' })
    .getByRole('button', { name, exact: true })
    .click();
}

async function template(page: Page, title: string) {
  const previous = (await state(page)).activeId;
  await page.getByRole('button', { name: 'New pipeline', exact: true }).click();
  await page
    .getByRole('dialog')
    .getByRole('button', {
      name: title === 'Research brief' ? 'Guided demo' : 'Building blocks',
      exact: true,
    })
    .click();
  await page
    .getByRole('dialog')
    .getByRole('button', { name: new RegExp(`^${title} `) })
    .click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page.locator('.inspector h2')).toBeVisible();
  await expect.poll(async () => (await state(page)).activeId).not.toBe(previous);
}

async function fits(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true,
  );
  const controlsFit = await page.locator('.inspector-content').evaluateAll((panels) =>
    panels.every((panel) => {
      const bounds = panel.getBoundingClientRect();
      return [...panel.querySelectorAll('input, textarea, select')]
        .filter((element) => element.getClientRects().length)
        .every((element) => {
          const rect = element.getBoundingClientRect();
          return rect.left >= bounds.left && rect.right <= bounds.right + 1;
        });
    }),
  );
  expect(controlsFit).toBe(true);
}

async function configure(page: Page, id: string, node: any) {
  if (node.type === 'llm') {
    await page.getByRole('combobox', { name: 'Prompt source', exact: true }).selectOption('text');
    await page
      .getByRole('textbox', { name: 'Instructions', exact: true })
      .fill('Return a verified response.');
    await save(page);
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id].llm.prompt.text)
      .toBe('Return a verified response.');
  } else if (node.type === 'agent') {
    await page.getByRole('spinbutton', { name: 'Maximum model steps', exact: true }).fill('0');
    await save(page);
    await expect(page.getByRole('alert')).toContainText('whole number');
    await page.getByRole('spinbutton', { name: 'Maximum model steps', exact: true }).fill('6');
    await save(page);
    await page.getByText('Tool permissions', { exact: true }).click();
    await page
      .getByRole('textbox', { name: 'Allowed tools', exact: true })
      .fill('{"inherit":true}');
    await save(page);
    await expect.poll(async () => (await pipeline(page)).spec.nodes[id].agent.maxSteps).toBe(6);
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id].tools)
      .toEqual({ inherit: true });
  } else if (node.type === 'human') {
    await page
      .getByRole('textbox', { name: 'Review request', exact: true })
      .fill('Return your verified answer.');
    await save(page);
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id].human.prompt.text)
      .toBe('Return your verified answer.');
  } else if (node.type === 'code') {
    const index = node.code.command.length;
    await page.getByRole('button', { name: 'Add argument', exact: true }).click();
    await page
      .getByRole('textbox', { name: `Argument ${index}`, exact: true })
      .fill('--ui-verification');
    await save(page);
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id].code.command.at(-1))
      .toBe('--ui-verification');
    await page.getByRole('button', { name: `Remove argument ${index}`, exact: true }).click();
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id].code.command)
      .toEqual(node.code.command);
  } else if (node.type === 'tool') {
    await page.getByRole('combobox', { name: 'Tool arguments', exact: true }).selectOption('value');
    await page
      .getByRole('textbox', { name: 'Arguments JSON', exact: true })
      .fill('{"query":"Verified UI"}');
    await save(page);
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id].tool.arguments?.value?.query)
      .toBe('Verified UI');
    await page.getByRole('combobox', { name: 'Tool arguments', exact: true }).selectOption('expr');
    await page
      .getByRole('textbox', { name: 'Arguments expression', exact: true })
      .fill('{"query":"Verified expression"}');
    await save(page);
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id].tool.arguments.expr)
      .toBe('{"query":"Verified expression"}');
  } else if (node.type === 'switch') {
    await page
      .getByRole('textbox', { name: 'Route 1 condition', exact: true })
      .fill('args.score >= 20');
    await save(page);
    await page.getByRole('button', { name: 'Add route', exact: true }).click();
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id].switch.cases.length)
      .toBe(node.switch.cases.length + 1);
    await page.getByRole('button', { name: 'Remove route', exact: true }).last().click();
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id].switch.cases.length)
      .toBe(node.switch.cases.length);
  } else if (node.type === 'foreach' || node.type === 'loop') {
    const kind = node.type;
    const number = page.getByRole('spinbutton', {
      name: kind === 'foreach' ? 'Concurrent iterations' : 'Maximum iterations',
      exact: true,
    });
    await number.fill(kind === 'foreach' ? '4' : '5');
    await save(page);
    await expect
      .poll(
        async () =>
          (await pipeline(page)).spec.nodes[id][kind][
            kind === 'foreach' ? 'concurrency' : 'maxIterations'
          ],
      )
      .toBe(kind === 'foreach' ? 4 : 5);
    if (kind === 'loop') {
      await page
        .getByRole('textbox', { name: 'Stop condition', exact: true })
        .fill('body.outputs.count >= 3');
      await save(page);
      await page
        .getByRole('combobox', { name: 'When the limit is reached', exact: true })
        .selectOption('fail');
      await expect
        .poll(async () => (await pipeline(page)).spec.nodes[id].loop.onLimit)
        .toBe('fail');
    }
    await page.getByRole('button', { name: 'Edit body graph', exact: true }).click();
    await page
      .getByRole('textbox', { name: 'Review request', exact: true })
      .fill('Return the verified iteration result.');
    await save(page);
    const child = Object.keys(node[kind].body.nodes)[0];
    await expect
      .poll(
        async () => (await pipeline(page)).spec.nodes[id][kind].body.nodes[child].human.prompt.text,
      )
      .toBe('Return the verified iteration result.');
    await page.getByRole('button', { name: 'Parent graph', exact: true }).click();
  } else if (node.type === 'pipeline') {
    await expect(
      page.getByRole('combobox', { name: 'Child pipeline file', exact: true }),
    ).toHaveValue(node.pipeline.file);
    await expect(page.getByRole('textbox', { name: 'Child permissions', exact: true })).toHaveValue(
      JSON.stringify(node.pipeline.permissions, null, 2),
    );
  }
}

const templates = [
  'Research brief',
  'Model response',
  'Autonomous research',
  'Create an artifact',
  'MCP tool call',
  'Conditional routing',
  'Human review',
  'Parallel review',
  'Iterative refinement',
  'Reusable pipeline',
  'Pass a file',
];

for (const title of templates)
  test(`template: ${title} can be configured, saved and exported`, async ({ page }) => {
    await template(page, title);
    const original = await pipeline(page);
    const id = original.spec.nodes.research ? 'research' : Object.keys(original.spec.nodes)[0];
    await page.getByRole('textbox', { name: 'Description', exact: true }).fill(`Verified ${title}`);
    await save(page);
    await expect
      .poll(async () => (await pipeline(page)).spec.nodes[id]?.description)
      .toBe(`Verified ${title}`);
    await configure(page, id, original.spec.nodes[id]);
    await expect(page.locator('.validation-summary')).toContainText('Local checks passed');
    await fits(page);
    await page.screenshot({
      path: test.info().outputPath(`knotra-audit-template-${title.replaceAll(' ', '-')}.png`),
    });
    await page.getByRole('button', { name: 'Inputs & outputs', exact: true }).click();
    const selected = await page
      .locator('.inspector select')
      .evaluateAll((selects) =>
        selects.map((select) => (select as HTMLSelectElement).selectedIndex),
      );
    expect(selected.every((index) => index >= 0)).toBe(true);
    await fits(page);
    await page.getByRole('tab', { name: 'Code', exact: true }).click();
    await expect(page.getByRole('textbox', { name: 'Pipeline YAML', exact: true })).toContainText(
      original.metadata.name,
    );
    await page.getByRole('tab', { name: /^Files/ }).click();
    const workspace = await current(page);
    for (const file of workspace.files) {
      await page
        .locator('.file-list')
        .getByRole('button', { name: file.path, exact: true })
        .click();
      await expect(
        page.getByRole('textbox', { name: 'Package file contents', exact: true }),
      ).toBeVisible();
    }
    const download = page.waitForEvent('download');
    await page.getByRole('button', { name: 'Export', exact: true }).click();
    const bytes = unzipSync(new Uint8Array(readFileSync((await (await download).path())!)));
    expect(parse(Buffer.from(bytes[workspace.entrypoint]).toString())).toEqual(
      await pipeline(page),
    );
    for (const file of workspace.files)
      expect(Buffer.from(bytes[file.path])).toEqual(Buffer.from(file.content, 'base64'));
    await page.reload();
    await openActiveEditor(page);
    await expect(page.getByRole('textbox', { name: 'Description', exact: true })).toHaveValue(
      `Verified ${title}`,
    );
  });

test('all nine block types can be added with valid setup and ports', async ({ page }) => {
  test.setTimeout(60_000);
  for (const [kind, title] of [
    ['llm', 'Language model'],
    ['agent', 'AI agent'],
    ['code', 'Code'],
    ['tool', 'MCP tool'],
    ['switch', 'Switch'],
    ['human', 'Human review'],
    ['foreach', 'For each'],
    ['loop', 'Loop'],
    ['pipeline', 'Pipeline'],
  ]) {
    await page.getByRole('button', { name: 'Add node', exact: true }).click();
    await page
      .getByRole('dialog')
      .getByRole('button', { name: new RegExp(`^${title} `) })
      .click();
    await expect(page.locator('.inspector h2')).toHaveText(kind);
    await expect.poll(async () => (await pipeline(page)).spec.nodes[kind]?.type).toBe(kind);
    await expect(page.locator('.validation-summary')).toContainText('Local checks passed');
    await fits(page);
    await page.getByRole('button', { name: 'Inputs & outputs', exact: true }).click();
    await fits(page);
    await page.getByRole('button', { name: 'Setup', exact: true }).click();
  }
});

test('scratch workflow supports port values, expressions, schemas and block removal', async ({
  page,
}) => {
  await page.getByRole('button', { name: 'New pipeline', exact: true }).click();
  await page.getByRole('button', { name: 'Start from scratch', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Untitled pipeline', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Delete node', exact: true })).toBeDisabled();
  await page
    .getByRole('textbox', { name: 'Review request', exact: true })
    .fill('Return a useful answer.');
  await save(page);
  await page.getByRole('button', { name: 'Duplicate node', exact: true }).click();
  await expect(page.locator('.inspector h2')).toHaveText('ask copy');
  await page.getByRole('button', { name: 'Inputs & outputs', exact: true }).click();
  await page.getByRole('button', { name: 'Add input', exact: true }).click();
  await page.getByRole('textbox', { name: 'New input name', exact: true }).fill('context');
  await page.getByRole('button', { name: 'Create port', exact: true }).click();
  await page.getByRole('textbox', { name: 'Value for context', exact: true }).fill('User context');
  await save(page);
  await expect
    .poll(async () => (await pipeline(page)).spec.nodes.ask_copy?.inputs?.context?.bind?.value)
    .toBe('User context');
  await page
    .getByRole('combobox', { name: 'Source for context', exact: true })
    .selectOption('$expression');
  await page
    .getByRole('textbox', { name: 'Expression for context', exact: true })
    .fill('"Computed context"');
  await save(page);
  await expect
    .poll(async () => (await pipeline(page)).spec.nodes.ask_copy?.inputs?.context?.bind?.expr)
    .toBe('"Computed context"');
  await page
    .getByRole('combobox', { name: 'Source for context', exact: true })
    .selectOption('nodes.ask.outputs.answer');
  await page.getByText('Select part of the output', { exact: true }).click();
  await page.getByRole('textbox', { name: 'JSON pointer', exact: true }).fill('/answer');
  await save(page);
  await expect
    .poll(async () => (await pipeline(page)).spec.nodes.ask_copy?.inputs?.context?.bind?.path)
    .toBe('/answer');
  await page.getByRole('button', { name: 'Add output', exact: true }).click();
  await page.getByRole('textbox', { name: 'New output name', exact: true }).fill('extra');
  await page.getByRole('button', { name: 'Create port', exact: true }).click();
  const port = page
    .locator('.port-editor-card')
    .filter({ has: page.locator('strong').filter({ hasText: /^extra$/ }) });
  await port.getByText('Schema & details', { exact: true }).click();
  await page.getByRole('textbox', { name: 'Schema for extra', exact: true }).fill('bad JSON');
  await save(page);
  await expect(port.getByRole('alert')).toBeVisible();
  await page
    .getByRole('textbox', { name: 'Schema for extra', exact: true })
    .fill('{"type":["string","null"]}');
  await save(page);
  await expect(port.getByRole('alert')).toHaveCount(0);
  await expect(port.getByRole('combobox', { name: 'Type of extra' })).toHaveValue('$custom');
  await page.getByRole('button', { name: 'Remove output extra', exact: true }).click();
  await page.getByRole('button', { name: 'Remove input context', exact: true }).click();
  await page.getByRole('button', { name: 'Delete node', exact: true }).click();
  await page.getByRole('button', { name: 'Keep node', exact: true }).click();
  await page.getByRole('button', { name: 'Delete node', exact: true }).click();
  await page.getByRole('button', { name: 'Remove node', exact: true }).click();
  await expect.poll(async () => Object.keys((await pipeline(page)).spec.nodes)).toEqual(['ask']);
  await page.getByRole('button', { name: 'Delete pipeline', exact: true }).click();
  await page.getByRole('button', { name: 'Keep pipeline', exact: true }).click();
  await page.getByRole('button', { name: 'Delete pipeline', exact: true }).click();
  await page.getByRole('button', { name: 'Remove pipeline', exact: true }).click();
  await expect(page.locator('.pipelines-page')).toBeVisible();
  await page.getByRole('button', { name: 'Open Research brief', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Research brief', exact: true })).toBeVisible();
});

test('files, prompt selection and malformed YAML recover through the editor', async ({ page }) => {
  await page.getByRole('tab', { name: /^Files/ }).click();
  await page.getByRole('button', { name: 'Add package file', exact: true }).click();
  const input = page.locator('.file-list input[type=file]');
  await input.setInputFiles({
    name: 'instructions.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('Write a careful brief.'),
  });
  await expect(
    page.locator('.file-list').getByRole('button', { name: 'instructions.txt', exact: true }),
  ).toBeVisible();
  await page
    .locator('.file-list')
    .getByRole('button', { name: 'instructions.txt', exact: true })
    .click();
  await page
    .getByRole('textbox', { name: 'Package file contents', exact: true })
    .fill('Write a verified brief.');
  await save(page);
  await input.setInputFiles({
    name: 'instructions.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('duplicate'),
  });
  await expect(page.getByRole('status')).toContainText('already exists');
  await input.setInputFiles({
    name: 'binary.bin',
    mimeType: 'application/octet-stream',
    buffer: Buffer.from([0, 255, 1, 128]),
  });
  await page.locator('.file-list').getByRole('button', { name: 'binary.bin', exact: true }).click();
  await expect(
    page.getByText('4 B · Binary file preserved with the package', { exact: true }),
  ).toBeVisible();
  const exported = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const binary = unzipSync(new Uint8Array(readFileSync((await (await exported).path())!)));
  expect(Buffer.from(binary['binary.bin'])).toEqual(Buffer.from([0, 255, 1, 128]));
  await page.getByRole('tab', { name: 'Canvas', exact: true }).click();
  await page.getByRole('combobox', { name: 'Prompt source', exact: true }).selectOption('file');
  await expect(page.getByRole('combobox', { name: 'Prompt file', exact: true })).toHaveValue(
    'instructions.txt',
  );
  await expect
    .poll(async () => (await pipeline(page)).spec.nodes.research.agent.prompt.file)
    .toBe('instructions.txt');
  const valid = (await current(page)).source;
  await page.getByRole('tab', { name: 'Code', exact: true }).click();
  await page.getByRole('textbox', { name: 'Pipeline YAML', exact: true }).fill('spec: [');
  await page.getByRole('tab', { name: 'Canvas', exact: true }).click();
  await expect(
    page.getByRole('heading', { name: 'The YAML needs a little attention' }),
  ).toBeVisible();
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('validation errors');
  await page.locator('.validation-summary').click();
  await expect(page.getByRole('log')).toBeVisible();
  await page.getByRole('button', { name: 'Open YAML editor', exact: true }).click();
  await page.getByRole('textbox', { name: 'Pipeline YAML', exact: true }).fill(valid);
  await page.getByRole('tab', { name: 'Canvas', exact: true }).click();
  await expect(page.locator('.validation-summary')).toContainText('Local checks passed');
  await page.getByRole('button', { name: 'Zoom in', exact: true }).click();
  await page.getByRole('button', { name: 'Zoom out', exact: true }).click();
  await page.getByRole('button', { name: 'Fit graph', exact: true }).click();
});

test('resource tabs lead directly to their editors and persist alias edits', async ({ page }) => {
  for (const title of ['Models', 'MCP tools', 'Sandboxes', 'Secret references']) {
    await nav(page, 'Resources');
    await page
      .locator('.resource-tabs')
      .getByRole('button', { name: new RegExp(`^${title} `) })
      .click();
    await page.getByRole('button', { name: 'Edit pipeline resources', exact: true }).click();
    if (title === 'Secret references')
      await expect(page.getByRole('tab', { name: 'Code', exact: true })).toHaveAttribute(
        'aria-selected',
        'true',
      );
    else await expect(page.locator('.workflow-resources')).toBeVisible();
  }
  await nav(page, 'Resources');
  await page.getByRole('button', { name: 'Edit pipeline resources', exact: true }).click();
  await page
    .getByRole('textbox', { name: 'researcher · connection ID', exact: true })
    .fill('model_verified');
  await save(page);
  await nav(page, 'Resources');
  await expect(
    page
      .locator('.resource-card')
      .filter({ has: page.getByRole('heading', { name: 'researcher', exact: true }) }),
  ).toContainText('model_verified');
  await page.screenshot({ path: test.info().outputPath('knotra-audit-resources-edited.png') });
});

test('settings, search, shortcuts and modal dismissal work with persisted preferences', async ({
  page,
}) => {
  await page.keyboard.press('Control+k');
  await expect(page.getByRole('textbox', { name: 'Search workspace', exact: true })).toBeFocused();
  await page.keyboard.type('Settings');
  await page
    .getByRole('dialog')
    .getByRole('button', { name: 'Settings View', exact: true })
    .click();
  await expect(page.getByText('Disconnected', { exact: true })).toBeVisible();
  await expect(page.locator('.notice.small')).toContainText(
    'Start your local Knotra engine and connect using its address.',
  );
  await expect(page.locator('.notice.small')).toContainText(
    'Running workflows continue on the engine when you close the app.',
  );
  await page
    .getByRole('textbox', { name: 'Engine base URL', exact: true })
    .fill('file:///tmp/engine');
  await page.getByRole('button', { name: 'Save address', exact: true }).click();
  await expect(page.getByRole('alert')).toBeVisible();
  await page
    .getByRole('textbox', { name: 'Engine base URL', exact: true })
    .fill('http://127.0.0.1:9000/');
  await page.getByRole('button', { name: 'Save address', exact: true }).click();
  await expect.poll(async () => (await state(page)).engineUrl).toBe('http://127.0.0.1:9000');
  await page.getByRole('radio', { name: 'Light', exact: true }).click();
  await page.getByRole('button', { name: 'Collapse navigation', exact: true }).click();
  await page.reload();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await expect(page.getByRole('button', { name: 'Expand navigation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Expand navigation', exact: true }).click();
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByRole('textbox', { name: 'Engine base URL', exact: true })).toHaveValue(
    'http://127.0.0.1:9000',
  );
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export backup', exact: true }).click();
  const backup = JSON.parse(readFileSync((await (await download).path())!, 'utf8'));
  expect(backup.theme).toBe('light');
  expect(backup.workspaces).toHaveLength(6);
  await page.keyboard.press('Control+n');
  await expect(page.getByRole('dialog')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.getByRole('button', { name: 'New pipeline', exact: true }).click();
  await page.locator('.modal-backdrop').click({ position: { x: 5, y: 5 } });
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.screenshot({ path: test.info().outputPath('knotra-audit-settings-light.png') });
});

test('folder and loose YAML imports work and reject invalid selections', async ({ page }) => {
  await page.getByRole('button', { name: 'Open package', exact: true }).click();
  const chooser = page.waitForEvent('filechooser');
  await page.getByRole('button', { name: /^Select package folder/ }).click();
  await (await chooser).setFiles(resolve('../contracts/v1/fixtures/positive/llm'));
  await expect(page.getByRole('heading', { name: 'llm-contract', exact: true })).toBeVisible();
  await expect.poll(async () => (await current(page)).files.length).toBe(2);
  await page.getByRole('button', { name: 'Open package', exact: true }).click();
  const looseChooser = page.waitForEvent('filechooser');
  await page.getByRole('button', { name: /^Select files or ZIP/ }).click();
  await (
    await looseChooser
  ).setFiles(resolve('../contracts/v1/fixtures/positive/human/pipeline.yaml'));
  await expect(page.getByRole('heading', { name: 'basic-human', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Open package', exact: true }).click();
  await page.getByLabel('Import pipeline package', { exact: true }).setInputFiles({
    name: 'invalid.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('not YAML'),
  });
  await expect(page.getByRole('status')).toContainText('Select a YAML pipeline');
  await page.getByRole('button', { name: 'Close dialog', exact: true }).click();
});

test('ZIP import selects the only Pipeline after profile and schema YAML files', async ({
  page,
}) => {
  const pipeline = Buffer.from(
    readFileSync('../contracts/v1/fixtures/positive/llm/pipeline.yaml', 'utf8').replaceAll(
      'reply.schema.json',
      'reply.schema.yaml',
    ),
  );
  const prompt = readFileSync('../contracts/v1/fixtures/positive/llm/prompt.txt');
  const schema = readFileSync('../contracts/v1/fixtures/positive/llm/reply.schema.json');
  const contents = {
    'profile.yaml': Buffer.from(
      'apiVersion: knotra/v1\nkind: EngineProfile\nmetadata: {name: local}\nspec: {}\n',
    ),
    'reply.schema.yaml': schema,
    'pipeline.yaml': pipeline,
    'prompt.txt': prompt,
  };
  await page.getByRole('button', { name: 'Open package', exact: true }).click();
  await page.getByLabel('Import pipeline package', { exact: true }).setInputFiles({
    name: 'mixed.zip',
    mimeType: 'application/zip',
    buffer: Buffer.from(zipSync(contents)),
  });
  await expect(page.getByRole('heading', { name: 'llm-contract', exact: true })).toBeVisible();
  await expect.poll(async () => (await current(page)).entrypoint).toBe('pipeline.yaml');
  await expect
    .poll(async () => (await current(page)).files.map((file: { path: string }) => file.path).sort())
    .toEqual(['prompt.txt', 'reply.schema.yaml']);
  const downloaded = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const exported = unzipSync(new Uint8Array(readFileSync((await (await downloaded).path())!)));
  expect(Object.keys(exported).sort()).toEqual([
    'pipeline.yaml',
    'prompt.txt',
    'reply.schema.yaml',
  ]);
  for (const path of Object.keys(exported)) {
    expect(Buffer.from(exported[path])).toEqual(contents[path as keyof typeof contents]);
  }

  await page.getByRole('button', { name: 'Open package', exact: true }).click();
  await page.getByLabel('Import pipeline package', { exact: true }).setInputFiles({
    name: 'profile-only.zip',
    mimeType: 'application/zip',
    buffer: Buffer.from(zipSync({ 'profile.yaml': contents['profile.yaml'] })),
  });
  await expect(page.getByRole('status')).toContainText('Package has no Pipeline YAML entrypoint.');
  await expect(page.getByRole('heading', { name: 'llm-contract', exact: true })).toBeVisible();
});

test('ZIP import preserves an entrypoint BOM and rejects duplicate paths before overwrite', async ({
  page,
}) => {
  const pipeline = Buffer.concat([
    Buffer.from([0xef, 0xbb, 0xbf]),
    readFileSync('../contracts/v1/fixtures/positive/human/pipeline.yaml'),
  ]);
  await page.getByRole('button', { name: 'Open package', exact: true }).click();
  await page.getByLabel('Import pipeline package', { exact: true }).setInputFiles({
    name: 'bom.ZIP',
    mimeType: 'application/zip',
    buffer: Buffer.from(zipSync({ 'PIPELINE.YAML': pipeline })),
  });
  await expect(page.getByRole('heading', { name: 'basic-human', exact: true })).toBeVisible();
  const downloaded = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const exported = unzipSync(new Uint8Array(readFileSync((await (await downloaded).path())!)));
  expect(Buffer.from(exported['PIPELINE.YAML'])).toEqual(pipeline);

  for (const exact of [false, true]) {
    const archive = Buffer.from(zipSync({ 'pipeline.yaml': pipeline, 'pipeline.YAML': pipeline }));
    if (exact) {
      let position = 0;
      while ((position = archive.indexOf('pipeline.YAML', position)) >= 0) {
        archive.write('pipeline.yaml', position);
        position += 'pipeline.yaml'.length;
      }
    }
    await page.getByRole('button', { name: 'Open package', exact: true }).click();
    await page.getByLabel('Import pipeline package', { exact: true }).setInputFiles({
      name: 'duplicate.zip',
      mimeType: 'application/zip',
      buffer: archive,
    });
    await expect(page.getByRole('status')).toContainText('Duplicate package path:');
    await expect(page.getByRole('heading', { name: 'basic-human', exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Close dialog', exact: true }).click();
  }
});

test('demo tabs, cancellation, review links and artifact search complete the run journey', async ({
  page,
}) => {
  test.setTimeout(60_000);
  await nav(page, 'Inbox');
  await expect(
    page.getByRole('heading', { name: "You're all caught up.", exact: true }),
  ).toBeVisible();
  await nav(page, 'Artifacts');
  await expect(
    page.getByRole('heading', { name: 'Your outputs live here', exact: true }),
  ).toBeVisible();
  await nav(page, 'Runs');
  await page.getByRole('button', { name: 'Explore the demo', exact: true }).click();
  await page.getByRole('textbox', { name: 'Research topic', exact: true }).fill('');
  await expect(page.getByRole('button', { name: 'Start demo', exact: true })).toBeDisabled();
  await page
    .getByRole('textbox', { name: 'Research topic', exact: true })
    .fill('Cancelled UI check');
  await page.keyboard.press('Enter');
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(page.locator('.page-heading .status')).toHaveText('Cancelled');
  await page.getByRole('button', { name: 'All runs', exact: true }).click();
  await page
    .getByRole('combobox', { name: 'Filter runs by status', exact: true })
    .selectOption('cancelled');
  await expect(page.locator('.table-row')).toHaveCount(1);
  await page.getByRole('textbox', { name: 'Search runs', exact: true }).fill('missing');
  await expect(page.getByRole('heading', { name: 'No matching runs', exact: true })).toBeVisible();
  await page.getByRole('textbox', { name: 'Search runs', exact: true }).fill('');
  await page.getByRole('button', { name: 'Try the guided demo', exact: true }).click();
  await page
    .getByRole('textbox', { name: 'Research topic', exact: true })
    .fill('Completed UI check');
  await page.getByRole('button', { name: 'Start demo', exact: true }).click();
  await page.getByRole('button', { name: 'Inputs', exact: true }).click();
  await expect(page.locator('.json-view')).toContainText('Completed UI check');
  await page.getByRole('button', { name: 'Snapshot', exact: true }).click();
  await expect(
    page.getByRole('textbox', { name: 'Immutable run snapshot', exact: true }),
  ).toHaveAttribute('contenteditable', 'false');
  await page.getByRole('button', { name: 'Timeline', exact: true }).click();
  await expect(page.locator('.timeline-event').first()).toBeVisible();
  await expect(page.getByRole('button', { name: 'Review request', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Review request', exact: true }).click();
  await page.getByRole('button', { name: 'View run', exact: true }).click();
  await expect(
    page.getByRole('heading', { name: 'Completed UI check', exact: true }),
  ).toBeVisible();
  await page.getByRole('button', { name: 'Review request', exact: true }).click();
  await page.screenshot({ path: test.info().outputPath('knotra-audit-review-dark.png') });
  await page.getByRole('button', { name: 'JSON response', exact: true }).click();
  await page
    .getByRole('textbox', { name: 'Review JSON response', exact: true })
    .fill('invalid JSON');
  await page.getByRole('button', { name: 'Submit response', exact: true }).click();
  await expect(page.getByRole('alert')).toBeVisible();
  await page
    .getByRole('textbox', { name: 'Review JSON response', exact: true })
    .fill('{"feedback":"UI verified"}');
  await page.getByRole('button', { name: 'Submit response', exact: true }).click();
  await expect.poll(async () => (await state(page)).runs[0]?.status).toBe('succeeded');
  await nav(page, 'Runs');
  await page
    .getByRole('combobox', { name: 'Filter runs by status', exact: true })
    .selectOption('all');
  await page.locator('.table-row').filter({ hasText: 'Completed UI check' }).click();
  await page.getByRole('button', { name: 'Outputs', exact: true }).click();
  await expect(page.locator('.json-view')).toContainText('document');
  await page.screenshot({ path: test.info().outputPath('knotra-audit-run-complete.png') });
  await nav(page, 'Artifacts');
  await page.locator('.artifact-card').click();
  await expect(page.locator('.artifact-text')).toContainText('UI verified');
  await page.getByRole('textbox', { name: 'Search artifacts', exact: true }).fill('missing');
  await expect(
    page.getByRole('heading', { name: 'No matching artifacts', exact: true }),
  ).toBeVisible();
  await page
    .getByRole('textbox', { name: 'Search artifacts', exact: true })
    .fill('Completed UI check');
  await expect(page.locator('.artifact-card')).toHaveCount(1);
  await page.screenshot({ path: test.info().outputPath('knotra-audit-artifact-dark.png') });
  for (const theme of ['dark', 'light'] as const) {
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page
      .getByRole('radio', { name: theme === 'dark' ? 'Dark' : 'Light', exact: true })
      .click();
    for (const [width, height] of [
      [1456, 767],
      [1000, 767],
      [1000, 680],
    ]) {
      await page.setViewportSize({ width, height });
      for (const view of ['Runs', 'Inbox', 'Artifacts', 'Resources']) {
        await nav(page, view);
        if (view === 'Runs') {
          await page.locator('.table-row').filter({ hasText: 'Completed UI check' }).click();
          const bottom = await page.locator('.run-bottom').boundingBox();
          expect(bottom!.y + bottom!.height).toBeLessThanOrEqual(height);
          await page.getByRole('button', { name: 'All runs', exact: true }).click();
        }
        if (view === 'Artifacts') {
          await page.locator('.artifact-card').first().click();
          const inspector = await page.locator('.artifact-inspector').boundingBox();
          expect(inspector!.x + inspector!.width).toBeLessThanOrEqual(width);
        }
        await fits(page);
        await page.screenshot({
          path: test
            .info()
            .outputPath(`knotra-audit-${view.toLowerCase()}-${theme}-${width}-${height}.png`),
        });
      }
    }
  }
});

test('keyboard save commits the currently focused setup field', async ({ page }) => {
  const description = page.getByRole('textbox', { name: 'Description', exact: true });
  await description.fill('Saved with keyboard while editing');
  await page.keyboard.press('Control+s');
  await expect
    .poll(async () => (await current(page)).savedSource)
    .toContain('Saved with keyboard while editing');
  await expect(description).toBeFocused();
  await page.getByRole('button', { name: 'Workflow', exact: true }).click();
  const title = page.getByRole('textbox', { name: 'Workflow title', exact: true });
  await title.fill('Saved workflow title');
  await page.keyboard.press('Control+s');
  await expect
    .poll(async () => (await current(page)).savedSource)
    .toContain('Saved workflow title');
  await expect(title).toBeFocused();
  await page.getByText('Models (2)', { exact: true }).click();
  const connection = page.getByRole('textbox', { name: 'researcher · connection ID', exact: true });
  await connection.fill('model_revision');
  await page.keyboard.press('Control+s');
  await expect.poll(async () => (await current(page)).savedSource).toContain('model_revision');
  await expect(connection).toBeFocused();
});

test('workflow inputs, exports and all editable resource aliases persist', async ({ page }) => {
  await page.getByRole('button', { name: 'Workflow', exact: true }).click();
  await page
    .getByRole('textbox', { name: 'Workflow title', exact: true })
    .fill('Verified workflow');
  await save(page);
  await expect(page.getByRole('heading', { name: 'Verified workflow', exact: true })).toBeVisible();
  await page.getByRole('textbox', { name: 'New workflow input', exact: true }).fill('context');
  await page.getByRole('button', { name: 'Add workflow input', exact: true }).click();
  await page
    .getByRole('textbox', { name: 'Default for context', exact: true })
    .fill('"Reference information"');
  await save(page);
  await expect
    .poll(async () => (await pipeline(page)).spec.inputs?.context?.default)
    .toBe('Reference information');
  await page.getByRole('textbox', { name: 'New workflow output', exact: true }).fill('summary');
  await page
    .getByRole('combobox', { name: 'Export from', exact: true })
    .selectOption('nodes.research.outputs.summary');
  await page.getByRole('button', { name: 'Add workflow output', exact: true }).click();
  await page
    .getByRole('combobox', { name: 'Source for summary', exact: true })
    .selectOption('inputs.context');
  await expect
    .poll(async () => (await pipeline(page)).spec.outputs?.summary?.bind?.from)
    .toBe('inputs.context');
  for (const [kind, summary, aliasLabel, alias, idLabel, connection] of [
    ['models', 'Models', 'New model alias', 'assistant', 'Connection ID', 'model_verified'],
    ['mcp', 'MCP servers', 'New server alias', 'auxiliary', 'Connection ID', 'search_verified'],
    ['sandboxes', 'Sandboxes', 'New sandbox alias', 'isolated', 'Profile ID', 'sandbox_verified'],
  ]) {
    const panel = page
      .locator('.workflow-resources details')
      .filter({ has: page.locator('summary').filter({ hasText: new RegExp(`^${summary} `) }) });
    await panel.locator('summary').click();
    await panel.getByRole('textbox', { name: aliasLabel, exact: true }).fill(alias);
    await panel.getByRole('textbox', { name: idLabel, exact: true }).fill(connection);
    await panel.getByRole('button', { name: 'Add alias', exact: true }).click();
    await expect
      .poll(
        async () =>
          (await pipeline(page)).spec[kind]?.[alias]?.[
            kind === 'sandboxes' ? 'profile' : 'connection'
          ],
      )
      .toBe(connection);
  }
  await page.getByRole('button', { name: 'Remove workflow output summary', exact: true }).click();
  await page.getByRole('button', { name: 'Remove workflow input context', exact: true }).click();
  await save(page);
  await expect(page.locator('.validation-summary')).toContainText('Local checks passed');
  await page.reload();
  await openActiveEditor(page);
  await page.getByRole('button', { name: 'Workflow', exact: true }).click();
  await page.getByText('Models (3)', { exact: true }).click();
  await expect(
    page.getByRole('textbox', { name: 'assistant · connection ID', exact: true }),
  ).toHaveValue('model_verified');
});

test('empty workspace creation and modal keyboard navigation remain usable', async ({ page }) => {
  const count = await page.locator('.sidebar-pipelines > button').count();
  for (let index = 0; index < count; index++) {
    if (index > 0) await page.locator('.sidebar-pipelines > button').first().click();
    await page.getByRole('button', { name: 'Delete pipeline', exact: true }).click();
    await page.getByRole('button', { name: 'Remove pipeline', exact: true }).click();
  }
  await expect(
    page.getByRole('heading', { name: 'Create your first pipeline', exact: true }),
  ).toBeVisible();
  await page.locator('.empty').getByRole('button', { name: 'New pipeline', exact: true }).click();
  const last = page.getByRole('button', { name: 'Start from scratch', exact: true });
  await last.focus();
  await page.keyboard.press('Tab');
  await expect(page.getByRole('button', { name: 'Close dialog', exact: true })).toBeFocused();
  await page.keyboard.press('Shift+Tab');
  await expect(last).toBeFocused();
  await last.click();
  await expect(page.getByRole('heading', { name: 'Untitled pipeline', exact: true })).toBeVisible();
});

test('documentation opens the project repository in a separate browser tab', async ({
  page,
  context,
}) => {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const opened = context.waitForEvent('page');
  await page.getByRole('button', { name: 'Project documentation', exact: true }).click();
  const tab = await opened;
  await tab.waitForURL('https://github.com/michael-bill/knotra', { waitUntil: 'commit' });
  await expect(
    page.getByRole('heading', { name: 'Workspace settings', exact: true }),
  ).toBeVisible();
  await tab.close();
});
