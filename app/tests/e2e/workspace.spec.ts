import { test, expect, type Page } from '@playwright/test';
import { readFileSync, readdirSync } from 'node:fs';
import { resolve } from 'node:path';
import { createHash } from 'node:crypto';
import { zipSync, unzipSync } from 'fflate';

async function navigate(page: Page, name: string) {
  await page
    .getByRole('navigation', { name: 'Main navigation' })
    .getByRole('button', { name, exact: true })
    .click();
}

async function open(page: Page) {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Research brief', exact: true })).toBeVisible();
}

async function persisted(page: Page) {
  return page.evaluate(() => JSON.parse(localStorage.getItem('knotra.workspace.v1')!));
}

test('authoring stays synchronized and navigation preference survives reload', async ({ page }) => {
  const failures: string[] = [];
  page.on('pageerror', (e) => failures.push(e.message));
  await open(page);
  await page
    .getByRole('textbox', { name: 'Description', exact: true })
    .fill('Check the sources carefully');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await page.getByRole('tab', { name: 'Code', exact: true }).click();
  await expect
    .poll(async () =>
      (await persisted(page)).workspaces[0].source.includes('Check the sources carefully'),
    )
    .toBe(true);
  await expect(page.getByRole('button', { name: 'Run demo', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Collapse navigation' }).click();
  await expect(page.locator('.app-sidebar')).toHaveCSS('width', '74px');
  await expect.poll(async () => (await persisted(page)).compact).toBe(true);
  await page.reload();
  await expect(page.getByRole('button', { name: 'Expand navigation' })).toBeVisible();
  await page.getByRole('button', { name: 'Expand navigation' }).click();
  await expect(page.getByRole('textbox', { name: 'Description', exact: true })).toHaveValue(
    'Check the sources carefully',
  );
  await navigate(page, 'Runs');
  await expect(page.getByRole('heading', { name: 'Run history' })).toBeVisible();
  await navigate(page, 'Inbox');
  await expect(page.getByRole('heading', { name: "You're all caught up." })).toBeVisible();
  await navigate(page, 'Artifacts');
  await expect(page.getByRole('heading', { name: 'Artifacts', exact: true })).toBeVisible();
  await navigate(page, 'Resources');
  await expect(page.getByRole('heading', { name: 'Models & tools' })).toBeVisible();
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByRole('checkbox', { name: 'Compact navigation' })).toHaveCount(0);
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export backup' }).click();
  const backup = JSON.parse(readFileSync((await (await download).path())!, 'utf8'));
  expect(
    backup.workspaces.some((w: { source: string }) =>
      w.source.includes('Check the sources carefully'),
    ),
  ).toBe(true);
  expect(failures).toEqual([]);
});

test('complete ZIP package import preserves child contracts and every exported byte', async ({
  page,
}) => {
  await open(page);
  const root = resolve('../contracts/v1/fixtures/positive/subpipeline');
  const contents = Object.fromEntries(
    readdirSync(root, { recursive: true, withFileTypes: true })
      .filter((entry) => entry.isFile())
      .map((entry) => {
        const path = resolve(entry.parentPath, entry.name);
        return [path.slice(root.length + 1), new Uint8Array(readFileSync(path))];
      }),
  );
  await page.getByRole('button', { name: 'Open package', exact: true }).click();
  await page.getByLabel('Import pipeline package', { exact: true }).setInputFiles({
    name: 'subpipeline.zip',
    mimeType: 'application/zip',
    buffer: Buffer.from(zipSync(contents)),
  });
  const chooser = page.getByRole('dialog');
  await expect(chooser.getByRole('heading', { name: 'Choose the entrypoint.' })).toBeVisible();
  await chooser.getByRole('combobox').selectOption('pipeline.yaml');
  await chooser.getByRole('button', { name: 'Open package', exact: true }).click();
  await expect(
    page.getByRole('heading', { name: 'subpipeline-contract', exact: true }),
  ).toBeVisible();
  await page.locator('.react-flow__node').first().click();
  await page.getByRole('button', { name: 'Inputs & outputs', exact: true }).click();
  await expect(
    page
      .locator('.inspector .port-editor-card')
      .filter({ has: page.locator('strong').filter({ hasText: /^reply$/ }) }),
  ).toBeVisible();
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const exported = unzipSync(new Uint8Array(readFileSync((await (await download).path())!)));
  expect(Object.keys(exported).sort()).toEqual(Object.keys(contents).sort());
  for (const [path, bytes] of Object.entries(contents))
    expect(Buffer.from(exported[path]), path).toEqual(Buffer.from(bytes));
});

test('saved human request resumes once and produces a traceable artifact', async ({ page }) => {
  await open(page);
  const original = (await persisted(page)).workspaces[0].source;
  await page.getByRole('button', { name: 'Run demo', exact: true }).click();
  await page.getByRole('textbox', { name: 'Research topic' }).fill('Persistent review test');
  await page.getByRole('button', { name: 'Start demo', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Review request' })).toBeVisible();
  await expect.poll(async () => (await persisted(page)).runs[0]?.requestId).toBeTruthy();
  const request = (await persisted(page)).runs[0].requestId;
  await page.reload();
  await navigate(page, 'Inbox');
  expect((await persisted(page)).runs[0].requestId).toBe(request);
  await page.getByRole('button', { name: 'JSON response', exact: true }).click();
  await page.getByRole('textbox', { name: 'Review JSON response' }).fill('{"feedback": 12}');
  await page.getByRole('button', { name: 'Submit response' }).click();
  await expect(page.getByRole('alert')).toBeVisible();
  await page.getByRole('button', { name: 'Simple response', exact: true }).click();
  await page.getByRole('textbox', { name: 'Your feedback' }).fill('Reviewed and ready.');
  await page.getByRole('button', { name: 'Submit response' }).click();
  await expect.poll(async () => (await persisted(page)).runs[0]?.status).toBe('succeeded');
  const run = (await persisted(page)).runs[0];
  expect(run.source).toBe(original);
  expect(run.response.feedback).toBe('Reviewed and ready.');
  await navigate(page, 'Artifacts');
  await page.locator('.artifact-card').first().click();
  await expect(page.locator('.hash')).toHaveText(run.artifacts[0].sha256);
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const bytes = readFileSync((await (await download).path())!);
  expect(createHash('sha256').update(bytes).digest('hex')).toBe(run.artifacts[0].sha256);
  expect(bytes.byteLength).toBe(run.artifacts[0].size);
  expect(bytes.toString()).toContain('Reviewed and ready.');
  // Column geometry catches missing grid rules and overlapping labels in the desktop layout.
  for (const theme of ['dark', 'light'] as const) {
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page
      .getByRole('radio', { name: theme === 'dark' ? 'Dark' : 'Light', exact: true })
      .click();
    for (const width of [1456, 1000]) {
      await page.setViewportSize({ width, height: 767 });
      await navigate(page, 'Runs');
      const controls = await page.locator('.runs-toolbar').evaluate((toolbar) => {
        const search = toolbar.querySelector('.search-field')!.getBoundingClientRect();
        const filter = toolbar.querySelector('select')!.getBoundingClientRect();
        return {
          sameHeight: search.height === filter.height,
          aligned: search.y === filter.y,
          gap: filter.x - search.right,
        };
      });
      expect(controls).toEqual({ sameHeight: true, aligned: true, gap: 12 });
      await page.getByRole('combobox', { name: 'Filter runs by status' }).selectOption('running');
      await expect(page.getByRole('heading', { name: 'No matching runs' })).toBeVisible();
      await page.getByRole('combobox', { name: 'Filter runs by status' }).selectOption('succeeded');
      await expect(page.locator('.table-header')).toHaveCSS('display', 'grid');
      await expect(page.locator('.table-row')).toHaveCSS('display', 'grid');
      const geometry = await page.locator('.run-table').evaluate((table) => {
        const bounds = table.getBoundingClientRect();
        const header = Array.from(table.querySelector('.table-header')!.children).map((el) =>
          el.getBoundingClientRect(),
        );
        const row = Array.from(table.querySelector('.table-row')!.children).map((el) =>
          el.getBoundingClientRect(),
        );
        return {
          aligned: row.every((cell, i) => Math.abs(cell.x - header[i].x) < 1),
          separated: row.every((cell, i) => !i || row[i - 1].right <= cell.left),
          fits: row.every((cell) => cell.right <= bounds.right),
        };
      });
      expect(geometry).toEqual({ aligned: true, separated: true, fits: true });
      await page.screenshot({ path: `/private/tmp/knotra-runs-${theme}-${width}.png` });
    }
    await page.locator('.table-row').first().click();
    await expect(page.locator('.run-bottom')).toBeVisible();
    const bottom = await page.locator('.run-bottom').boundingBox();
    expect(bottom!.y + bottom!.height).toBeLessThanOrEqual(767);
    await page.getByRole('button', { name: 'Snapshot', exact: true }).click();
    await expect(page.getByRole('textbox', { name: 'Immutable run snapshot' })).toBeVisible();
    await page.getByRole('button', { name: 'All runs', exact: true }).click();
  }
});

for (const theme of ['dark', 'light'] as const)
  test(`${theme} palette persists and keeps YAML and popups legible`, async ({ page }) => {
    await open(page);
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page
      .getByRole('radio', { name: theme === 'dark' ? 'Dark' : 'Light', exact: true })
      .click();
    await expect.poll(async () => (await persisted(page)).theme).toBe(theme);
    await page.reload();
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
    await expect(page.locator(`.react-flow.${theme}`)).toBeVisible();
    await page.getByRole('tab', { name: 'Code', exact: true }).click();
    const editor = page.getByRole('textbox', { name: 'Pipeline YAML' });
    await editor.fill('# readable comment\nanswer: 12\napproved: true\nmessage: hello');
    await expect(page.locator('.knotra-scalar-number')).toHaveText('12');
    await expect(page.locator('.knotra-scalar-boolean')).toHaveText('true');
    const contrast = await page.locator('.cm-content span').evaluateAll((spans) => {
      const linear = (x: number) => (x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4);
      const luminance = (color: string) => {
        const rgb = color
          .match(/[\d.]+/g)!
          .slice(0, 3)
          .map((n) => linear(Number(n) / 255));
        return 0.2126 * rgb[0] + 0.7152 * rgb[1] + 0.0722 * rgb[2];
      };
      const background = luminance(
        getComputedStyle(document.querySelector('.cm-editor')!).backgroundColor,
      );
      return spans.map((span) => {
        const foreground = luminance(getComputedStyle(span).color);
        return (
          (Math.max(foreground, background) + 0.05) / (Math.min(foreground, background) + 0.05)
        );
      });
    });
    expect(contrast.length).toBeGreaterThan(3);
    expect(Math.min(...contrast)).toBeGreaterThanOrEqual(4.5);
    await expect
      .poll(async () => (await persisted(page)).workspaces[0].source)
      .toContain('answer: 12');
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page
      .getByRole('radio', { name: theme === 'dark' ? 'Dark' : 'Light', exact: true })
      .focus();
    await page.keyboard.press('ArrowRight');
    await expect(page.locator('html')).toHaveAttribute(
      'data-theme',
      theme === 'dark' ? 'light' : 'dark',
    );
    await page.keyboard.press('ArrowLeft');
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
    await navigate(page, 'Pipelines');
    await page.getByRole('tab', { name: 'Code', exact: true }).click();
    await expect(page.getByRole('textbox', { name: 'Pipeline YAML' })).toContainText('answer: 12');
    await page.getByRole('button', { name: 'New pipeline', exact: true }).click();
    await expect(page.locator('.modal-backdrop')).toHaveCSS('backdrop-filter', 'none');
    await expect(page.locator('.modal>header')).toHaveCSS('background-image', 'none');
    await page.getByRole('button', { name: 'Close dialog' }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
  });

test('build and configure blocks with named ports, drag connections and workflow resources', async ({
  page,
}) => {
  await open(page);
  await page.getByRole('button', { name: 'Add node', exact: true }).click();
  await page
    .getByRole('dialog')
    .getByRole('button', { name: /^Human review/ })
    .click();
  await expect(page.locator('.inspector h2')).toHaveText('human');
  await page.getByRole('button', { name: 'Inputs & outputs', exact: true }).click();
  await page.getByRole('button', { name: 'Add input', exact: true }).click();
  await page.getByRole('textbox', { name: 'New input name', exact: true }).fill('brief');
  await page.getByRole('button', { name: 'Create port', exact: true }).click();
  await page.getByRole('button', { name: 'draft output brief', exact: true }).focus();
  await page.keyboard.press('Enter');
  await page.getByRole('button', { name: 'human input brief', exact: true }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('combobox', { name: 'Source for brief', exact: true })).toHaveValue(
    'nodes.draft.outputs.brief',
  );
  await page.getByRole('button', { name: 'review output feedback', exact: true }).hover();
  const from = await page
    .getByRole('button', { name: 'review output feedback', exact: true })
    .boundingBox();
  const to = await page
    .getByRole('button', { name: 'human input brief', exact: true })
    .boundingBox();
  await page.mouse.move(from!.x + from!.width / 2, from!.y + from!.height / 2);
  await page.mouse.down();
  await page.mouse.move(from!.x + from!.width / 2 + 5, from!.y + from!.height / 2, { steps: 3 });
  await expect(page.locator('.react-flow__connection')).toHaveCount(1);
  await page.mouse.move(to!.x + to!.width / 2, to!.y + to!.height / 2, { steps: 20 });
  await page.mouse.up();
  await expect(page.getByRole('combobox', { name: 'Source for brief', exact: true })).toHaveValue(
    'nodes.review.outputs.feedback',
  );
  await page.getByRole('button', { name: 'human output answer', exact: true }).click();
  await page.getByRole('button', { name: 'draft input summary', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('cycle');
  await page.getByRole('button', { name: 'discover output result', exact: true }).click();
  await page.getByRole('button', { name: 'human input brief', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('different data types');
  // Move a block without changing pipeline notation; remember its layout after reload.
  const originalPosition = (await persisted(page)).workspaces[0].positions['::human'];
  const card = page.locator('.react-flow__node[data-id="human"] .flow-card-top');
  const before = await card.boundingBox();
  await page.mouse.move(before!.x + 70, before!.y + 10);
  await page.mouse.down();
  await page.mouse.move(before!.x + 90, before!.y + 35, { steps: 10 });
  await page.mouse.up();
  await expect
    .poll(async () => (await persisted(page)).workspaces[0].positions?.['::human'])
    .not.toEqual(originalPosition);
  const position = (await persisted(page)).workspaces[0].positions['::human'];
  await page.getByRole('button', { name: 'Workflow', exact: true }).click();
  await page.getByText('Models (2)', { exact: true }).click();
  await page.getByRole('textbox', { name: 'New model alias', exact: true }).fill('assistant');
  await page.getByRole('textbox', { name: 'Connection ID', exact: true }).fill('local_model');
  await page.getByRole('button', { name: 'Add alias', exact: true }).click();
  await page.getByRole('textbox', { name: 'New workflow output', exact: true }).fill('reviewed');
  await page
    .getByRole('combobox', { name: 'Export from', exact: true })
    .selectOption('nodes.human.outputs.answer');
  await page.getByRole('button', { name: 'Add workflow output', exact: true }).click();
  await page.getByRole('button', { name: 'Add node', exact: true }).click();
  await page
    .getByRole('dialog')
    .getByRole('button', { name: /^AI agent/ })
    .click();
  await expect(page.locator('.inspector h2')).toHaveText('agent');
  await page.getByRole('combobox', { name: /^Model alias/ }).selectOption('assistant');
  await page.getByRole('textbox', { name: /^Instructions/ }).fill('Summarize the supplied facts.');
  await page.getByRole('spinbutton', { name: /^Maximum model steps/ }).fill('7');
  const centers = await page.evaluate(() => {
    const dot = document.querySelector('.title-unsaved')!.getBoundingClientRect();
    const title = document.querySelector('.workspace-title h1')!.getBoundingClientRect();
    const logo = document.querySelector('.brand img')!.getBoundingClientRect();
    const toggle = document.querySelector('.nav-toggle')!.getBoundingClientRect();
    return {
      dot: Math.abs(dot.y + dot.height / 2 - title.y - title.height / 2),
      toggle: Math.abs(logo.y + logo.height / 2 - toggle.y - toggle.height / 2),
    };
  });
  expect(centers.dot).toBeLessThan(1);
  expect(centers.toggle).toBeLessThan(1);
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect
    .poll(async () => (await persisted(page)).workspaces[0].source)
    .toContain('maxSteps: 7');
  const source = (await persisted(page)).workspaces[0].source;
  expect(source).toContain('connection: local_model');
  expect(source).toContain('model: assistant');
  expect(source).toContain('from: nodes.review.outputs.feedback');
  expect(source).toContain('from: nodes.human.outputs.answer');
  await page.reload();
  expect((await persisted(page)).workspaces[0].positions['::human']).toEqual(position);
  await page.getByRole('button', { name: 'Workflow', exact: true }).click();
  await expect(
    page.getByRole('combobox', { name: 'Source for reviewed', exact: true }),
  ).toHaveValue('nodes.human.outputs.answer');
});

test('configure a nested body and add blocks in its own scope', async ({ page }) => {
  await open(page);
  await page.getByRole('button', { name: 'foreach-contract', exact: true }).click();
  await page.getByRole('spinbutton', { name: /^Concurrent iterations/ }).fill('3');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await page.getByRole('button', { name: 'Edit body graph', exact: true }).click();
  await expect(page.getByRole('button', { name: 'review input topic', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Add node', exact: true }).click();
  await page
    .getByRole('dialog')
    .getByRole('button', { name: /^AI agent/ })
    .click();
  await expect(page.locator('.inspector h2')).toHaveText('agent');
  await expect(
    page.getByRole('button', { name: 'agent input question', exact: true }),
  ).toBeVisible();
  await page.getByRole('button', { name: 'Parent graph', exact: true }).click();
  await expect(
    page.getByRole('button', { name: 'review_all input items', exact: true }),
  ).toBeVisible();
  await expect(page.getByRole('button', { name: 'agent input question', exact: true })).toHaveCount(
    0,
  );
  await expect
    .poll(
      async () =>
        (await persisted(page)).workspaces.find((w: { source: string }) =>
          w.source.includes('foreach-contract'),
        ).source,
    )
    .toContain('concurrency: 3');
});

for (const theme of ['dark', 'light'] as const)
  test(`${theme} input sources stay stacked at desktop widths`, async ({ page }) => {
    await open(page);
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page
      .getByRole('radio', { name: theme === 'dark' ? 'Dark' : 'Light', exact: true })
      .click();
    await navigate(page, 'Pipelines');
    await page.locator('.react-flow__node[data-id="draft"] .flow-card-top').click();
    await page.getByRole('button', { name: 'Inputs & outputs', exact: true }).click();
    for (const width of [1456, 1000]) {
      await page.setViewportSize({ width, height: 767 });
      await expect
        .poll(() =>
          page.locator('.graph-view').evaluate((canvas) => {
            const bounds = canvas.getBoundingClientRect();
            return [...canvas.querySelectorAll('.react-flow__node')].every((node) => {
              const rect = node.getBoundingClientRect();
              return (
                rect.left >= bounds.left - 1 &&
                rect.top >= bounds.top - 1 &&
                rect.right <= bounds.right + 1 &&
                rect.bottom <= bounds.bottom + 1
              );
            });
          }),
        )
        .toBe(true);
      const binding = page.locator('.binding-editor').first();
      await binding.scrollIntoViewIfNeeded();
      const geometry = await binding.evaluate((element) => {
        const bounds = element.getBoundingClientRect();
        const picker = element.querySelector('select')!.getBoundingClientRect();
        const field = element.querySelector('.field')!.getBoundingClientRect();
        const status = element.querySelector('.binding-status')!.getBoundingClientRect();
        const advanced = element.querySelector('.field-details')!.getBoundingClientRect();
        return {
          fullWidth: Math.abs(picker.width - bounds.width) < 1,
          stacked: status.top >= field.bottom && advanced.top >= status.bottom,
          fits: picker.right <= bounds.right + 1 && advanced.right <= bounds.right + 1,
        };
      });
      expect(geometry).toEqual({ fullWidth: true, stacked: true, fits: true });
      await page.screenshot({ path: `/private/tmp/knotra-source-${theme}-${width}.png` });
    }
  });
