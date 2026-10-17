const test = require('node:test');
const assert = require('node:assert/strict');
const markdownit = require('markdown-it');
const documentHtml = require('../crit-document-html.js');

const md = markdownit({ html: true, linkify: true });

test('hasRawHtml: plain markdown has no raw HTML tokens', () => {
  const tokens = md.parse('# Title\n\nSome `<b>` code and 1 < 2.\n\n```html\n<img onerror=x>\n```\n\n| a | b |\n|---|---|\n| <x | y |\n', {});
  assert.equal(documentHtml.hasRawHtml(tokens), false);
});

test('hasRawHtml: detects block and inline raw HTML', () => {
  assert.equal(documentHtml.hasRawHtml(md.parse('<img src=x onerror="alert(1)">\n', {})), true);
  assert.equal(documentHtml.hasRawHtml(md.parse('Press <kbd>Ctrl</kbd>.\n', {})), true);
  // Inline HTML nested in a table cell lives in the inline token's children.
  assert.equal(documentHtml.hasRawHtml(md.parse('| a |\n|---|\n| <svg onload=x> |\n', {})), true);
});

test('sanitizeBlocks: runs every HTML field through the sanitizer', () => {
  const seen = [];
  const sanitizer = {
    html: (v) => { seen.push(['html', v]); return 'H(' + v + ')'; },
    row: (v) => { seen.push(['row', v]); return 'R(' + v + ')'; },
  };
  const blocks = [
    { html: '<p>a</p>' },
    { html: '', isEmpty: true },
    { html: '<table><tbody><tr><td>x</td></tr></tbody></table>', nativeRowHtml: '<tr><td>x</td></tr>' },
  ];
  documentHtml.sanitizeBlocks(blocks, sanitizer);
  assert.equal(blocks[0].html, 'H(<p>a</p>)');
  assert.equal(blocks[1].html, '');
  assert.equal(blocks[2].html, 'H(<table><tbody><tr><td>x</td></tr></tbody></table>)');
  assert.equal(blocks[2].nativeRowHtml, 'R(<tr><td>x</td></tr>)');
  assert.equal(seen.length, 3);
});

test('create: forbids form controls, embeds and style; rows are sanitized in table context', () => {
  const calls = [];
  const fakePurify = {
    sanitize(value, config) {
      calls.push({ value, config });
      if (config.RETURN_DOM_FRAGMENT) {
        const inner = value.replace(/^<table><tbody>/, '').replace(/<\/tbody><\/table>$/, '');
        return { querySelector: (sel) => (sel === 'tbody' ? { innerHTML: inner } : null) };
      }
      return value;
    },
  };
  const s = documentHtml.create(fakePurify);
  s.html('<p>x</p>');
  assert.equal(s.row('<tr><td>y</td></tr>'), '<tr><td>y</td></tr>');
  assert.equal(calls[1].value, '<table><tbody><tr><td>y</td></tr></tbody></table>');
  for (const tag of ['form', 'input', 'button', 'iframe', 'object', 'embed', 'style', 'base', 'meta', 'link']) {
    assert.ok(calls[0].config.FORBID_TAGS.includes(tag), tag + ' must be forbidden');
  }
  for (const attr of ['formaction', 'srcdoc', 'ping', 'action']) {
    assert.ok(calls[0].config.FORBID_ATTR.includes(attr), attr + ' must be forbidden');
  }
  assert.equal(s.html(''), '');
  assert.equal(s.row(''), '');
});
