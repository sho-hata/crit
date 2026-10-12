import { test, expect } from '@playwright/test';
import { clearAllComments, loadPage, goSection, revealFile, fileItem, diffLine, setDiffStyle, showLine } from './helpers';
import { mockLSP, newCalls, location, hoverWord, hoverForTooltip, chordClickWord, jumpFlashed, type LspCalls } from './lsp-helpers';

// LSP wiring on Pierre diffs in git mode: split and unified layouts, the
// old side, in-review jumps (including into a collapsed gap between hunks)
// and the references widget. /api/lsp/* is mocked (see lsp-helpers).

/** `logRequest(r)` inside main() — a rendered new-side line of server.go. */
const USE_LINE = 42;
/** `func logRequest(...)` in server.go. */
const DEF_LINE = 19;
/** The replaced `http.HandleFunc("/", func(...` on the old side of server.go. */
const OLD_LINE = 21;
/** `func handleSearch` in routes.go: inside the >20-line gap between its two hunks. */
const GAP_LINE = 34;

const REFERENCES = [
  location('server.go', DEF_LINE, ['func logRequest(r *http.Request) {'], { character: 5 }),
  location('server.go', USE_LINE, ['\t\tlogRequest(r)'], { character: 2 }),
  location('server.go', 51, ['\t\tlogRequest(r)'], { character: 2 }),
];

