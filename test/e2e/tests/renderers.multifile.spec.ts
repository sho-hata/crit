import { test, expect, type APIRequestContext } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { addComment, clearAllComments, loadPage, revealFile, fileHeader, diffLine, roundComplete, sessionCwd } from './helpers';

// Pluggable renderers (issue #989): the built-in mermaid renderer claims
// .mmd / .mermaid files and gives them a Diagram / Source toggle.

const MMD = 'flow.mmd';
const SEQ = 'seq.mmd';

async function commentsOn(request: APIRequestContext, filePath: string) {
  const res = await request.get(`/api/file/comments?path=${encodeURIComponent(filePath)}`);
  return (await res.json()) as { start_line: number; end_line: number; body: string }[];
}

test.describe('Pluggable renderers — Mermaid files', () => {
  test.beforeEach(async ({ page, request }) => {
    await clearAllComments(request);
    await loadPage(page);
  });

  test('a .mmd file opens as a rendered diagram with a Diagram / Source toggle', async ({ page }) => {
    const item = await revealFile(page, MMD);
    const target = item.locator('.crit-render[data-crit-renderer="mermaid"][data-crit-kind="file"]');
    await expect(target).toHaveAttribute('data-crit-state', 'rendered');
    await expect(target.locator('.crit-render-output svg')).toBeVisible();
    await expect(target.locator('.crit-render-source')).toBeHidden();

    const toggle = fileHeader(page, MMD).locator('.file-header-toggle');
    await expect(toggle.locator('.toggle-btn')).toHaveText(['Diagram', 'Source']);
    await expect(toggle.locator('[data-mode="rendered"]')).toHaveAttribute('aria-pressed', 'true');
  });

  test('Source shows the file as code and Diagram switches back', async ({ page }) => {
    const item = await revealFile(page, MMD);
    const toggle = fileHeader(page, MMD).locator('.file-header-toggle');
    await toggle.locator('[data-mode="document"]').click();
    await expect(diffLine(item, 1)).toContainText('flowchart TD');
    await expect(item.locator('.crit-render')).toHaveCount(0);

    await toggle.locator('[data-mode="rendered"]').click();
    await expect(item.locator('.crit-render .crit-render-output svg')).toBeVisible();
  });

  test('clicking a diagram node opens a comment on the line that defines it', async ({ page, request }) => {
    const item = await revealFile(page, MMD);
    const node = item.locator('.crit-render-output g.node', { hasText: 'Authorized?' });
    await expect(node).toBeVisible();
    await node.click();

    const form = item.locator('.comment-form');
    await expect(form).toBeVisible();
    await form.locator('textarea').fill('Which roles count as authorized?');
    await form.locator('.btn-primary').click();

    await expect(item.locator('.comment-card .comment-body')).toContainText('Which roles count as authorized?');
    await expect.poll(async () => (await commentsOn(request, MMD)).map(c => `${c.start_line}-${c.end_line}`))
      .toEqual(['2-2']);
  });

});

test.describe('Pluggable renderers — seeded comments', () => {
  test('line comments show below the diagram', async ({ page, request }) => {
    await clearAllComments(request);
    await addComment(request, MMD, 4, 'Return 403 here');
    await loadPage(page);
    const item = await revealFile(page, MMD);
    const svg = item.locator('.crit-render-output svg');
    const card = item.locator('.comment-card', { hasText: 'Return 403 here' });
    await expect(svg).toBeVisible();
    await expect(card).toBeVisible();
    await expect.poll(async () => {
      const s = await svg.boundingBox();
      const c = await card.boundingBox();
      return !!s && !!c && c.y >= s.y + s.height - 1;
    }, { message: 'comment card sits below the diagram' }).toBe(true);
  });
});

