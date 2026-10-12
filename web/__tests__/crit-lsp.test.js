'use strict';

// Pure helpers only; DOM-dependent behavior (tooltip, peek, references) is
// exercised in E2E.

const { test } = require('node:test');
const assert = require('node:assert');

const lsp = require('../crit-lsp.js');

// ===== fake DOM nodes (duck-typed: nodeType / childNodes / textContent) =====

function textNode(t) {
  return { nodeType: 3, textContent: t };
}

function el(...kids) {
  return {
    nodeType: 1,
    childNodes: kids,
    get textContent() {
      return kids.map(function (k) { return k.textContent; }).join('');
    },
  };
}

test('textOffsetIn: offset within a bare text node', function () {
  const t = textNode('func main() {');
  const root = el(t);
  assert.strictEqual(lsp.textOffsetIn(root, t, 5), 5);
});

test('textOffsetIn: accumulates across highlight spans', function () {
  // <root>"func "<span>"main"</span>"() {"</root> — like hljs output
  const t1 = textNode('func ');
  const t2 = textNode('main');
  const span = el(t2);
  const t3 = textNode('() {');
  const root = el(t1, span, t3);
  assert.strictEqual(lsp.textOffsetIn(root, t2, 2), 7); // "func " + "ma"
  assert.strictEqual(lsp.textOffsetIn(root, t3, 0), 9); // "func main"
});

test('textOffsetIn: element target counts child index', function () {
  const span1 = el(textNode('abc'));
  const span2 = el(textNode('de'));
  const root = el(span1, span2);
  // caretPositionFromPoint may return (root, childIndex)
  assert.strictEqual(lsp.textOffsetIn(root, root, 1), 3); // after span1
  assert.strictEqual(lsp.textOffsetIn(root, root, 2), 5);
});

test('textOffsetIn: target outside root returns -1', function () {
  const root = el(textNode('abc'));
  const stranger = textNode('zzz');
  assert.strictEqual(lsp.textOffsetIn(root, stranger, 0), -1);
});

test('textOffsetIn: UTF-16 semantics (surrogate pairs count as 2)', function () {
  // '\u{1F600}' (😀) is 2 UTF-16 code units — .length gives 2.
  const t1 = textNode('a\u{1F600}b');
  const t2 = textNode('cd');
  const root = el(t1, t2);
  assert.strictEqual(lsp.textOffsetIn(root, t2, 1), 5); // 1+2+1 + 1
});

// ===== hunk lookups =====

const HUNKS = [
  { NewStart: 10, NewCount: 5 },  // lines 10-14
  { NewStart: 30, NewCount: 10 }, // lines 30-39
  { NewStart: 40, NewCount: 3 },  // lines 40-42 (contiguous with previous)
];

test('findHunkForLine: hits and misses', function () {
  assert.strictEqual(lsp.findHunkForLine(HUNKS, 10), 0);
  assert.strictEqual(lsp.findHunkForLine(HUNKS, 14), 0);
  assert.strictEqual(lsp.findHunkForLine(HUNKS, 15), -1); // gap
  assert.strictEqual(lsp.findHunkForLine(HUNKS, 39), 1);
  assert.strictEqual(lsp.findHunkForLine(HUNKS, 42), 2);
  assert.strictEqual(lsp.findHunkForLine(HUNKS, 43), -1); // trailing
  assert.strictEqual(lsp.findHunkForLine(HUNKS, 5), -1);  // leading
  assert.strictEqual(lsp.findHunkForLine([], 1), -1);
  assert.strictEqual(lsp.findHunkForLine(null, 1), -1);
});

// ===== references helpers =====

