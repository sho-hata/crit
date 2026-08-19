import { test, expect } from '@playwright/test';
import { clearAllComments, loadPage } from './helpers';

// The one-row header needs ~1300px once a long branch name fills the chip.
// Above the mobile breakpoint (which hides half the chrome) nothing used to
// give, so narrow desktop windows pushed Approve off-screen and gave the
// document a horizontal scrollbar. These widths cover the gap between the
// mobile rules (≤768px) and a roomy desktop.
const WIDTHS = [1280, 1100, 960, 820];

test.describe('Header at narrow desktop widths', () => {
  test.beforeEach(async ({ page, request }) => {
    await clearAllComments(request);
  });

  for (const width of WIDTHS) {
    test(`no horizontal page overflow at ${width}px with a long branch name`, async ({ page }) => {
      await page.setViewportSize({ width, height: 700 });
      await loadPage(page);
      // Force the worst case: a branch name far longer than the fixture's.
      await page.evaluate(() => {
        document.getElementById('branchName')!.textContent =
          'feat/sole-proprietor-applications-list-with-a-very-long-name';
      });

      await expect(async () => {
        const overflow = await page.evaluate(
          () => document.documentElement.scrollWidth - window.innerWidth
        );
        expect(overflow).toBeLessThanOrEqual(0);
      }).toPass();

      // Approve is the primary action — it must stay inside the viewport.
      const approve = page.locator('#finishBtn');
      await expect(approve).toBeVisible();
      const box = (await approve.boundingBox())!;
      expect(box.x + box.width).toBeLessThanOrEqual(width);

      // The viewed counter must stay on one line (it used to wrap to three).
      const viewed = page.locator('#viewedCount');
      const height = await viewed.evaluate(el => el.getBoundingClientRect().height);
      expect(height).toBeLessThan(28);
    });
  }
});
