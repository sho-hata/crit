'use strict';

// Tests for the pure helpers in crit-lsp.js (offset math + hunk lookup).
// DOM-dependent behavior (tooltip, peek popup) is exercised in E2E.

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
  // No guessed fallback: an empty/missing list must not offer hovers the
  // server would reject (init only runs when lsp_available is true, which
  // guarantees a non-empty list from the server).
  assert.strictEqual(lsp.makeExtensionMatcher(null).test('a.go'), false);
  assert.strictEqual(lsp.makeExtensionMatcher([]).test('a.ts'), false);
  assert.strictEqual(lsp.makeExtensionMatcher(['t(s', '.*']).test('a.t(s'), false);
  // Regex metacharacters must not survive into the pattern.
  const re = lsp.makeExtensionMatcher(['t(s', '.*', 'js']);
  assert.strictEqual(re.test('a.js'), true);
  assert.strictEqual(re.test('a.tXs'), false);
});

test('hljsLanguageForPath: grammar follows the peeked file, not Go', function () {
  assert.strictEqual(lsp.hljsLanguageForPath('internal/server/main.go'), 'go');
  assert.strictEqual(lsp.hljsLanguageForPath('src/App.tsx'), 'typescript');
  assert.strictEqual(lsp.hljsLanguageForPath('lib/util.MTS'), 'typescript');
  assert.strictEqual(lsp.hljsLanguageForPath('web/app.js'), 'javascript');
  assert.strictEqual(lsp.hljsLanguageForPath('web/mod.cjs'), 'javascript');
  assert.strictEqual(lsp.hljsLanguageForPath('app/main.py'), 'python');
  assert.strictEqual(lsp.hljsLanguageForPath('typeshed/stdlib/json/__init__.pyi'), 'python');
  // No grammar → null → escaped plain text, never the wrong grammar.
  assert.strictEqual(lsp.hljsLanguageForPath('runtime/asm_arm64.s'), null);
  assert.strictEqual(lsp.hljsLanguageForPath('README.md'), null);
  assert.strictEqual(lsp.hljsLanguageForPath(''), null);
  assert.strictEqual(lsp.hljsLanguageForPath(null), null);
});

test('serverErrorText: keeps the server reason, one line, bounded', function () {
  // The reason IS the diagnosis — a language server that will not start says
  // exactly why, and that line is what the reviewer needs in the tooltip.
  const real = 'lsp hover: lsp: initializing typescript-language-server: ' +
    'Could not find a valid TypeScript installation.';
  assert.strictEqual(lsp.serverErrorText(real, 502), real);

  // Only the first line: Go errors can carry a stack-ish tail.
  assert.strictEqual(lsp.serverErrorText('first line\nsecond line', 502), 'first line');

  // An empty body still has to say something actionable.
  assert.match(lsp.serverErrorText('', 502), /HTTP 502/);
  assert.match(lsp.serverErrorText('   \n  ', 503), /HTTP 503/);
  assert.match(lsp.serverErrorText(null, 500), /HTTP 500/);

  // Long bodies are truncated rather than blowing out the tooltip.
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
