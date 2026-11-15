import { test, expect } from '@playwright/test';
import { clearAllComments, fileHeader, loadPage, revealFile } from './helpers';

// skill.md's "Raw HTML" section carries script payloads. The document renders
// on the API's origin, so none of them may run, while safe raw HTML
// (kbd, details, images, tables) keeps rendering. document-html.js prefixes
// the author's classes with user-content-, as GitHub does.
test.describe('Markdown raw HTML — skill.md', () => {
  test.beforeEach(async ({ request }) => {
    await clearAllComments(request);
  });

  test('renders safe raw HTML and drops script payloads in Document view', async ({ page }) => {
    await loadPage(page);
    await revealFile(page, 'skill.md');
    await fileHeader(page, 'skill.md').locator('.file-header-toggle .toggle-btn[data-mode="document"]').click();
    const section = page.locator('[id="file-section-skill.md"].pierre-document');
    await expect(section.locator('.document-wrapper')).toBeVisible();

    await expect(section.locator('kbd', { hasText: 'Ctrl' })).toBeVisible();
    await expect(section.locator('details.user-content-xss-details summary', { hasText: 'More' })).toBeVisible();
    await expect(section.locator('td', { hasText: 'one' })).toBeVisible();

    // Images survive without their handlers; wait until both failed loads settled.
    const images = section.locator('img.user-content-xss-img, img.user-content-xss-cell');
    await expect(images).toHaveCount(2);
    await expect.poll(() => images.evaluateAll((els) => els.every((el) => (el as HTMLImageElement).complete))).toBe(true);
    for (const img of await images.all()) {
      await expect(img).not.toHaveAttribute('onerror', /.*/);
    }
    await expect(section.locator('details.user-content-xss-details')).not.toHaveAttribute('ontoggle', /.*/);
    await expect(section.locator('a.user-content-xss-link')).not.toHaveAttribute('href', /javascript:/i);
    await expect(section.locator('form, button:has-text("Approve"), svg[onload]')).toHaveCount(0);

    expect(await page.evaluate(() => (window as unknown as { __critDocXss?: string }).__critDocXss)).toBeUndefined();
  });

  test('a document label cannot activate the Approve button', async ({ page }) => {
    await loadPage(page);
    await revealFile(page, 'skill.md');
    await fileHeader(page, 'skill.md').locator('.file-header-toggle .toggle-btn[data-mode="document"]').click();
    const label = page.locator('[id="file-section-skill.md"].pierre-document label.user-content-xss-label');
    await expect(label).toBeVisible();
    await expect(label).not.toHaveAttribute('for', /.*/);

    // Record (and swallow) any click reaching Approve so a regression can't finish the shared review.
    await page.evaluate(() => {
      window.addEventListener('click', (e) => {
        if ((e.target as Element).id === 'finishBtn') {
          (window as unknown as { __finishClicked?: boolean }).__finishClicked = true;
          e.stopImmediatePropagation();
          e.preventDefault();
        }
      }, true);
    });
    await label.click();
    expect(await page.evaluate(() => (window as unknown as { __finishClicked?: boolean }).__finishClicked)).toBeUndefined();
  });
});
