'use strict';

const { test } = require('node:test');
const assert = require('node:assert');

const view = require('../crit-view.js');
const lsp = require('../crit-lsp.js');

test('parseViewParams reads path and a positive line', () => {
  assert.deepStrictEqual(view.parseViewParams('?path=internal%2Ffoo.go&line=42'), { path: 'internal/foo.go', line: 42 });
});

test('parseViewParams: missing, zero or junk line means top of file', () => {
  assert.deepStrictEqual(view.parseViewParams('?path=a.go'), { path: 'a.go', line: 0 });
  assert.deepStrictEqual(view.parseViewParams('?path=a.go&line=0'), { path: 'a.go', line: 0 });
  assert.deepStrictEqual(view.parseViewParams('?path=a.go&line=-3'), { path: 'a.go', line: 0 });
  assert.deepStrictEqual(view.parseViewParams('?path=a.go&line=abc'), { path: 'a.go', line: 0 });
  assert.deepStrictEqual(view.parseViewParams(''), { path: '', line: 0 });
});

test('parseViewParams round-trips crit-lsp.viewURL', () => {
  const url = lsp.viewURL('web/a b/x.ts', 7);
  assert.strictEqual(url, '/view?path=web%2Fa%20b%2Fx.ts&line=7');
  assert.deepStrictEqual(view.parseViewParams(url.slice(url.indexOf('?'))), { path: 'web/a b/x.ts', line: 7 });
  assert.strictEqual(lsp.viewURL('x.go', 0), '/view?path=x.go');
});

test('lineCount ignores one trailing newline, like the review list', () => {
  assert.strictEqual(view.lineCount(''), 0);
  assert.strictEqual(view.lineCount('a'), 1);
  assert.strictEqual(view.lineCount('a\n'), 1);
  assert.strictEqual(view.lineCount('a\nb\n'), 2);
  assert.strictEqual(view.lineCount('a\n\n'), 2);
});
