import { test, expect } from '@playwright/test';
import { clearAllComments } from './helpers';
import { mockLSP, newCalls, location, hoverForTooltip, chordClickWord, jumpFlashed, type LspCalls } from './lsp-helpers';

// /api/lsp/* is mocked (see lsp-helpers).

/** `func logRequest(...)` in server.go (the working-tree version). */
const DEF_LINE = 19;
/** `logRequest(r)` inside the "/" handler. */
const USE_LINE = 42;

function codeLine(page: Parameters<typeof mockLSP>[0], line: number) {
  return page.locator(`diffs-container code[data-code] [data-content] > [data-line="${line}"]`);
}

test.describe('File viewer — /view', () => {
  let calls: LspCalls;

  test.beforeEach(async ({ request }) => {
    await clearAllComments(request);
    calls = newCalls();
  });

  test('renders the whole file and lands on ?line=', async ({ page }) => {
    await page.goto(`/view?path=server.go&line=${DEF_LINE}`);
    await expect(page).toHaveTitle(/^server\.go · Crit$/);
    await expect(page.locator('#viewPath')).toHaveText('server.go');
    await expect(page.locator('.crit-view-file-header')).toContainText('server.go');
    const target = codeLine(page, DEF_LINE);
    await expect(target).toContainText('func logRequest');
    await expect.poll(() => jumpFlashed(target)).toBe(true);
    await expect(codeLine(page, 1)).toContainText('package main');
    await expect(page.locator('.file-tree-panel')).toBeHidden();
    await expect(page.locator('#finishBtn')).toBeHidden();
  });

  test('a missing file shows a message instead of a page error', async ({ page }) => {
    await page.goto('/view?path=nope/missing.go');
    await expect(page.locator('.crit-view-message')).toContainText('File not found: nope/missing.go');
  });

  test('hover sends the viewer path and shows the tooltip', async ({ page }) => {
    await mockLSP(page, calls);
    await page.goto(`/view?path=server.go&line=${USE_LINE}`);
    const line = codeLine(page, USE_LINE);
    await expect(line).toContainText('logRequest(r)');
    const p = await hoverForTooltip(page, line, 'logRequest');
    await expect(page.locator('.lsp-tooltip')).toContainText('authMiddleware checks for a valid API key');
    expect(calls.hover.length).toBeGreaterThan(0);
    for (const call of calls.hover) expect(call).toEqual({ path: 'server.go', line: String(USE_LINE), char: String(p.char) });
  });

  test('a definition in another repo file opens it in place; Back returns', async ({ page }) => {
    await mockLSP(page, calls, {
      definition: [location('routes.go', 1, ['package main'], { in_session: false })],
    });
    await page.goto(`/view?path=server.go&line=${USE_LINE}`);
    const line = codeLine(page, USE_LINE);
    await expect(line).toContainText('logRequest(r)');
    await chordClickWord(page, line, 'logRequest');

    await expect(page).toHaveURL(/\/view\?path=routes\.go&line=1$/);
    await expect(page.locator('#viewPath')).toHaveText('routes.go');
    const target = codeLine(page, 1);
    await expect(target).toContainText('package main');
    await expect.poll(() => jumpFlashed(target)).toBe(true);
    await expect(page.locator('.lsp-peek')).toHaveCount(0);

    await page.goBack();
    await expect(page).toHaveURL(/\/view\?path=server\.go&line=42$/);
    await expect(page.locator('#viewPath')).toHaveText('server.go');
    await expect(codeLine(page, USE_LINE)).toContainText('logRequest(r)');
  });

  test('a definition outside the repo peeks, with the viewer link', async ({ page }) => {
    await mockLSP(page, calls, {
      definition: [location('/usr/local/go/src/fmt/print.go', 5, ['func Printf()'], { in_session: false, in_repo: false, display_path: 'GOROOT/src/fmt/print.go' })],
    });
    await page.goto(`/view?path=server.go&line=${USE_LINE}`);
    const line = codeLine(page, USE_LINE);
    await expect(line).toContainText('logRequest(r)');
    await chordClickWord(page, line, 'logRequest');
    const peek = page.locator('.lsp-peek');
    await expect(peek).toBeVisible();
    await expect(peek.locator('.lsp-peek-title')).toHaveText('GOROOT/src/fmt/print.go:5');
    await expect(peek.locator('.lsp-peek-open')).toHaveCount(0);
    await expect(page).toHaveURL(/\/view\?path=server\.go/);
  });
});
