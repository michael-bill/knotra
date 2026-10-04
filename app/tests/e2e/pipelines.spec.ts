import { expect, test } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { parse } from 'yaml';

const key = 'knotra.workspace.v1';
const titles = [
  'Hello, model',
  'Research dossier from source materials',
  'Build a playable game',
  'From brief to reviewed publication',
];

test('fresh launch lists four runnable starters, filters them, and opens editors explicitly', async ({
  page,
}, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await page.goto('/');
  await expect(page.locator('.pipeline-card')).toHaveCount(4);
  await expect(page.locator('.graph-view')).toHaveCount(0);
  for (const title of titles)
    await expect(page.getByRole('button', { name: `Open ${title}`, exact: true })).toBeVisible();
  await page.getByRole('textbox', { name: 'Search pipelines' }).fill('source materials');
  await expect(page.locator('.pipeline-card')).toHaveCount(1);
  await page.getByRole('button', { name: `Open ${titles[1]}`, exact: true }).click();
  await expect(page.locator('.graph-view')).toBeVisible();
  await expect(page.getByRole('heading', { name: titles[1], exact: true })).toBeVisible();
  await page
    .getByRole('navigation', { name: 'Main navigation' })
    .getByRole('button', { name: 'Pipelines', exact: true })
    .click();
  await expect(page.locator('.pipeline-card')).toHaveCount(4);
  await page.locator('.sidebar-pipelines > button').filter({ hasText: titles[2] }).click();
  await expect(page.getByRole('heading', { name: titles[2], exact: true })).toBeVisible();
  await page.reload();
  await expect(page.locator('.pipeline-card')).toHaveCount(4);
  await expect(page.locator('.graph-view')).toHaveCount(0);
  const saved = await page.evaluate((key) => JSON.parse(localStorage.getItem(key)!), key);
  expect(saved.starterRevision).toBe(3);
  expect(
    saved.workspaces.map((workspace: { source: string }) => parse(workspace.source).metadata.title),
  ).toEqual(titles);
  await page.screenshot({ path: testInfo.outputPath('starter-pipeline-list.png'), fullPage: true });
  expect(errors).toEqual([]);
});

test('Library separates runnable starters, building blocks and guided demo and opens a selected template', async ({
  page,
}) => {
  await page.goto('/');
  await page
    .locator('.app-sidebar')
    .getByRole('button', { name: 'New pipeline', exact: true })
    .click();
  await expect(page.getByRole('button', { name: 'Ready to run', exact: true })).toHaveAttribute(
    'aria-pressed',
    'true',
  );
  await expect(page.locator('.template-card')).toHaveCount(4);
  await page.getByRole('button', { name: 'Guided demo', exact: true }).click();
  await expect(page.locator('.template-card')).toHaveCount(1);
  await expect(page.locator('.template-card')).toContainText('Research brief');
  await page.getByRole('button', { name: 'Building blocks', exact: true }).click();
  await expect(
    page.locator('.template-card').filter({ hasText: 'Local Ollama greeting' }),
  ).toHaveCount(1);
  await page.getByRole('button', { name: 'Ready to run', exact: true }).click();
  await page.locator('.template-card').filter({ hasText: titles[0] }).click();
  await expect(page.getByRole('heading', { name: titles[0], exact: true })).toBeVisible();
  await expect(page.locator('.graph-view')).toBeVisible();
  await expect(page.locator('.sidebar-pipelines > button')).toHaveCount(5);
});

test('unreadable local data remains downloadable and a reviewed legacy backup restores exactly after reload', async ({
  page,
}) => {
  const raw = '{"version":1,"workspaces":[BROKEN USER BYTES';
  await page.addInitScript(
    ({ key, raw }) => {
      if (!sessionStorage.getItem('corruption-seeded')) {
        localStorage.setItem(key, raw);
        sessionStorage.setItem('corruption-seeded', '1');
      }
    },
    { key, raw },
  );
  await page.goto('/');
  await expect(
    page.getByRole('heading', { name: 'Could not open the saved workspace' }),
  ).toBeVisible();
  expect(await page.evaluate((key) => localStorage.getItem(key), key)).toBe(raw);
  const downloaded = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Download original data' }).click();
  expect(readFileSync((await (await downloaded).path())!, 'utf8')).toBe(raw);
  const source = readFileSync('../examples/starter/hello/pipeline.yaml', 'utf8');
  const backup = {
    version: 1,
    workspaces: [
      {
        id: 'restored-draft',
        entrypoint: 'pipeline.yaml',
        source,
        savedSource: source,
        files: [],
        updatedAt: '2026-01-01T10:00:00Z',
      },
    ],
    runs: [],
    activeId: 'restored-draft',
    engineUrl: 'http://127.0.0.1:8787',
    theme: 'light',
    compact: false,
    locale: 'en',
  };
  await page.getByLabel('Restore from a backup', { exact: true }).setInputFiles({
    name: 'backup.json',
    mimeType: 'application/json',
    buffer: Buffer.from(JSON.stringify(backup)),
  });
  await expect(
    page.getByText('Replace the unreadable workspace with this verified backup?', { exact: false }),
  ).toBeVisible();
  expect(await page.evaluate((key) => localStorage.getItem(key), key)).toBe(raw);
  await page.getByRole('button', { name: 'Restore backup', exact: true }).click();
  await expect(page.locator('.pipeline-card')).toHaveCount(1);
  await page.reload();
  await expect(page.locator('.pipeline-card')).toHaveCount(1);
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  const restored = await page.evaluate((key) => JSON.parse(localStorage.getItem(key)!), key);
  expect(restored.workspaces).toEqual(backup.workspaces);
  expect(restored.starterRevision).toBe(3);
});

test('a disconnected starter directs execution to engine settings', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('button', { name: `Open ${titles[0]}`, exact: true }).click();
  await expect(page.getByRole('button', { name: 'Run', exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: 'Connect engine', exact: true }).click();
  await expect(
    page.getByRole('heading', { name: 'Workspace settings', exact: true }),
  ).toBeVisible();
  await expect(page.getByRole('textbox', { name: 'Engine base URL', exact: true })).toBeVisible();
});

test.describe('Russian starter workspace', () => {
  test.use({ locale: 'ru-RU' });
  test('starts localized and keeps list and library legible in the light theme', async ({
    page,
  }, testInfo) => {
    const ru = JSON.parse(readFileSync('src/locales/ru.json', 'utf8'));
    await page.goto('/');
    await expect(page.locator('html')).toHaveAttribute('lang', 'ru');
    await expect(page.locator('.pipeline-card')).toHaveCount(4);
    await expect(page.locator('.pipeline-card').first()).not.toContainText(titles[0]);
    await page.getByRole('button', { name: ru['navigation.settings'], exact: true }).click();
    await page.getByRole('radio', { name: ru['resources.light'], exact: true }).click();
    const navigation = page.getByRole('navigation', { name: ru['navigation.main'] });
    await navigation.getByRole('button', { name: ru['navigation.pipelines'], exact: true }).click();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
    await page.screenshot({
      path: testInfo.outputPath('starter-pipeline-list-ru-light.png'),
      fullPage: true,
    });
    await page
      .locator('.app-sidebar')
      .getByRole('button', { name: ru['shell.newPipeline'], exact: true })
      .click();
    await expect(page.locator('.template-card')).toHaveCount(4);
    await page.screenshot({
      path: testInfo.outputPath('starter-library-ru-light.png'),
      fullPage: true,
    });
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth),
    ).toBe(true);
  });
});
