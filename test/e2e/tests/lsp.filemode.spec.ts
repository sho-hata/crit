import { test, expect, type Page } from '@playwright/test';
import { clearAllComments, loadPage, goSection, mdSection, diffLine } from './helpers';
import { mockLSP, newCalls, location, hoverForTooltip, chordClickWord, jumpFlashed, HOVER_MARKDOWN, type LspCalls } from './lsp-helpers';

// LSP wiring in file mode, where a code file is one Pierre whole-file item
// (a side-less code[data-code] column) rather than a diff. The hover and
// jump paths have to work on that markup as well as on diffs.

/** `func authMiddleware(...)` in the fixture's server.go. */
const USE_LINE = 24;
/** `func logRequest(...)` — where the definition mock points. */
const DEF_LINE = 19;

const DEFINITION = [location('server.go', DEF_LINE, ['func logRequest(r *http.Request) {'])];
const REFERENCES = [
  location('server.go', DEF_LINE, ['func logRequest(r *http.Request) {'], { character: 5 }),
  location('server.go', 42, ['\t\tlogRequest(r)'], { character: 2 }),
  location('server.go', 51, ['\t\tlogRequest(r)'], { character: 2 }),
];

async function authMiddlewareLine(page: Page) {
  const line = diffLine(await goSection(page), USE_LINE);
  await expect(line).toContainText('func authMiddleware');
  return line;
}

test.describe('LSP — File Mode — whole-file view', () => {
  let calls: LspCalls;

  test.beforeEach(async ({ page, request }) => {
    await clearAllComments(request);
    calls = newCalls();
    await mockLSP(page, calls, { definition: DEFINITION, references: REFERENCES });
  });

  test('code files render as whole-file Pierre items, markdown as a document', async ({ page }) => {
    await loadPage(page);
    const go = await goSection(page);
    await expect(go.locator('code[data-code]')).toHaveCount(1);
    await expect(go.locator('code[data-additions], code[data-deletions], code[data-unified]')).toHaveCount(0);
    await expect((await mdSection(page)).locator('.pierre-document')).toHaveCount(1);
  });

  test('hover over a code line shows the tooltip with the resolved line and column', async ({ page }) => {
    await loadPage(page);
    const line = await authMiddlewareLine(page);
    const p = await hoverForTooltip(page, line, 'authMiddleware');

    await expect(page.locator('.lsp-tooltip')).toContainText('authMiddleware checks for a valid API key');
    expect(calls.hover.length).toBeGreaterThan(0);
    for (const call of calls.hover) expect(call).toEqual({ path: 'server.go', line: String(USE_LINE), char: String(p.char) });
  });

  test('Cmd/Ctrl+Click jumps to the definition inside the file view', async ({ page }) => {
    await loadPage(page);
    const line = await authMiddlewareLine(page);
    const p = await chordClickWord(page, line, 'authMiddleware');

    await expect.poll(() => calls.definition.length).toBe(1);
    expect(calls.definition[0]).toEqual({ path: 'server.go', line: String(USE_LINE), char: String(p.char) });

    // Handled in-review: the target row is selected and flashed. Falling
    // back to the peek popup instead is exactly the regression this guards.
    const target = diffLine(await goSection(page), DEF_LINE);
    await expect.poll(() => jumpFlashed(target)).toBe(true);
    await expect(target).toHaveAttribute('data-selected-line', /.*/);
    await expect(page.locator('.lsp-peek')).toHaveCount(0);
  });

  test('Cmd/Ctrl+Shift+Click opens the references widget under the line; Enter jumps', async ({ page }) => {
    await loadPage(page);
    const item = await goSection(page);
    const line = await authMiddlewareLine(page);
    await chordClickWord(page, line, 'authMiddleware', { shift: true });

    await expect.poll(() => calls.references.length).toBe(1);
    // A Pierre annotation of the file's item, not a floating panel.
    const refs = item.locator('.lsp-refs');
    await expect(refs).toBeVisible();
    await expect(refs).not.toHaveClass(/lsp-refs-floating/);
    await expect(refs.locator('.lsp-refs-item')).toHaveCount(3);
    await expect(refs.locator('.lsp-refs-count-total')).toContainText('3 references');
    await expect(refs.locator('.lsp-refs-list')).toBeFocused();

    await page.keyboard.press('Enter');
    await expect(page.locator('.lsp-refs')).toHaveCount(0);
    await expect(diffLine(item, DEF_LINE)).toBeVisible();
  });
});

// The local-environment note: shown when the server says an answer comes from
// the reviewer's own environment (range/PR focus + Python). It must be a quiet
// footnote — present exactly when the server asks for it, never otherwise.
test.describe('LSP — local environment note', () => {
  let calls: LspCalls;

  test.beforeEach(async ({ page, request }) => {
    await clearAllComments(request);
    calls = newCalls();
    await mockLSP(page, calls, { definition: DEFINITION });
  });

  async function hoverAuthMiddleware(page: Page) {
    await loadPage(page);
    const line = await authMiddlewareLine(page);
    await hoverForTooltip(page, line, 'authMiddleware');
    return { tooltip: page.locator('.lsp-tooltip'), line };
  }

  test('hover shows the note between the docs and the key hint when local_env is set', async ({ page }) => {
    await page.route('**/api/lsp/hover*', route =>
      route.fulfill({ json: { contents: HOVER_MARKDOWN, local_env: true } }));

    const { tooltip } = await hoverAuthMiddleware(page);

    const note = tooltip.locator('.lsp-tooltip-note');
    await expect(note).toHaveCount(1);
    await expect(note).toContainText('local environment');
    const children = await tooltip.evaluate(el =>
      Array.from(el.children).map(c => c.className || c.tagName.toLowerCase()));
    expect(children.indexOf('lsp-tooltip-note')).toBeGreaterThan(-1);
    expect(children.indexOf('lsp-tooltip-note')).toBeLessThan(children.indexOf('lsp-tooltip-hint'));
  });

  test('hover has no note without local_env', async ({ page }) => {
    const { tooltip } = await hoverAuthMiddleware(page);
    await expect(tooltip).toContainText('authMiddleware checks for a valid API key');
    await expect(tooltip.locator('.lsp-tooltip-note')).toHaveCount(0);
  });

  for (const tc of [
    { name: 'a local_env target shows the note above the source', localEnv: true },
    { name: 'an ordinary target shows no note', localEnv: false },
  ]) {
    test(`peek: ${tc.name}`, async ({ page }) => {
      // Not in the review, so the jump falls back to the peek popup.
      await page.route('**/api/lsp/definition*', route => route.fulfill({
        json: {
          locations: [{
            path: '/site-packages/mylib/__init__.py',
            display_path: '$SITE_PACKAGES/mylib/__init__.py',
            line: 1,
            in_session: false,
            in_repo: false,
            peek_start: 1,
            peek: ['def greet(name: str) -> str:', '    return "hi"'],
            ...(tc.localEnv ? { local_env: true } : {}),
          }],
        },
      }));

      await loadPage(page);
      const line = await authMiddlewareLine(page);
      await chordClickWord(page, line, 'authMiddleware');

      const peek = page.locator('.lsp-peek');
      await expect(peek).toBeVisible();
      await expect(peek.locator('.lsp-peek-code').first()).toContainText('def greet');
      const note = peek.locator('.lsp-peek-note');
      if (tc.localEnv) {
        await expect(note).toHaveCount(1);
        await expect(note).toContainText('local copy');
      } else {
        await expect(note).toHaveCount(0);
      }
    });
  }
});
