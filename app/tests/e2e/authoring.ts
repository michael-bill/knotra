import { expect, type Page } from '@playwright/test';

export async function activeWorkspace(page: Page) {
  return page.evaluate(() => {
    const state = JSON.parse(localStorage.getItem('knotra.workspace.v1')!);
    return state.workspaces.find((workspace: { id: string }) => workspace.id === state.activeId);
  });
}

export async function openActiveEditor(page: Page) {
  await expect
    .poll(() => page.evaluate(() => localStorage.getItem('knotra.workspace.v1')))
    .not.toBeNull();
  const index = await page.evaluate(() => {
    const state = JSON.parse(localStorage.getItem('knotra.workspace.v1')!);
    return state.workspaces.findIndex(
      (workspace: { id: string }) => workspace.id === state.activeId,
    );
  });
  await page.locator('.sidebar-pipelines > button').nth(index).click();
}

export async function addResearchDemo(page: Page) {
  await expect(page.locator('.pipelines-page')).toBeVisible();
  await page
    .locator('.app-sidebar')
    .getByRole('button', { name: 'New pipeline', exact: true })
    .click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('button', { name: 'Guided demo', exact: true }).click();
  await dialog.getByRole('button', { name: /^Research brief / }).click();
  await expect(page.getByRole('heading', { name: 'Research brief', exact: true })).toBeVisible();
}
