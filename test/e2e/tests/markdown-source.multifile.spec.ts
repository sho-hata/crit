import { test, expect, type APIRequestContext } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { addComment, clearAllComments, loadPage, revealFile, fileHeader, diffLine, openLineComment, roundComplete, sessionCwd } from './helpers';

// Files mode: a markdown file can show its source even with no diff to show.

const MD = 'plan.md';

async function fileContent(request: APIRequestContext, filePath: string) {
  const res = await request.get(`/api/file?path=${encodeURIComponent(filePath)}`);
  return ((await res.json()) as { content: string }).content;
}

async function commentsOn(request: APIRequestContext, filePath: string) {
  const res = await request.get(`/api/file/comments?path=${encodeURIComponent(filePath)}`);
  return (await res.json()) as { start_line: number; end_line: number; body: string }[];
}

test.describe('Markdown Source view — files mode', () => {
  test.beforeEach(async ({ page, request }) => {
    await clearAllComments(request);
    await loadPage(page);
  });

  test('a markdown file with no diff has a Document / Source toggle', async ({ page }) => {
    await revealFile(page, MD);
    const toggle = fileHeader(page, MD).locator('.file-header-toggle');
    await expect(toggle.locator('.toggle-btn')).toHaveText(['Document', 'Source']);
    await expect(toggle.locator('[data-mode="document"]')).toHaveAttribute('aria-pressed', 'true');
  });

  test('Source shows the raw file and Document switches back', async ({ page }) => {
    const item = await revealFile(page, MD);
    const toggle = fileHeader(page, MD).locator('.file-header-toggle');
    await toggle.locator('[data-mode="source"]').click();
    await expect(toggle.locator('[data-mode="source"]')).toHaveAttribute('aria-pressed', 'true');
    await expect(diffLine(item, 1)).toHaveText('# Migration Plan');

    await toggle.locator('[data-mode="document"]').click();
    await expect(item.locator('.pierre-document h1')).toHaveText('Migration Plan');
  });

  test('a line comment made in Source is on that line of the file', async ({ page, request }) => {
    const item = await revealFile(page, MD);
    await fileHeader(page, MD).locator('[data-mode="source"]').click();
    await expect(diffLine(item, 1)).toHaveText('# Migration Plan');

    const form = await openLineComment(page, item, 3);
    await form.locator('textarea').fill('Comment from source');
    await form.locator('.btn-primary').click();

    await expect.poll(async () => (await commentsOn(request, MD)).map(c => [c.start_line, c.body]))
      .toEqual([[3, 'Comment from source']]);
  });

  test('an existing line comment shows on its line in Source', async ({ page, request }) => {
    await addComment(request, MD, 1, 'Rename this plan');
    await loadPage(page);
    const item = await revealFile(page, MD);
    await fileHeader(page, MD).locator('[data-mode="source"]').click();
    await expect(diffLine(item, 1)).toHaveText('# Migration Plan');
    await expect(item.locator('.comment-card', { hasText: 'Rename this plan' })).toBeVisible();
  });

  test('Source sits next to Diff while the file has changes', async ({ page, request }) => {
    const file = path.join(await sessionCwd(request), MD);
    const original = fs.readFileSync(file, 'utf8');
    fs.writeFileSync(file, original.replace('# Migration Plan', '# Migration Plan v2'));
    try {
      await roundComplete(request);
      await loadPage(page);
      const item = await revealFile(page, MD);
      const toggle = fileHeader(page, MD).locator('.file-header-toggle');
      await expect(toggle.locator('.toggle-btn')).toHaveText(['Document', 'Diff', 'Source']);
      await toggle.locator('[data-mode="source"]').click();
      await expect(diffLine(item, 1)).toHaveText('# Migration Plan v2');
    } finally {
      fs.writeFileSync(file, original);
      await roundComplete(request);
    }
  });

  test('Diff goes back to Document when a round leaves the file with no changes', async ({ page, request }) => {
    const file = path.join(await sessionCwd(request), MD);
    const original = fs.readFileSync(file, 'utf8');
    const v2 = original.replace('# Migration Plan', '# Migration Plan v2');
    fs.writeFileSync(file, v2);
    try {
      await roundComplete(request);
      await loadPage(page);
      await revealFile(page, MD);
      const toggle = fileHeader(page, MD).locator('.file-header-toggle');
      await toggle.locator('[data-mode="diff"]').click();
      await expect(toggle.locator('[data-mode="diff"]')).toHaveAttribute('aria-pressed', 'true');

      // Edit the file and put it back in the same round: the round ends with no hunks.
      fs.writeFileSync(file, original.replace('# Migration Plan', '# Migration Plan v3'));
      await expect.poll(async () => (await fileContent(request, MD)).startsWith('# Migration Plan v3')).toBe(true);
      fs.writeFileSync(file, v2);
      await expect.poll(async () => (await fileContent(request, MD)).startsWith('# Migration Plan v2')).toBe(true);
      await roundComplete(request);

      await expect(toggle.locator('.toggle-btn')).toHaveText(['Document', 'Source']);
      await expect(toggle.locator('[data-mode="document"]')).toHaveAttribute('aria-pressed', 'true');
    } finally {
      fs.writeFileSync(file, original);
      await roundComplete(request);
    }
  });
});
