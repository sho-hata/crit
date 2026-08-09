'use strict';

// Tests for the pure helpers in crit-lsp.js (offset math).
// DOM-dependent behavior (the hover tooltip) is exercised in E2E.

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
