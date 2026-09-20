import { test, expect, type Page } from '@playwright/test';
import { clearAllComments, loadPage, goSection, mdSection } from './helpers';

// LSP wiring in file mode (`crit server.go handler.js plan.md`), where code
// files render in document view — the whole file, one .line-block per source
// line — instead of the dual-gutter diff markup. Hover/definition used to be
// bound to diff markup only, so the feature silently did nothing here.
//
// The language server itself is mocked: /api/lsp/* is intercepted so the test
// runs on machines without gopls and asserts the frontend wiring (which line
// and column the UI resolves, where the jump lands), not gopls's answers.

const HOVER_MARKDOWN = 'authMiddleware checks for a valid API key.';

/** Line the definition mock points at: `func logRequest(...)` in server.go. */
const DEF_LINE = 19;

type Recorded = { path: string; line: string; char: string };

/** Route /api/config + /api/lsp/* so LSP is on and answers deterministically. */
async function mockLSP(page: Page, calls: { hover: Recorded[]; definition: Recorded[] }) {
  await page.route('**/api/config', async route => {
    const res = await route.fetch();
    const json = await res.json();
    json.lsp_available = true;
    json.lsp_extensions = ['go', 'ts', 'tsx', 'js', 'jsx'];
    await route.fulfill({ response: res, json });
  });

  const record = (url: string): Recorded => {
    const q = new URL(url).searchParams;
    return { path: q.get('path') ?? '', line: q.get('line') ?? '', char: q.get('char') ?? '' };
  };

  await page.route('**/api/lsp/hover*', async route => {
    calls.hover.push(record(route.request().url()));
    await route.fulfill({ json: { contents: HOVER_MARKDOWN } });
  });

  await page.route('**/api/lsp/definition*', async route => {
    calls.definition.push(record(route.request().url()));
    await route.fulfill({
      json: {
        locations: [{
          path: 'server.go',
          display_path: 'server.go',
          line: DEF_LINE,
          in_session: true,
          in_repo: true,
          peek_start: DEF_LINE,
          peek: ['func logRequest(r *http.Request) {'],
        }],
      },
    });
  });
}

/** The document-view block holding `func authMiddleware(...)` in server.go. */
function authMiddlewareLine(page: Page) {
  return goSection(page).locator('.code-document .line-block')
    .filter({ hasText: 'func authMiddleware' });
}

test.describe('LSP — File Mode — document view', () => {
  let calls: { hover: Recorded[]; definition: Recorded[] };

  test.beforeEach(async ({ page, request }) => {
    await clearAllComments(request);
    calls = { hover: [], definition: [] };
    await mockLSP(page, calls);
  });

  test('code files render as .code-document, markdown does not', async ({ page }) => {
    await loadPage(page);
    // The scope LSP eligibility keys off: only code files carry it, which is
    // what keeps hover off rendered markdown prose (same .line-block markup).
    await expect(goSection(page).locator('.document-wrapper.code-document')).toHaveCount(1);
    await expect(mdSection(page).locator('.document-wrapper.code-document')).toHaveCount(0);
  });

  test('hover over a code line shows the tooltip', async ({ page }) => {
    await loadPage(page);
    const line = authMiddlewareLine(page);
    await expect(line).toHaveCount(1);
    const lineNum = await line.getAttribute('data-start-line');

    await line.locator('code').scrollIntoViewIfNeeded();
    await line.locator('code').hover();

    const tooltip = page.locator('.lsp-tooltip');
    await expect(tooltip).toBeVisible();
    await expect(tooltip).toContainText('authMiddleware checks for a valid API key');

    expect(calls.hover[0].path).toBe('server.go');
    expect(calls.hover[0].line).toBe(lineNum);
    expect(Number(calls.hover[0].char)).toBeGreaterThan(0);
  });

  test('Cmd/Ctrl+Click jumps to the definition inside the document view', async ({ page }) => {
    await loadPage(page);
    const line = authMiddlewareLine(page);
    await expect(line).toHaveCount(1);

    await line.locator('code').scrollIntoViewIfNeeded();
    await line.locator('code').click({ modifiers: ['ControlOrMeta'] });

    await expect.poll(() => calls.definition.length).toBe(1);
    expect(calls.definition[0].path).toBe('server.go');

    // Handled in-review: the target line flashes. Falling back to the peek
    // popup instead is exactly the regression this guards.
    const target = goSection(page).locator(`.line-block[data-start-line="${DEF_LINE}"]`);
    await expect(target).toHaveClass(/lsp-jump-flash/);
    await expect(page.locator('.lsp-peek')).toHaveCount(0);
  });
});
