import { test, expect } from '@playwright/test';
import { clearAllLivePins, forgeUnauthedWrite, getIframe, seedLivePin, waitForAgentReady } from './livemode-helpers';

test.describe('security — two-port origin separation (Scenario 17)', () => {
  test.beforeEach(async ({ request }) => {
    await clearAllLivePins(request);
  });

  test('upstream-origin fetch to API port is blocked by browser CORS', async ({ page, request }) => {
    await page.goto('/live');
    // Wait for iframe to be navigable so we can run script in its origin.
    await expect(getIframe(page).locator('#title')).toBeVisible();

    // The API origin is the chrome's origin; iframe is on the proxy origin.
    const apiOrigin = new URL(page.url()).origin;

    const result = await forgeUnauthedWrite(
      page,
      { start_line: 0, end_line: 0, body: 'forged', dom_anchor: { pathname: '/' } },
      apiOrigin,
    );
    // CORS preflight failure throws TypeError; status is undefined.
    expect(result.ok).toBe(false);
    expect(result.error ?? '').toMatch(/Failed to fetch|TypeError|NetworkError/);

    // Secondary: forged write did not land. Read all comments via API.
    const sessionResp = await request.get('/api/session');
    const session = await sessionResp.json();
    const totalComments =
      (session.review_comments?.length ?? 0) +
      (session.files ?? []).reduce(
        (acc: number, f: { comments?: unknown[] }) => acc + (f.comments?.length ?? 0),
        0,
      );
    expect(totalComments).toBe(0);
  });
});

// The page under review learns every pin id from set-pins, so a forged
// selection with reanchor_for must not move an anchor unless the reviewer
// armed re-anchor for that pin.
test.describe('security — unsolicited re-anchor', () => {
  test.beforeEach(async ({ request }) => {
    await clearAllLivePins(request);
  });

  test('a forged reanchor_for selection sends no PUT', async ({ page, request }) => {
    const pin = await seedLivePin(request, 'Anchored pin', { pathname: '/', css_selector: '#title', tag_chain: ['H1'] });
    await waitForAgentReady(page);

    const puts: string[] = [];
    page.on('request', (req) => {
      if (req.method() === 'PUT' && req.url().includes('/api/comment/')) puts.push(req.url());
    });
    const iframe = page.frames().find((f) => f !== page.mainFrame());
    expect(iframe).toBeTruthy();
    await iframe!.evaluate((pinId) => {
      const anchor = {
        pathname: '/', css_selector: '#forged', tag_chain: ['DIV'], outer_html: '<div id="forged"></div>',
        accessible_name: '', role: '', landmark: '', viewport_width: 1280, viewport_height: 800,
      };
      window.parent.postMessage({ type: 'selection', dom_anchor: anchor, reanchor_for: pinId }, '*');
      // A second, harmless message: once it is logged the forged one was dispatched.
      window.parent.postMessage({ type: 'agent-error', kind: 'e2e-sentinel', message: 'sentinel' }, '*');
    }, pin.id);

    await expect.poll(() => page.evaluate(() => {
      const log = (window as unknown as { __critLiveMessages?: { type: string; kind?: string }[] }).__critLiveMessages ?? [];
      return log.some((m) => m.type === 'selection') && log.some((m) => m.kind === 'e2e-sentinel');
    })).toBe(true);
    expect(puts).toEqual([]);

    const comments = await (await request.get('/api/file/comments?path=%2F')).json();
    const stored = (comments as { id: string; dom_anchor?: { css_selector?: string } }[]).find((c) => c.id === pin.id);
    expect(stored?.dom_anchor?.css_selector).toBe('#title');
  });
});