test.describe('LSP — Git Mode — Pierre diff', () => {
  let calls: LspCalls;

  test.beforeEach(async ({ request }) => {
    await clearAllComments(request);
    calls = newCalls();
  });

  // Pierre virtualizes rows inside a file too: scroll until the row exists.
  async function useLine(page: Parameters<typeof goSection>[0]) {
    const line = diffLine(await goSection(page), USE_LINE);
    await showLine(page, line);
    await expect(line).toContainText('logRequest(r)');
    return line;
  }

  test('hover over a new-side line sends its line and column and shows the tooltip', async ({ page }) => {
    await mockLSP(page, calls);
    await loadPage(page);
    const p = await hoverForTooltip(page, await useLine(page), 'logRequest');

    await expect(page.locator('.lsp-tooltip')).toContainText('authMiddleware checks for a valid API key');
    expect(calls.hover.length).toBeGreaterThan(0);
    for (const call of calls.hover) expect(call).toEqual({ path: 'server.go', line: String(USE_LINE), char: String(p.char) });
  });

  test('hover over the old side sends nothing', async ({ page }) => {
    await mockLSP(page, calls);
    await loadPage(page);
    const item = await goSection(page);
    const old = diffLine(item, OLD_LINE, 'old');
    await expect(old).toContainText('http.HandleFunc');
    await hoverWord(page, old, 'HandleFunc');
    // A later new-side hover is the positive signal that the old one, which
    // would have fired first, produced no request.
    await hoverForTooltip(page, await useLine(page), 'logRequest');
    expect(calls.hover.length).toBeGreaterThan(0);
    for (const call of calls.hover) expect(call.line).toBe(String(USE_LINE));
  });

  test('unified diff: a context line maps to its new line number, a deletion to nothing', async ({ page }) => {
    await mockLSP(page, calls);
    await loadPage(page);
    await setDiffStyle(page, 'unified');
    const item = await goSection(page);
    const old = diffLine(item, OLD_LINE, 'old');
    await expect(old).toContainText('http.HandleFunc');
    await hoverWord(page, old, 'HandleFunc');
    const p = await hoverForTooltip(page, await useLine(page), 'logRequest');
    expect(calls.hover.length).toBeGreaterThan(0);
    for (const call of calls.hover) expect(call).toEqual({ path: 'server.go', line: String(USE_LINE), char: String(p.char) });
  });

  test('Cmd/Ctrl+Click jumps to a definition in another session file', async ({ page }) => {
    // utils.go is staged, so it is in the session; `func Reverse` (line 13)
    // is inside its only hunk.
    await mockLSP(page, calls, { definition: [location('utils.go', 13, ['func Reverse(s string) string {'])] });
    await loadPage(page);
    const p = await chordClickWord(page, await useLine(page), 'logRequest');
    await expect.poll(() => calls.definition.length).toBe(1);
    expect(calls.definition[0]).toEqual({ path: 'server.go', line: String(USE_LINE), char: String(p.char) });

    // The jump itself brings utils.go in; revealing it here would race it.
    const target = diffLine(fileItem(page, 'utils.go'), 13);
    await expect(target).toContainText('func Reverse');
    await expect.poll(() => jumpFlashed(target)).toBe(true);
    await expect(target).toHaveAttribute('data-selected-line', /.*/);
    await expect(page.locator('.lsp-peek')).toHaveCount(0);
  });

  test('a definition before the first hunk falls back to the peek popup', async ({ page }) => {
    // `func Capitalize` (utils.go:5) precedes the hunk, which starts at line
    // 8; leading and trailing gaps are not expanded.
    await mockLSP(page, calls, { definition: [location('utils.go', 5, ['func Capitalize(s string) string {'])] });
    await loadPage(page);
    await chordClickWord(page, await useLine(page), 'logRequest');
    await expect.poll(() => calls.definition.length).toBe(1);

    const peek = page.locator('.lsp-peek');
    await expect(peek).toBeVisible();
    await expect(peek.locator('.lsp-peek-title')).toHaveText('utils.go:5');
    await expect(peek.locator('.lsp-peek-code').first()).toContainText('func Capitalize');
  });

  test('a definition inside a collapsed gap expands the hunks and lands on the line', async ({ page }) => {
    await mockLSP(page, calls, { definition: [location('routes.go', GAP_LINE, ['func handleSearch(w http.ResponseWriter, r *http.Request) {'])] });
    await loadPage(page);
    // Precondition: the gap is collapsed, so the line is not rendered.
    const routes = await revealFile(page, 'routes.go');
    await expect(diffLine(routes, GAP_LINE)).toHaveCount(0);

    await chordClickWord(page, await useLine(page), 'logRequest');
    await expect.poll(() => calls.definition.length).toBe(1);

    const target = diffLine(fileItem(page, 'routes.go'), GAP_LINE);
    await expect(target).toHaveCount(1);
    await expect(target).toContainText('func handleSearch');
    await expect(target).toBeInViewport();
    await expect.poll(() => jumpFlashed(target)).toBe(true);
    await expect(target).toHaveAttribute('data-selected-line', /.*/);
    await expect(page.locator('.lsp-peek')).toHaveCount(0);
  });

  test('Cmd/Ctrl+Shift+Click lists references under the clicked line; Enter jumps to one', async ({ page }) => {
    await mockLSP(page, calls, { references: REFERENCES });
    await loadPage(page);
    const item = await goSection(page);
    const p = await chordClickWord(page, await useLine(page), 'logRequest', { shift: true });
    await expect.poll(() => calls.references.length).toBe(1);
    expect(calls.references[0]).toEqual({ path: 'server.go', line: String(USE_LINE), char: String(p.char) });

    // Mounted as a Pierre annotation of server.go's item, right under the line.
    const refs = item.locator('.lsp-refs');
    await expect(refs).toBeVisible();
    await expect(refs).not.toHaveClass(/lsp-refs-floating/);
    await expect(refs.locator('.lsp-refs-symbol')).toHaveText('logRequest');
    await expect(refs.locator('.lsp-refs-item')).toHaveCount(3);
    await expect(refs.locator('.lsp-refs-match').first()).toHaveText('logRequest');
    const below = await refs.evaluate((el, line) => {
      const host = el.closest('diffs-container')!;
      const row = host.shadowRoot!.querySelector(`code[data-additions] [data-content] > [data-line="${line}"]`)!;
      return el.getBoundingClientRect().top >= row.getBoundingClientRect().bottom - 1;
    }, USE_LINE);
    expect(below).toBe(true);

    await page.keyboard.press('Enter');
    await expect(page.locator('.lsp-refs')).toHaveCount(0);
    const target = diffLine(item, DEF_LINE);
    await expect(target).toContainText('func logRequest');
    await expect(target).toBeVisible();
  });
});