test('groupLocationsByFile: groups in order, keeps flat indices', function () {
  const locs = [
    { display_path: 'a.go', line: 3 },
    { display_path: 'a.go', line: 9 },
    { display_path: 'b.go', line: 1 },
  ];
  const groups = lsp.groupLocationsByFile(locs);
  assert.strictEqual(groups.length, 2);
  assert.strictEqual(groups[0].display_path, 'a.go');
  assert.deepStrictEqual(groups[0].items.map(function (it) { return it.idx; }), [0, 1]);
  assert.strictEqual(groups[1].display_path, 'b.go');
  assert.strictEqual(groups[1].items[0].loc, locs[2]);
  assert.deepStrictEqual(lsp.groupLocationsByFile([]), []);
  assert.deepStrictEqual(lsp.groupLocationsByFile(null), []);
});

test('refSnippet: extracts the reference line from its peek window', function () {
  const loc = { line: 12, peek_start: 10, peek: ['a', 'b', 'target', 'd'] };
  assert.strictEqual(lsp.refSnippet(loc), 'target');
});

test('refSnippet: empty when no peek or line outside window', function () {
  assert.strictEqual(lsp.refSnippet({ line: 5 }), '');
  assert.strictEqual(lsp.refSnippet({ line: 5, peek_start: 10, peek: ['x'] }), '');
  assert.strictEqual(lsp.refSnippet({ line: 11, peek_start: 10, peek: ['x'] }), '');
});

test('findGapForLine: inner gap only', function () {
  assert.deepStrictEqual(lsp.findGapForLine(HUNKS, 15), { prevIdx: 0, nextIdx: 1 });
  assert.deepStrictEqual(lsp.findGapForLine(HUNKS, 29), { prevIdx: 0, nextIdx: 1 });
  assert.strictEqual(lsp.findGapForLine(HUNKS, 10), null);  // inside hunk
  assert.strictEqual(lsp.findGapForLine(HUNKS, 5), null);   // leading gap
  assert.strictEqual(lsp.findGapForLine(HUNKS, 100), null); // trailing gap
  // Contiguous hunks (40 directly follows 30..39): no gap between them.
  assert.strictEqual(lsp.findGapForLine(HUNKS, 40), null);
  assert.strictEqual(lsp.findGapForLine([], 1), null);
});

test('makeExtensionMatcher: matches server-provided extensions, case-insensitive', function () {
  const re = lsp.makeExtensionMatcher(['go', 'ts', 'tsx']);
  assert.strictEqual(re.test('internal/server/main.go'), true);
  assert.strictEqual(re.test('web/src/App.tsx'), true);
  assert.strictEqual(re.test('web/src/UTIL.TS'), true);
  assert.strictEqual(re.test('README.md'), false);
  assert.strictEqual(re.test('main.go.orig'), false);
  assert.strictEqual(re.test('script.js'), false); // not in the list
});

test('makeExtensionMatcher: empty list matches nothing, unsafe entries dropped', function () {
  // No guessed fallback: it would offer hovers the server rejects.
  assert.strictEqual(lsp.makeExtensionMatcher(null).test('a.go'), false);
  assert.strictEqual(lsp.makeExtensionMatcher([]).test('a.ts'), false);
  assert.strictEqual(lsp.makeExtensionMatcher(['t(s', '.*']).test('a.t(s'), false);
  // Regex metacharacters must not survive into the pattern.
  const re = lsp.makeExtensionMatcher(['t(s', '.*', 'js']);
  assert.strictEqual(re.test('a.js'), true);
  assert.strictEqual(re.test('a.tXs'), false);
});

// ===== Pierre line lookup (composedPath, innermost first) =====

function pierrePath(opts) {
  const line = { tagName: 'DIV', dataset: Object.assign({ line: String(opts.line) }, opts.lineType ? { lineType: opts.lineType } : {}) };
  const span = { tagName: 'SPAN', dataset: {} };
  const content = { tagName: 'DIV', dataset: { content: '' } };
  const code = { tagName: 'CODE', dataset: Object.assign({ code: '' }, opts.column ? { [opts.column]: '' } : {}) };
  const pre = { tagName: 'PRE', dataset: { diff: '' } };
  const root = { nodeType: 11 }; // ShadowRoot: no dataset
  const host = { tagName: 'DIFFS-CONTAINER', dataset: { critPath: opts.path } };
  const body = { tagName: 'BODY', dataset: {} };
  return { path: [span, line, content, code, pre, root, host, body, { nodeType: 9 }, {}], line };
}

