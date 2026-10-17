// Sanitizes rendered markdown documents (Document view, rendered diff).
// Dependencies: window.DOMPurify (dompurify.min.js must load first).
//
// Reviewed markdown is repo-controlled and renders on the Crit origin, which
// is the API's origin: raw HTML that runs script there could POST to /api/*
// (grant project-hook trust, finish the review). Documents keep GitHub-like
// raw HTML; anything that executes, embeds another document or submits is
// dropped. DOMPurify's defaults already strip <script>, on* handlers and
// javascript: URLs.
(function () {
  'use strict';

  var FORBID_TAGS = [
    'style', 'form', 'input', 'button', 'textarea', 'select', 'option',
    'iframe', 'frame', 'frameset', 'object', 'embed', 'portal', 'base',
    'link', 'meta', 'noscript', 'template', 'dialog'
  ];
  var FORBID_ATTR = ['action', 'formaction', 'ping', 'srcdoc'];

  // Documents without raw HTML tokens contain only markup markdown-it
  // generated itself (text escaped, links validated), so they skip the
  // sanitizer — 10,000+ line plans pay nothing.
  function hasRawHtml(tokens) {
    for (var i = 0; i < tokens.length; i++) {
      var t = tokens[i];
      if (t.type === 'html_block' || t.type === 'html_inline') return true;
      if (t.children && hasRawHtml(t.children)) return true;
    }
    return false;
  }

  // purify: a DOMPurify instance. Returns { html, row } sanitizers.
  function create(purify) {
    var config = { FORBID_TAGS: FORBID_TAGS, FORBID_ATTR: FORBID_ATTR };
    var fragmentConfig = { FORBID_TAGS: FORBID_TAGS, FORBID_ATTR: FORBID_ATTR, RETURN_DOM_FRAGMENT: true };

    function html(value) {
      return value ? purify.sanitize(value, config) : value;
    }

    // A bare <tr> is dropped by the HTML parser outside table context, so
    // rows are sanitized inside a table and unwrapped again.
    function row(value) {
      if (!value) return value;
      var frag = purify.sanitize('<table><tbody>' + value + '</tbody></table>', fragmentConfig);
      var tbody = frag.querySelector('tbody');
      return tbody ? tbody.innerHTML : '';
    }

    return { html: html, row: row };
  }

  // Sanitizes every HTML field of the line blocks in place.
  function sanitizeBlocks(blocks, sanitizer) {
    for (var i = 0; i < blocks.length; i++) {
      var b = blocks[i];
      if (b.html) b.html = sanitizer.html(b.html);
      if (b.nativeRowHtml) b.nativeRowHtml = sanitizer.row(b.nativeRowHtml);
    }
    return blocks;
  }

  var api = { hasRawHtml: hasRawHtml, create: create, sanitizeBlocks: sanitizeBlocks, FORBID_TAGS: FORBID_TAGS };
  if (typeof window !== 'undefined') {
    window.crit = window.crit || {};
    // Own instance: comment-html.js's hook strips classes documents rely on.
    if (window.DOMPurify) api.sanitizer = create(window.DOMPurify(window));
    window.crit.documentHtml = api;
  }
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  }
})();
