import { expect, test } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { unzipSync } from 'fflate';
import { parse, parseDocument } from 'yaml';
import { activeWorkspace, addResearchDemo } from './authoring';

test('uploading a missing declared file preserves its declaration and restores a valid package', async ({
  page,
}) => {
  await page.goto('/');
  await addResearchDemo(page);
  const workspace = await activeWorkspace(page);
  const document = parseDocument(workspace.source);
  document.setIn(['spec', 'files'], [...(parse(workspace.source).spec.files ?? []), 'check.mjs']);
  const source = document.toString();
  const content = 'export const check = () => true;\n';
  await page.getByRole('tab', { name: 'Code', exact: true }).click();
  await page.getByRole('textbox', { name: 'Pipeline YAML', exact: true }).fill(source);
  await expect(page.locator('.validation-summary')).toContainText('1 issue to resolve');
  await page.getByRole('tab', { name: /^Files/ }).click();
  const upload = page.locator('.file-list input[type=file]');
  await upload.setInputFiles({
    name: 'check.mjs',
    mimeType: 'text/javascript',
    buffer: Buffer.from(content),
  });

  await expect(page.locator('.validation-summary')).toContainText('Local checks passed');
  await expect(page.getByRole('heading', { name: 'Research brief', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect.poll(async () => (await activeWorkspace(page)).source).toBe(source);
  const saved = await activeWorkspace(page);
  expect(parse(saved.source).spec.files.filter((path: string) => path === 'check.mjs')).toEqual([
    'check.mjs',
  ]);
  expect(saved.files.filter((file: { path: string }) => file.path === 'check.mjs')).toEqual([
    { path: 'check.mjs', content: Buffer.from(content).toString('base64') },
  ]);

  await upload.setInputFiles({
    name: 'check.mjs',
    mimeType: 'text/javascript',
    buffer: Buffer.from('do not overwrite'),
  });
  await expect(page.getByRole('status')).toContainText('already exists');
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Export', exact: true }).click();
  const exported = unzipSync(new Uint8Array(readFileSync((await (await download).path())!)));
  expect(Buffer.from(exported['check.mjs']).toString()).toBe(content);
  expect(parse(Buffer.from(exported[workspace.entrypoint]).toString()).spec.files).toEqual(
    parse(source).spec.files,
  );
});