test('lineHitFromPath: split additions column → new-side line of the host file', function () {
  const p = pierrePath({ line: 6, lineType: 'change-addition', column: 'additions', path: 'main.go' });
  const hit = lsp.lineHitFromPath(p.path);
  assert.strictEqual(hit.path, 'main.go');
  assert.strictEqual(hit.line, 6);
  assert.strictEqual(hit.lineEl, p.line);
});

test('lineHitFromPath: deletions column never maps to the file', function () {
  const p = pierrePath({ line: 6, lineType: 'change-deletion', column: 'deletions', path: 'main.go' });
  assert.strictEqual(lsp.lineHitFromPath(p.path), null);
  // Context rows of the old column carry the old numbering too.
  const ctx = pierrePath({ line: 4, lineType: 'context', column: 'deletions', path: 'main.go' });
  assert.strictEqual(lsp.lineHitFromPath(ctx.path), null);
});

test('lineHitFromPath: unified column keeps context and additions, drops deletions', function () {
  const add = pierrePath({ line: 6, lineType: 'change-addition', column: 'unified', path: 'a.ts' });
  assert.strictEqual(lsp.lineHitFromPath(add.path).line, 6);
  const ctx = pierrePath({ line: 4, lineType: 'context', column: 'unified', path: 'a.ts' });
  assert.strictEqual(lsp.lineHitFromPath(ctx.path).line, 4);
  const del = pierrePath({ line: 6, lineType: 'change-deletion', column: 'unified', path: 'a.ts' });
  assert.strictEqual(lsp.lineHitFromPath(del.path), null);
});

test('lineHitFromPath: files-mode whole-file column (no side)', function () {
  const p = pierrePath({ line: 22, lineType: 'context', column: null, path: 'app.py' });
  assert.deepStrictEqual(lsp.lineHitFromPath(p.path), { lineEl: p.line, path: 'app.py', line: 22 });
});

test('lineHitFromPath: gutter cells, light-DOM elements and empty paths miss', function () {
  // The gutter uses data-column-number, not data-line.
  const gutter = { tagName: 'DIV', dataset: { columnNumber: '6', lineType: 'context' } };
  const code = { tagName: 'CODE', dataset: { additions: '' } };
  const host = { tagName: 'DIFFS-CONTAINER', dataset: { critPath: 'main.go' } };
  assert.strictEqual(lsp.lineHitFromPath([gutter, code, host]), null);
  // The peek popup's own rows carry data-line but sit in no Pierre column.
  const peekLine = { tagName: 'DIV', dataset: { line: '12' } };
  assert.strictEqual(lsp.lineHitFromPath([peekLine, { tagName: 'DIV', dataset: {} }, { tagName: 'BODY', dataset: {} }]), null);
  // A line without a host (detached render) is not attributable to a file.
  assert.strictEqual(lsp.lineHitFromPath([peekLine, code]), null);
  assert.strictEqual(lsp.lineHitFromPath([]), null);
  assert.strictEqual(lsp.lineHitFromPath(null), null);
});

test('serverErrorText: keeps the server reason, one line, bounded', function () {
  const real = 'lsp hover: lsp: initializing typescript-language-server: ' +
    'Could not find a valid TypeScript installation.';
  assert.strictEqual(lsp.serverErrorText(real, 502), real);

  // Go errors can carry a stack-ish tail.
  assert.strictEqual(lsp.serverErrorText('first line\nsecond line', 502), 'first line');

  assert.match(lsp.serverErrorText('', 502), /HTTP 502/);
  assert.match(lsp.serverErrorText('   \n  ', 503), /HTTP 503/);
  assert.match(lsp.serverErrorText(null, 500), /HTTP 500/);

  const long = lsp.serverErrorText('x'.repeat(500), 502);
  assert.ok(long.length <= 200, 'length = ' + long.length);
  assert.ok(long.endsWith('…'));
});

