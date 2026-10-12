import { expect, type Locator, type Page } from '@playwright/test';
import { nextFrames, waitForScrollStable } from './helpers';

// Shared by the LSP specs (git mode and file mode).
//
// /api/lsp/* is mocked so the suites run without a language server and
// assert the frontend wiring — which file, line and UTF-16 column the
// pointer resolved to inside Pierre's shadow DOM, and where a jump lands —
// not what gopls answers.

export type Recorded = { path: string; line: string; char: string };
export type LspCalls = { hover: Recorded[]; definition: Recorded[]; references: Recorded[] };

export function newCalls(): LspCalls {
  return { hover: [], definition: [], references: [] };
}

export const HOVER_MARKDOWN = 'authMiddleware checks for a valid API key.';

/** One location as /api/lsp/definition and /api/lsp/references return it. */
export function location(path: string, line: number, peek: string[], extra: Record<string, unknown> = {}) {
  return { path, display_path: path, line, character: 0, in_session: true, in_repo: true, peek_start: line, peek, ...extra };
}

/**
 * Route /api/config (LSP on, all server-covered extensions) and /api/lsp/*
 * with deterministic answers, recording every request's path/line/char.
 */
export async function mockLSP(page: Page, calls: LspCalls, answers: {
  definition?: unknown[];
  references?: unknown[];
  hover?: Record<string, unknown>;
} = {}) {
  await page.route('**/api/config', async route => {
    const res = await route.fetch();
    const json = await res.json();
    json.lsp_available = true;
    json.lsp_extensions = ['go', 'ts', 'tsx', 'js', 'jsx', 'py', 'tf'];
    await route.fulfill({ response: res, json });
  });

  const record = (url: string): Recorded => {
    const q = new URL(url).searchParams;
    return { path: q.get('path') ?? '', line: q.get('line') ?? '', char: q.get('char') ?? '' };
  };

  await page.route('**/api/lsp/hover*', async route => {
    calls.hover.push(record(route.request().url()));
    await route.fulfill({ json: answers.hover ?? { contents: HOVER_MARKDOWN } });
  });
  await page.route('**/api/lsp/definition*', async route => {
    calls.definition.push(record(route.request().url()));
    await route.fulfill({ json: { locations: answers.definition ?? [] } });
  });
  await page.route('**/api/lsp/references*', async route => {
    calls.references.push(record(route.request().url()));
    await route.fulfill({ json: { locations: answers.references ?? [] } });
  });
}

// Runs in the page: the viewport point over the first character of `word`
// on a Pierre line, and that character's UTF-16 column (what the server
// must receive). Pierre's row text is the source line verbatim.
function measureWord(el: Element, word: string) {
  const idx = el.textContent!.indexOf(word);
  if (idx < 0) return null;
  const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
  let pos = 0;
  let node = walker.nextNode();
  while (node && idx >= pos + node.nodeValue!.length) {
    pos += node.nodeValue!.length;
    node = walker.nextNode();
  }
  if (!node) return null;
  const range = document.createRange();
  range.setStart(node, idx - pos);
  range.setEnd(node, idx - pos + 1);
  const r = range.getBoundingClientRect();
  const box = el.getBoundingClientRect();
  // 40% into the first character: left of its midpoint, so the caret the
  // browser reports is the boundary before it.
  const x = r.left + r.width * 0.4;
  const y = r.top + r.height / 2;
  // The sticky file header can cover a row near the top of the pane.
  const hit = (el.getRootNode() as Document | ShadowRoot).elementFromPoint(x, y);
  const hits = !!hit && (hit === el || el.contains(hit));
  return { x, y, char: idx, relX: x - box.left, relY: y - box.top, hits };
}

export type WordPoint = { x: number; y: number; char: number; relX: number; relY: number; hits: boolean };

/** Whether a Pierre row is running the LSP jump flash (a stylesheet rule, not a class). */
export function jumpFlashed(line: Locator): Promise<boolean> {
  return line.evaluate(el => getComputedStyle(el).animationName === 'lsp-jump-flash');
}

/**
 * Centre the row, wait for it to hold still (rows re-mount while the list
 * settles) and return the point over `word`'s first character.
 */
export async function wordPoint(page: Page, line: Locator, word: string): Promise<WordPoint> {
  let point: WordPoint | null = null;
  await expect(async () => {
    await line.evaluate(el => el.scrollIntoView({ block: 'center' }));
    await waitForScrollStable(page);
    const first = await line.evaluate(measureWord, word);
    await nextFrames(page);
    const second = await line.evaluate(measureWord, word);
    expect(first && second && Math.abs(first.y - second.y) < 1 && second.hits).toBe(true);
    point = second;
  }).toPass({ timeout: 10_000 });
  return point!;
}

/** Hover `word` on a Pierre line (two moves, so a resting pointer still fires mousemove). */
export async function hoverWord(page: Page, line: Locator, word: string): Promise<WordPoint> {
  const p = await wordPoint(page, line, word);
  await page.mouse.move(p.x, p.y + 3);
  await line.hover({ position: { x: p.relX, y: p.relY } });
  return p;
}

/**
 * Hover `word` until the LSP tooltip shows. Pierre can still move the row
 * after the pointer lands (a scroll cancels a pending hover, by design), so
 * the hover is repeated rather than waited on.
 */
export async function hoverForTooltip(page: Page, line: Locator, word: string): Promise<WordPoint> {
  let p: WordPoint | null = null;
  await expect(async () => {
    p = await hoverWord(page, line, word);
    await expect(page.locator('.lsp-tooltip')).toBeVisible({ timeout: 2000 });
  }).toPass({ timeout: 15_000 });
  return p!;
}

/** Cmd/Ctrl(+Shift)+Click `word` on a Pierre line. */
export async function chordClickWord(page: Page, line: Locator, word: string, opts: { shift?: boolean } = {}): Promise<WordPoint> {
  const p = await wordPoint(page, line, word);
  const modifiers: ('ControlOrMeta' | 'Shift')[] = opts.shift ? ['ControlOrMeta', 'Shift'] : ['ControlOrMeta'];
  await line.click({ modifiers, position: { x: p.relX, y: p.relY } });
  return p;
}
