import { test, expect } from '@playwright/test';
import { clearAllLivePins, seedLivePin } from './livemode-helpers';

// Once the daemon sends server-shutdown, live mode's own finish flow must not
// re-enable the button: clicking it would POST /api/finish to a stopped server.
test.describe('finish button after server shutdown', () => {
  test.beforeEach(async ({ page, request }) => {
    await clearAllLivePins(request);
    // Stand in for a daemon that just stopped: the event stream delivers
    // server-shutdown, while the other endpoints keep answering.
    await page.route('**/api/events', (route) =>
      route.fulfill({
        status: 200,
        headers: { 'content-type': 'text/event-stream' },
        body: 'event: server-shutdown\ndata: {"type":"server-shutdown"}\n\n',
      }),
    );
  });

  test('shows a disabled Session complete button', async ({ page }) => {
    await page.goto('/live');

    await expect(page.locator('.disconnected-banner')).toBeVisible();
    const finishBtn = page.locator('#finishBtn');
    await expect(finishBtn).toHaveText('Session complete');
    await expect(finishBtn).toBeDisabled();
    await expect(finishBtn).not.toHaveClass(/btn-primary/);
  });

  test('stays locked when a pin is resolved afterwards', async ({ page, request }) => {
    await seedLivePin(request, 'Fix this', { pathname: '/', css_selector: '#primary-btn', tag_chain: ['BUTTON'] });
    await page.goto('/live');
    const finishBtn = page.locator('#finishBtn');
    await expect(finishBtn).toHaveText('Session complete');

    // Resolving refreshes the panel, which relabels the finish button.
    await page.locator('#commentsPanelBody .crit-live-comment-resolve').first().click();
    await expect(page.locator('#commentsPanelBody .crit-live-comment-resolve[title="Unresolve"]')).toHaveCount(1);

    await expect(finishBtn).toHaveText('Session complete');
    await expect(finishBtn).toBeDisabled();
  });
});