// ===== local-environment note (range/PR focus, Python) =====

function escapeHTML(s) {
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

test('composeTooltip: the note sits between the docs and the key hint', function () {
  const html = lsp.composeTooltip('<p>docs</p>', 'local env', 'click hint', escapeHTML);
  const docs = html.indexOf('<p>docs</p>');
  const note = html.indexOf('class="lsp-tooltip-note">local env<');
  const hint = html.indexOf('class="lsp-tooltip-hint">click hint<');
  assert.ok(docs !== -1 && note !== -1 && hint !== -1, html);
  assert.ok(docs < note && note < hint, 'order must be docs, note, hint: ' + html);
});

test('composeTooltip: without a note there is no note element at all', function () {
  for (const note of ['', undefined, null]) {
    const html = lsp.composeTooltip('<p>docs</p>', note, 'click hint', escapeHTML);
    assert.ok(html.indexOf('lsp-tooltip-note') === -1, 'note ' + note + ' rendered: ' + html);
    assert.ok(html.indexOf('class="lsp-tooltip-hint">click hint<') !== -1, html);
  }
});

test('composeTooltip: escapes the note and the hint', function () {
  const html = lsp.composeTooltip('', '<b>x</b>', 'a & b', escapeHTML);
  assert.ok(html.indexOf('<b>x</b>') === -1, 'unescaped markup in note: ' + html);
  assert.ok(html.indexOf('&lt;b&gt;x&lt;/b&gt;') !== -1, html);
  assert.ok(html.indexOf('a &amp; b') !== -1, html);
});

test('peekNoteHTML: only a local_env target with note text gets a strip', function () {
  const cases = [
    { name: 'local_env target', loc: { local_env: true }, text: 'note', want: true },
    { name: 'ordinary target', loc: { local_env: false }, text: 'note', want: false },
    { name: 'flag absent', loc: {}, text: 'note', want: false },
    { name: 'no text', loc: { local_env: true }, text: '', want: false },
    { name: 'no location', loc: null, text: 'note', want: false },
  ];
  for (const tc of cases) {
    const html = lsp.peekNoteHTML(tc.loc, tc.text, escapeHTML);
    assert.strictEqual(html !== '', tc.want, tc.name + ': ' + JSON.stringify(html));
    if (tc.want) assert.ok(html.indexOf('class="lsp-peek-note">note<') !== -1, html);
  }
});

test('wordAt: identifier under, or ending at, the caret', function () {
  assert.strictEqual(lsp.wordAt('const unmapIpv4 = (a)', 8), 'unmapIpv4');
  assert.strictEqual(lsp.wordAt('const unmapIpv4 = (a)', 15), 'unmapIpv4');
  assert.strictEqual(lsp.wordAt('x = $el.név', 5), '$el');
  assert.strictEqual(lsp.wordAt('x = $el.név', 9), 'név');
});

test('wordAt: empty outside identifiers or out of range', function () {
  assert.strictEqual(lsp.wordAt('a = (b)', 3), '');
  assert.strictEqual(lsp.wordAt('abc', 9), '');
  assert.strictEqual(lsp.wordAt('', 0), '');
});

test('symbolRange: range at the reported column when the text matches', function () {
  assert.deepStrictEqual(lsp.symbolRange('  address = unmapIpv4(v)', 12, 'unmapIpv4'), { start: 12, end: 21 });
});

test('symbolRange: null on mismatch (alias, stale column) or missing input', function () {
  assert.strictEqual(lsp.symbolRange('import { a as b } from "x"', 14, 'a'), null);
  assert.strictEqual(lsp.symbolRange('foo()', undefined, 'foo'), null);
  assert.strictEqual(lsp.symbolRange('foo()', 0, ''), null);
});