test.describe('Pluggable renderers — quoted comments', () => {
  test('a quote of diagram text is highlighted in the drawn diagram', async ({ page, request }) => {
    await clearAllComments(request);
    const res = await request.post(`/api/file/comments?path=${encodeURIComponent(MMD)}`, {
      data: { start_line: 1, end_line: 4, body: 'Name this step', quote: 'Handle request' },
    });
    expect(res.ok()).toBeTruthy();
    await loadPage(page);
    const item = await revealFile(page, MMD);
    await expect(item.locator('.crit-render-output svg')).toBeVisible();
    // The highlight is on the label in the diagram, not in the hidden source.
    await expect(item.locator('.crit-render-output mark.quote-highlight', { hasText: 'Handle request' })).toBeVisible();
    await expect(item.locator('.crit-render-source mark.quote-highlight')).toHaveCount(0);

    // Back from Source, the diagram comes from the render cache: one highlight, not two.
    const toggle = fileHeader(page, MMD).locator('.file-header-toggle');
    await toggle.locator('[data-mode="document"]').click();
    await expect(item.locator('.crit-render')).toHaveCount(0);
    await toggle.locator('[data-mode="rendered"]').click();
    await expect(item.locator('.crit-render-output mark.quote-highlight', { hasText: 'Handle request' })).toBeVisible();
    await expect(item.locator('.crit-render-output mark.quote-highlight')).toHaveCount(1);
  });

  test('a quote that also appears in the diagram CSS is highlighted on the label', async ({ page, request }) => {
    await clearAllComments(request);
    const res = await request.post(`/api/file/comments?path=${encodeURIComponent(MMD)}`, {
      data: { start_line: 1, end_line: 4, body: 'Say why', quote: 'no' },
    });
    expect(res.ok()).toBeTruthy();
    await loadPage(page);
    const item = await revealFile(page, MMD);
    await expect(item.locator('.crit-render-output mark.quote-highlight', { hasText: /^no$/ })).toBeVisible();
    await expect(item.locator('.crit-render-output style .quote-highlight')).toHaveCount(0);
  });

  test('a quote of an SVG text label keeps the label drawn', async ({ page, request }) => {
    await clearAllComments(request);
    const res = await request.post(`/api/file/comments?path=${encodeURIComponent(SEQ)}`, {
      data: { start_line: 1, end_line: 3, body: 'Which request?', quote: 'Send request' },
    });
    expect(res.ok()).toBeTruthy();
    await loadPage(page);
    const item = await revealFile(page, SEQ);
    const mark = item.locator('.crit-render-output svg text tspan.quote-highlight', { hasText: 'Send request' });
    await expect(mark).toHaveCount(1);
    // An HTML <mark> in SVG <text> is not drawn: the label would have no width.
    const width = await mark.evaluate(el => (el as SVGTextContentElement).getComputedTextLength());
    expect(width).toBeGreaterThan(0);
  });
});

test.describe('Pluggable renderers — theme', () => {
  test('a theme change re-renders the diagram in the new theme', async ({ page, request }) => {
    await clearAllComments(request);
    await loadPage(page);
    const applyTheme = (t: string) => page.evaluate(theme => {
      (window as unknown as { applyTheme: (t: string) => void }).applyTheme(theme);
    }, t);
    try {
      await applyTheme('light');
      const item = await revealFile(page, MMD);
      const target = item.locator('.crit-render');
      await expect(target).toHaveAttribute('data-crit-theme', /^light/);
      await expect(target).toHaveAttribute('data-crit-state', 'rendered');
      await target.locator('.crit-render-output svg').evaluate(svg => { svg.setAttribute('data-e2e-old', ''); });

      await applyTheme('dark');
      await expect(target).toHaveAttribute('data-crit-theme', /^dark/);
      await expect(target).toHaveAttribute('data-crit-state', 'rendered');
      // A fresh SVG, not the light one kept in place.
      await expect(target.locator('.crit-render-output svg')).toBeVisible();
      await expect(target.locator('.crit-render-output svg[data-e2e-old]')).toHaveCount(0);
    } finally {
      await applyTheme('system');
    }
  });
});

test.describe('Pluggable renderers — lazy loading', () => {
  test('a review with a diagram loads mermaid.min.js once, on demand', async ({ page, request }) => {
    await clearAllComments(request);
    const requested: string[] = [];
    page.on('request', req => { if (req.url().includes('mermaid.min.js')) requested.push(req.url()); });
    await loadPage(page);
    const item = await revealFile(page, MMD);
    await expect(item.locator('.crit-render')).toHaveAttribute('data-crit-state', 'rendered');
    expect(requested).toHaveLength(1);
    // The page shell does not list it; the renderer injects it.
    const html = await (await request.get('/')).text();
    expect(html).not.toContain("'mermaid.min.js'");
  });
});

test.describe('Pluggable renderers — render errors', () => {
  test('an invalid diagram shows the source and the error, not a blank block', async ({ page, request }) => {
    await clearAllComments(request);
    const file = path.join(await sessionCwd(request), MMD);
    const original = fs.readFileSync(file, 'utf8');
    fs.writeFileSync(file, 'flowchart TD\n  A --> \n  this is not mermaid ((\n');
    try {
      await roundComplete(request);
      await loadPage(page);
      const item = await revealFile(page, MMD);
      const target = item.locator('.crit-render');
      await expect(target).toHaveAttribute('data-crit-state', 'error');
      await expect(target.locator('.crit-render-error')).toContainText('Could not render mermaid');
      await expect(target.locator('.crit-render-source')).toContainText('this is not mermaid');
      // The rest of the review still renders.
      await expect(diffLine(await revealFile(page, 'main.go'), 1)).toBeVisible();
    } finally {
      fs.writeFileSync(file, original);
      await roundComplete(request);
    }
  });
});
