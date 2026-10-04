import { expect, test, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

test.use({ locale: 'en-US' });

const workspaceKey = 'knotra.workspace.v1';
const catalogs: Record<'en' | 'ru', Record<string, string>> = {
  en: JSON.parse(readFileSync(resolve('src/locales/en.json'), 'utf8')),
  ru: JSON.parse(readFileSync(resolve('src/locales/ru.json'), 'utf8')),
};
function label(key: string, locale: 'en' | 'ru') {
  const value = catalogs[locale][key];
  if (!value) throw new Error(`Missing ${locale} test label: ${key}`);
  return value;
}
const ru = (key: string) => label(key, 'ru');

async function persisted(page: Page) {
  return page.evaluate((key) => JSON.parse(localStorage.getItem(key) ?? 'null'), workspaceKey);
}

async function open(page: Page) {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Research brief', exact: true })).toBeVisible();
  await expect.poll(() => persisted(page)).not.toBeNull();
}

async function settings(page: Page, locale: 'en' | 'ru') {
  await page
    .getByRole('button', { name: label('navigation.settings', locale), exact: true })
    .click();
  await expect(
    page.getByRole('heading', {
      name: label('resources.workspaceSettings', locale),
      exact: true,
    }),
  ).toBeVisible();
}

async function contentSnapshot(page: Page) {
  return page.evaluate((key) => {
    const saved = JSON.parse(localStorage.getItem(key) ?? 'null');
    if (!saved) return null;
    const { locale: _locale, ...workspace } = saved;
    const engineCaches = Object.fromEntries(
      Object.keys(localStorage)
        .filter((key) => key.startsWith('knotra.engine.v1:'))
        .sort()
        .map((key) => [key, localStorage.getItem(key)]),
    );
    return { workspace, engineCaches };
  }, workspaceKey);
}

test('language switches live and persists without changing saved user content or engine caches', async ({
  page,
}) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await open(page);
  const seed = await persisted(page);
  const userTitle = 'navigation.settings';
  seed.workspaces[0].source = seed.workspaces[0].source.replace(
    'title: Research brief',
    `title: ${userTitle}`,
  );
  seed.workspaces[0].source += '\n# User content — Данные $& {name}\n';
  seed.workspaces[0].savedSource = seed.workspaces[0].source;
  seed.runs = [
    {
      id: 'saved-localization-demo',
      workspaceId: seed.workspaces[0].id,
      title: 'User title — Заголовок',
      topic: 'User topic — Тема $ {name}',
      createdAt: '2026-01-01T10:00:00Z',
      updatedAt: '2026-01-01T10:00:00Z',
      status: 'succeeded',
      source: seed.workspaces[0].source,
      files: seed.workspaces[0].files,
      inputs: { topic: 'User topic — Тема $ {name}' },
      nodes: {
        discover: 'succeeded',
        research: 'succeeded',
        draft: 'succeeded',
        review: 'succeeded',
        publish: 'succeeded',
      },
      events: [
        {
          id: 'saved-event',
          at: '2026-01-01T10:00:00Z',
          message: 'Saved content — Сохранённые данные $& {count}',
        },
      ],
      outputs: { answer: 'User output — Результат $& {name}' },
      artifacts: [],
    },
  ];
  const cacheKey = `knotra.engine.v1:${JSON.stringify([
    seed.engineUrl,
    'cached-engine',
    'cached-principal',
  ])}`;
  const cache = {
    cache: {
      savedRun: {
        id: 'saved-engine-run',
        title: 'Engine title — Заголовок',
        outputs: { result: '$& {count}\nEngine output — Данные движка' },
      },
    },
    pending: [],
  };
  await page.addInitScript(
    ({ seed, cacheKey, cache, workspaceKey }) => {
      if (localStorage.getItem('knotra.localization.fixture-installed')) return;
      localStorage.setItem(workspaceKey, JSON.stringify(seed));
      localStorage.setItem(cacheKey, JSON.stringify(cache));
      localStorage.setItem('knotra.localization.fixture-installed', 'true');
    },
    { seed, cacheKey, cache, workspaceKey },
  );
  await page.reload();
  await expect(page.getByRole('heading', { name: userTitle, exact: true })).toBeVisible();
  await expect.poll(async () => (await persisted(page))?.runs?.length).toBe(1);
  await expect
    .poll(async () => Object.keys((await persisted(page))?.workspaces?.[0]?.positions ?? {}).length)
    .toBe(5);
  const before = await contentSnapshot(page);

  await settings(page, 'en');
  await page.getByRole('combobox', { name: 'Language', exact: true }).selectOption('ru');
  await expect(page.locator('html')).toHaveAttribute('lang', 'ru');
  await expect(
    page.getByRole('heading', { name: 'Настройки рабочего пространства', exact: true }),
  ).toBeVisible();
  await expect(page.getByRole('combobox', { name: 'Язык', exact: true })).toHaveValue('ru');
  await expect.poll(async () => (await persisted(page))?.locale).toBe('ru');
  expect(await contentSnapshot(page)).toEqual(before);

  await page.reload();
  await expect(page.locator('html')).toHaveAttribute('lang', 'ru');
  await expect(page.getByRole('heading', { name: userTitle, exact: true })).toBeVisible();
  await settings(page, 'ru');
  await expect(page.getByRole('combobox', { name: 'Язык', exact: true })).toHaveValue('ru');
  expect(await contentSnapshot(page)).toEqual(before);

  await page.getByRole('button', { name: ru('resources.saveAddress'), exact: true }).click();
  await expect(page.locator('.toast')).toHaveText(
    ru('resources.engineAddressSavedConnectToCheckItsProtocol'),
  );
  await page.getByRole('combobox', { name: 'Язык', exact: true }).selectOption('en');
  await expect(page.locator('html')).toHaveAttribute('lang', 'en');
  await expect(
    page.getByRole('heading', { name: 'Workspace settings', exact: true }),
  ).toBeVisible();
  await expect(page.locator('.toast')).toHaveText(
    label('resources.engineAddressSavedConnectToCheckItsProtocol', 'en'),
  );
  await expect.poll(async () => (await persisted(page))?.locale).toBe('en');
  expect(await contentSnapshot(page)).toEqual(before);
  expect(errors).toEqual([]);
});

