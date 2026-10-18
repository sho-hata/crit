'use strict';
// rewriteImageSrcs runs after the document sanitizer, so the file path it
// inserts must stay inside the src attribute.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const appJs = fs.readFileSync(path.join(__dirname, '..', 'app.js'), 'utf8');
const sharedSrc = fs.readFileSync(path.join(__dirname, '..', 'crit-shared.js'), 'utf8');

const sandbox = { window: {}, document: { cookie: '' } };
new Function('window', 'document', sharedSrc)(sandbox.window, sandbox.document);
const escapeHtml = sandbox.window.crit.shared.escapeHTML;

function extractFunction(name) {
  const start = appJs.indexOf(`function ${name}(`);
  assert.notEqual(start, -1, `${name} must exist`);
  const bodyStart = appJs.indexOf('{', start);
  let depth = 0;
  for (let i = bodyStart; i < appJs.length; i++) {
    if (appJs[i] === '{') depth++;
    if (appJs[i] === '}') {
      depth--;
      if (depth === 0) return appJs.slice(start, i + 1);
    }
  }
  throw new Error(`could not extract ${name}`);
}

const rewriteImageSrcs = new Function(
  'escapeHtml',
  extractFunction('resolveImagePath') + '\n' + extractFunction('rewriteImageSrcs') + '\nreturn rewriteImageSrcs;'
)(escapeHtml);

test('rewriteImageSrcs: resolves relative images against the file directory', () => {
  assert.equal(
    rewriteImageSrcs('<img src="../img/a.png" alt="a">', 'docs/guide/plan.md'),
    '<img src="/files/docs/img/a.png" alt="a">'
  );
  assert.equal(rewriteImageSrcs('<img src="https://x/a.png">', 'docs/plan.md'), '<img src="https://x/a.png">');
});

test('rewriteImageSrcs: a quote in the directory cannot add attributes', () => {
  const out = rewriteImageSrcs('<img src="missing.png">', 'docs" onerror="window.__critDocXss=1" data-x="/plan.md');
  assert.equal(out, '<img src="/files/docs&quot; onerror=&quot;window.__critDocXss=1&quot; data-x=&quot;/missing.png">');
});

test('rewriteImageSrcs: an already-serialized src is not double-escaped', () => {
  assert.equal(rewriteImageSrcs('<img src="a&amp;b.png">', 'x&y/plan.md'), '<img src="/files/x&amp;y/a&amp;b.png">');
});
