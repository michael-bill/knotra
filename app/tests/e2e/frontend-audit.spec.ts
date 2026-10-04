import { test, expect } from '@playwright/test';

test('switching human requests preserves each unsent response and marks the displayed request', async ({
  page,
}) => {
  await page.goto('/tests/fixtures/frontend-audit.html?component=inbox');
  const first = page.getByRole('button', { name: /root\/first/ });
  const second = page.getByRole('button', { name: /root\/second/ });
  await expect(first).toHaveClass('active');
  const response = page.getByRole('textbox', { name: 'Engine review response' });
  await response.fill('{"feedback":"First review in progress"}');
  await second.click();
  await expect(response).toHaveValue('{}');
  await response.fill('{"feedback":"Second review in progress"}');
  await first.click();
  await expect(response).toHaveValue('{"feedback":"First review in progress"}');
  await second.click();
  await expect(response).toHaveValue('{"feedback":"Second review in progress"}');
  await page.getByRole('button', { name: 'View run' }).click();
  await expect(response).toHaveCount(0);
  await page.getByRole('button', { name: 'Return to inbox' }).click();
  await expect(response).toHaveValue('{"feedback":"First review in progress"}');
  await second.click();
  await expect(response).toHaveValue('{"feedback":"Second review in progress"}');
});

test('controlled dialog fields keep keyboard focus across rerenders and restore it on close', async ({
  page,
}) => {
  await page.goto('/tests/fixtures/frontend-audit.html?component=modal');
  const trigger = page.getByRole('button', { name: 'Open resolution' });
  await trigger.click();
  const evidence = page.getByRole('textbox', { name: 'Resolution evidence' });
  await evidence.click();
  await evidence.pressSequentially('Operation was not submitted.', { delay: 15 });
  await expect(evidence).toHaveValue('Operation was not submitted.');
  await expect(evidence).toBeFocused();
  await evidence.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(trigger).toBeFocused();
});

test('immutable source snapshots expose search without replacement actions', async ({ page }) => {
  await page.goto('/tests/fixtures/frontend-audit.html?component=snapshot');
  await page.locator('.cm-content').click();
  await page.keyboard.press('ControlOrMeta+f');
  await expect(page.locator('.cm-search')).toBeVisible();
  await expect(page.locator('.cm-search input[name="search"]')).toBeVisible();
  await expect(page.locator('.cm-search input[name="replace"]')).toHaveCount(0);
  await expect(page.locator('.cm-content')).toContainText('value: original');
});

test('artifact upload errors do not block verified exports and transient export failures allow retry', async ({
  page,
}) => {
  let downloads = 0;
  await page.route('http://127.0.0.1:19879/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname;
    const headers = { 'Access-Control-Allow-Origin': '*' };
    if (path === '/v1/info')
      return route.fulfill({
        headers,
        json: {
          protocol: 'knotra.desktop/1',
          engineId: 'audit-engine',
          principalId: 'audit-user',
          version: 'fixture',
          capabilities: [],
        },
      });
    if (path === '/v1/artifacts/artifact-1')
      return route.fulfill({
        headers,
        json: {
          artifact: {
            id: 'artifact-1',
            name: 'hello.txt',
            mediaType: 'text/plain',
            size: 5,
            sha256: '2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824',
            origin: {},
          },
        },
      });
    if (path === '/v1/artifacts/artifact-1/content') {
      if (++downloads === 2)
        return route.fulfill({
          headers,
          status: 503,
          json: { code: 'UNAVAILABLE', message: 'Temporary export failure.' },
        });
      return route.fulfill({ headers, contentType: 'text/plain', body: 'hello' });
    }
    return route.abort();
  });
  await page.goto('/tests/fixtures/frontend-audit.html?component=artifacts');
  await page.getByRole('button', { name: /hello.txt/ }).click();
  await expect(page.locator('.artifact-text')).toHaveText('hello');
  await page.getByLabel('Upload engine artifact').setInputFiles({
    name: 'input.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('input'),
  });
  await expect(page.getByRole('alert')).toHaveText('Upload failed in fixture.');
  const exportButton = page.getByRole('button', { name: 'Export', exact: true });
  await expect(exportButton).toBeEnabled();
  await exportButton.click();
  await expect(page.locator('.artifact-inspector [role="alert"]')).toHaveText(
    'Artifact bytes are unavailable.',
  );
  await expect(exportButton).toBeEnabled();
  const download = page.waitForEvent('download');
  await exportButton.click();
  expect((await download).suggestedFilename()).toBe('hello.txt');
});

for (const replacement of ['review_all', 'new_review']) {
  test(`source edits leaving a nested graph remain editable when its parent becomes ${replacement}`, async ({
    page,
  }) => {
    const errors: string[] = [];
    page.on('pageerror', (error) => errors.push(error.message));
    await page.goto('/tests/fixtures/frontend-audit.html?component=workspace');
    await expect(page.getByRole('button', { name: 'Run demo', exact: true })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Connect engine', exact: true })).toBeEnabled();
    await page.getByRole('button', { name: 'Edit body graph', exact: true }).click();
    await expect(page.locator('.scope-bar')).toContainText('review_all');
    await page.getByRole('tab', { name: 'Code', exact: true }).click();
    await page.locator('.cm-content').fill(`apiVersion: knotra/v1
kind: Pipeline
metadata:
  name: audit-root
spec:
  nodes:
    ${replacement}:
      type: human
      human:
        prompt:
          text: Review at the root.
      outputs:
        answer:
          schema:
            type: string
  outputs:
    answer:
      schema:
        type: string
      bind:
        from: nodes.${replacement}.outputs.answer
`);
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click();
    await expect(page.locator('.scope-bar')).toHaveCount(0);
    await expect(page.locator('.react-flow__node')).toHaveCount(1);
    await page.getByRole('button', { name: 'Duplicate node', exact: true }).click();
    await expect(page.locator('.react-flow__node')).toHaveCount(2);
    expect(errors).toEqual([]);
  });
}