test('Russian navigation, search, Library and editor expose localized accessible names', async ({
  page,
}) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await open(page);
  const source = (await persisted(page)).workspaces[0].source;
  await settings(page, 'en');
  await page.getByRole('combobox', { name: 'Language', exact: true }).selectOption('ru');

  const dark = page.getByRole('radio', { name: 'Тёмная', exact: true });
  await dark.focus();
  await dark.press('ArrowRight');
  await expect(page.getByRole('radio', { name: 'Светлая', exact: true })).toBeFocused();
  await expect(page.getByRole('radio', { name: 'Светлая', exact: true })).toHaveAttribute(
    'aria-checked',
    'true',
  );

  const engineUrl = page.getByRole('textbox', { name: ru('resources.engineBaseUrl'), exact: true });
  const savedEngineUrl = (await persisted(page)).engineUrl;
  await engineUrl.fill('not an engine URL');
  await page.getByRole('button', { name: ru('resources.saveAddress'), exact: true }).click();
  await expect(page.getByRole('alert')).toHaveText(ru('resources.enterAValidEngineUrl'));
  expect((await persisted(page)).engineUrl).toBe(savedEngineUrl);
  await engineUrl.fill(savedEngineUrl);

  await page.getByRole('button', { name: ru('shell.findAnything') }).click();
  const search = page.getByRole('textbox', { name: ru('shell.searchWorkspace'), exact: true });
  await search.fill(ru('navigation.resources'));
  await page
    .getByRole('dialog')
    .getByRole('button', { name: new RegExp(ru('navigation.resources')) })
    .click();
  await expect(
    page.getByRole('heading', { name: 'Модели и инструменты', exact: true }),
  ).toBeVisible();

  await page.getByRole('button', { name: ru('shell.newPipeline'), exact: true }).click();
  const library = page.getByRole('dialog');
  await expect(
    library.getByRole('heading', { name: ru('library.title'), exact: true }),
  ).toBeVisible();
  await expect(
    library.getByRole('heading', { name: ru('shell.researchBrief'), exact: true }),
  ).toBeVisible();
  await library.getByRole('button', { name: ru('common.closeDialog'), exact: true }).click();

  await page
    .getByRole('navigation', { name: ru('navigation.main'), exact: true })
    .getByRole('button', { name: ru('navigation.pipelines'), exact: true })
    .click();
  await expect(page.getByRole('heading', { name: 'Research brief', exact: true })).toBeVisible();
  await expect(
    page.getByRole('textbox', { name: ru('editor.description'), exact: true }),
  ).toBeVisible();
  await page.getByRole('tab', { name: ru('editor.code'), exact: true }).click();
  const editor = page.getByRole('textbox', { name: ru('editor.pipelineYaml'), exact: true });
  await expect(editor).toBeVisible();
  await expect(editor).toContainText('model_main');
  await editor.press(process.platform === 'darwin' ? 'Meta+f' : 'Control+f');
  await expect(page.locator('.cm-search input[name="search"]')).toHaveAttribute(
    'aria-label',
    ru('editor.find'),
  );
  expect((await persisted(page)).workspaces[0].source).toBe(source);

  await page.getByRole('button', { name: ru('shell.newPipeline'), exact: true }).click();
  await page
    .getByRole('dialog')
    .getByRole('button', { name: ru('shell.startFromScratch'), exact: true })
    .click();
  await page
    .getByRole('navigation', { name: ru('navigation.main'), exact: true })
    .getByRole('button', { name: ru('navigation.resources'), exact: true })
    .click();
  for (const [tab, empty] of [
    ['resources.models', 'resources.noModelsDeclared'],
    ['resources.mcpTools', 'resources.noMcpToolsDeclared'],
    ['resources.sandboxes', 'resources.noSandboxesDeclared'],
    ['resources.secretReferences', 'resources.noSecretReferencesDeclared'],
  ]) {
    await page
      .locator('.resource-tabs')
      .getByRole('button', { name: new RegExp(ru(tab)) })
      .click();
    await expect(page.getByRole('heading', { name: ru(empty), exact: true })).toBeVisible();
  }
  expect(errors).toEqual([]);
});
