// crit-lsp.js — LSP hover + go-to-definition for code-review mode.
//
// Talks to the local Go server's /api/lsp/* endpoints (which proxy gopls).
// Hover: rest the mouse over Go code in a diff → documentation tooltip.
// Definition: Cmd/Ctrl+Click → jump within the review, or a peek popup when
// the target lives outside the visible diff / session / repo.
//
// Dependencies (window.crit.* namespaces read):
//   - crit.shared (escapeHTML)
// UI adapters are injected via init(opts) so pure logic stays testable:
//   - opts.renderMarkdown(text) -> safe HTML for hover markdown
//   - opts.jumpToLocation(loc)  -> Promise<boolean>; true when the app
//     revealed the location inside the review UI
//   - opts.toast(message)       -> transient error/info notice
(function () {
  'use strict';

  // ===== Pure helpers (exported for Node tests) =====

  // textOffsetIn returns the UTF-16 offset of (target, offsetInNode) within
  // root's textContent, or -1 when target is not inside root. Duck-typed
  // (nodeType/childNodes/textContent) so tests can pass fake nodes.
  // For text nodes (nodeType 3) offsetInNode is a character offset; for
  // elements it is a child index (caretPositionFromPoint can return either).
  function textOffsetIn(root, target, offsetInNode) {
    var total = 0;
    var found = false;
    function textLen(node) {
      return (node.textContent || '').length;
    }
    function walk(node) {
      if (found) return;
      if (node === target) {
        if (node.nodeType === 3) {
          total += offsetInNode;
        } else {
          var kids = node.childNodes || [];
          for (var i = 0; i < offsetInNode && i < kids.length; i++) total += textLen(kids[i]);
        }
        found = true;
        return;
      }
      if (node.nodeType === 3) {
        total += textLen(node);
        return;
      }
      var children = node.childNodes || [];
      for (var j = 0; j < children.length; j++) {
        walk(children[j]);
        if (found) return;
      }
    }
    walk(root);
    return found ? total : -1;
  }

  // findHunkForLine returns the index of the hunk whose new-side range
  // contains line (1-based), or -1.
  function findHunkForLine(hunks, line) {
    for (var i = 0; i < (hunks || []).length; i++) {
      var h = hunks[i];
      if (line >= h.NewStart && line < h.NewStart + h.NewCount) return i;
    }
    return -1;
  }

  // findGapForLine returns {prevIdx, nextIdx} when line (1-based, new side)
  // falls in the collapsed gap between two adjacent hunks, or null. Leading
  // and trailing gaps are not covered — callers fall back to the peek popup.
  function findGapForLine(hunks, line) {
    for (var i = 1; i < (hunks || []).length; i++) {
      var prevEnd = hunks[i - 1].NewStart + hunks[i - 1].NewCount;
      if (line >= prevEnd && line < hunks[i].NewStart) {
        return { prevIdx: i - 1, nextIdx: i };
      }
    }
    return null;
  }

  // ===== Controller =====

  var HOVER_DELAY_MS = 350;
  var MAX_CONSECUTIVE_FAILURES = 3;

  var st = null; // controller state; null until init()

  function esc(s) {
    return window.crit.shared.escapeHTML(s);
  }

  // eligibleLineEl walks up from an event target to the enclosing new-side
  // .go diff line, returning {lineEl, contentEl} or null. Unified view rows
  // are .diff-line; split view sides are .diff-split-side — both carry the
  // same data-diff-* attributes via tagDiffLine.
  function eligibleLineEl(target) {
    if (!target || !target.closest) return null;
    var contentEl = target.closest('.diff-content');
    if (!contentEl) return null;
    var lineEl = contentEl.closest('.diff-line, .diff-split-side');
    if (!lineEl) return null;
    var path = lineEl.dataset.diffFilePath;
    if (!path || !/\.go$/.test(path)) return null;
    if (lineEl.dataset.diffSide === 'old') return null;
    var line = parseInt(lineEl.dataset.diffLineNum, 10);
    if (!line) return null;
    return { lineEl: lineEl, contentEl: contentEl, path: path, line: line };
  }

  // caretCharOffset computes the UTF-16 column under the pointer, or -1.
  function caretCharOffset(contentEl, x, y) {
    var node = null;
    var offset = 0;
    if (document.caretPositionFromPoint) {
      var pos = document.caretPositionFromPoint(x, y);
      if (pos) {
        node = pos.offsetNode;
        offset = pos.offset;
      }
    } else if (document.caretRangeFromPoint) {
      var range = document.caretRangeFromPoint(x, y);
      if (range) {
        node = range.startContainer;
        offset = range.startOffset;
      }
    }
    if (!node) return -1;
    return textOffsetIn(contentEl, node, offset);
  }

  function recordFailure() {
    st.failures++;
    if (st.failures >= MAX_CONSECUTIVE_FAILURES) {
      st.disabled = true;
      hideTooltip();
    }
  }

  function recordSuccess() {
    st.failures = 0;
  }

  // ===== Hover tooltip =====

  function ensureTooltip() {
    if (st.tooltip) return st.tooltip;
    var el = document.createElement('div');
    el.className = 'lsp-tooltip';
    el.setAttribute('role', 'tooltip');
    el.hidden = true;
    document.body.appendChild(el);
    st.tooltip = el;
    return el;
  }

  function hideTooltip() {
    if (st && st.tooltip) st.tooltip.hidden = true;
    if (st) st.hoverKey = null;
    abortInflight();
  }

  function abortInflight() {
    if (st.inflight) {
      st.inflight.abort();
      st.inflight = null;
    }
  }

  function showTooltip(html, x, y) {
    var tip = ensureTooltip();
    tip.innerHTML = html + '<div class="lsp-tooltip-hint">' + esc(st.defHintText) + '</div>';
    tip.hidden = false;
    // Position after layout so we can clamp to the viewport: prefer above
    // the cursor, fall back to below.
    tip.style.left = '0px';
    tip.style.top = '0px';
    var rect = tip.getBoundingClientRect();
    var left = Math.min(x, window.innerWidth - rect.width - 8);
    if (left < 8) left = 8;
    var top = y - rect.height - 12;
    if (top < 8) top = y + 20;
    tip.style.left = left + 'px';
    tip.style.top = top + 'px';
  }

  function onMouseMove(e) {
    if (st.disabled) return;
    var hit = eligibleLineEl(e.target);
    if (!hit) {
      // Moving onto the tooltip itself keeps it open (lets users select text).
      if (st.tooltip && !st.tooltip.hidden && st.tooltip.contains(e.target)) return;
      clearTimeout(st.hoverTimer);
      hideTooltip();
      return;
    }
    clearTimeout(st.hoverTimer);
    st.hoverTimer = setTimeout(function () {
      requestHover(hit, e.clientX, e.clientY);
    }, HOVER_DELAY_MS);
  }

  function requestHover(hit, x, y) {
    var char = caretCharOffset(hit.contentEl, x, y);
    if (char < 0) return;
    var key = hit.path + ':' + hit.line + ':' + char;
    if (st.hoverKey === key && st.tooltip && !st.tooltip.hidden) return;
    abortInflight();
    var ctl = new AbortController();
    st.inflight = ctl;
    // Slow-response indicator: the first request after gopls spawns can take
    // seconds (workspace load). Show a placeholder so the wait is visible.
    var loadingTimer = setTimeout(function () {
      showTooltip('<div class="lsp-tooltip-loading">' + esc(st.loadingText) + '</div>', x, y);
    }, 400);
    var url = '/api/lsp/hover?path=' + encodeURIComponent(hit.path) +
      '&line=' + hit.line + '&char=' + char;
    fetch(url, { signal: ctl.signal })
      .then(function (r) {
        if (!r.ok) throw new Error('hover ' + r.status);
        return r.json();
      })
      .then(function (data) {
        clearTimeout(loadingTimer);
        recordSuccess();
        st.hoverKey = key;
        if (!data.contents) {
          hideTooltip();
          return;
        }
        showTooltip(st.renderMarkdown(data.contents), x, y);
      })
      .catch(function (err) {
        clearTimeout(loadingTimer);
        if (err && err.name === 'AbortError') return;
        hideTooltip();
        recordFailure();
      });
  }

  // ===== Definition jump =====

  function onClick(e) {
    if (st.disabled) return;
    if (!e.metaKey && !e.ctrlKey) return;
    var hit = eligibleLineEl(e.target);
    if (!hit) return;
    var char = caretCharOffset(hit.contentEl, e.clientX, e.clientY);
    if (char < 0) return;
    e.preventDefault();
    e.stopPropagation();
    clearTimeout(st.hoverTimer);
    hideTooltip();
    var url = '/api/lsp/definition?path=' + encodeURIComponent(hit.path) +
      '&line=' + hit.line + '&char=' + char;
    document.documentElement.classList.add('lsp-busy');
    fetch(url)
      .finally(function () {
        document.documentElement.classList.remove('lsp-busy');
      })
      .then(function (r) {
        if (!r.ok) throw new Error('definition ' + r.status);
        return r.json();
      })
      .then(function (data) {
        recordSuccess();
        var locs = data.locations || [];
        if (locs.length === 0) {
          st.toast(st.notFoundText);
          return;
        }
        if (locs.length === 1) {
          resolveJump(locs[0]);
          return;
        }
        showPeek(locs, 0);
      })
      .catch(function (err) {
        if (err && err.name === 'AbortError') return;
        recordFailure();
        st.toast(st.errorText);
      });
  }

  // resolveJump tries the in-review jump first, falling back to the peek
  // popup (the server always attaches a peek when the file is readable).
  function resolveJump(loc) {
    Promise.resolve(loc.in_session ? st.jumpToLocation(loc) : false)
      .then(function (handled) {
        if (!handled) showPeek([loc], 0);
      });
  }

  // ===== Peek popup =====

  function hidePeek() {
    if (st.peek) {
      st.peek.remove();
      st.peek = null;
      st.peekStack = [];
      st.peekLocs = null;
      st.peekActive = 0;
      document.removeEventListener('keydown', onPeekKeydown, true);
    }
  }

  function onPeekKeydown(e) {
    if (e.key === 'Escape') {
      e.stopPropagation();
      // Step back through chained jumps first; close once at the root.
      if (st.peek && st.peekStack.length > 0) {
        popPeekView(st.peek);
      } else {
        hidePeek();
      }
    }
  }

  function highlightGoLine(text) {
    if (window.hljs) {
      try {
        return window.hljs.highlight(text, { language: 'go' }).value;
      } catch (err) { /* fall through to escaped text */ }
    }
    return esc(text);
  }

  // Maximum chained-jump history entries kept for the back button.
  var PEEK_HISTORY_MAX = 20;

  function renderPeekView(panel) {
    var locs = st.peekLocs;
    var active = st.peekActive;
    var loc = locs[active];

    var back = panel.querySelector('.lsp-peek-back');
    back.hidden = st.peekStack.length === 0;

    var title = panel.querySelector('.lsp-peek-title');
    title.textContent = loc.display_path + ':' + loc.line;

    var actions = panel.querySelector('.lsp-peek-actions');
    actions.innerHTML = '';
    if (loc.in_repo) {
      var open = document.createElement('a');
      open.className = 'lsp-peek-open';
      open.textContent = st.openFullText;
      open.href = '/files/' + loc.path.split('/').map(encodeURIComponent).join('/');
      open.target = '_blank';
      open.rel = 'noopener';
      actions.appendChild(open);
    }

    var tabsEl = panel.querySelector('.lsp-peek-tabs');
    if (locs.length > 1) {
      tabsEl.hidden = false;
      tabsEl.innerHTML = locs.map(function (l, i) {
        var cls = 'lsp-peek-tab' + (i === active ? ' active' : '');
        return '<button type="button" class="' + cls + '" data-idx="' + i + '">' +
          esc(l.display_path + ':' + l.line) + '</button>';
      }).join('');
    } else {
      tabsEl.hidden = true;
      tabsEl.innerHTML = '';
    }

    var body = panel.querySelector('.lsp-peek-body');
    if (!loc.peek || loc.peek.length === 0) {
      body.innerHTML = '<div class="lsp-peek-empty">' + esc(st.noPreviewText) + '</div>';
      return;
    }
    var html = '';
    if (loc.peek_truncated) {
      html += '<div class="lsp-peek-truncated">' + esc(st.truncatedText) + '</div>';
    }
    for (var i = 0; i < loc.peek.length; i++) {
      var lineNo = loc.peek_start + i;
      var cls = 'lsp-peek-line' + (lineNo === loc.line ? ' lsp-peek-target' : '');
      html += '<div class="' + cls + '" data-line="' + lineNo + '"><span class="lsp-peek-num">' + lineNo +
        '</span><span class="lsp-peek-code">' + highlightGoLine(loc.peek[i]) + '</span></div>';
    }
    body.innerHTML = html;
    var target = body.querySelector('.lsp-peek-target');
    if (target) target.scrollIntoView({ block: 'center' });
  }

  // chainedJumpFromPeek handles Cmd/Ctrl+Click on code inside the popup:
  // definition-from-definition. The server only accepts these positions for
  // files under repo root / GOROOT / GOMODCACHE — the same roots the peek
  // content itself came from.
  function chainedJumpFromPeek(panel, e) {
    var codeEl = e.target.closest('.lsp-peek-code');
    if (!codeEl) return;
    var lineEl = codeEl.closest('.lsp-peek-line');
    if (!lineEl) return;
    var lineNo = parseInt(lineEl.dataset.line, 10);
    if (!lineNo) return;
    var char = caretCharOffset(codeEl, e.clientX, e.clientY);
    if (char < 0) return;
    e.preventDefault();
    e.stopPropagation();
    var from = st.peekLocs[st.peekActive];
    var url = '/api/lsp/definition?path=' + encodeURIComponent(from.path) +
      '&line=' + lineNo + '&char=' + char;
    document.documentElement.classList.add('lsp-busy');
    fetch(url)
      .finally(function () {
        document.documentElement.classList.remove('lsp-busy');
      })
      .then(function (r) {
        if (!r.ok) throw new Error('definition ' + r.status);
        return r.json();
      })
      .then(function (data) {
        var locs = data.locations || [];
        if (locs.length === 0) {
          st.toast(st.notFoundText);
          return;
        }
        if (locs.length === 1 && locs[0].in_session) {
          // Chained jump landed back in the review: close and navigate.
          Promise.resolve(st.jumpToLocation(locs[0])).then(function (handled) {
            if (handled) {
              hidePeek();
            } else {
              pushPeekView(panel, locs, 0);
            }
          });
          return;
        }
        pushPeekView(panel, locs, 0);
      })
      .catch(function () {
        st.toast(st.errorText);
      });
  }

  function pushPeekView(panel, locs, active) {
    st.peekStack.push({ locs: st.peekLocs, active: st.peekActive });
    if (st.peekStack.length > PEEK_HISTORY_MAX) st.peekStack.shift();
    st.peekLocs = locs;
    st.peekActive = active;
    renderPeekView(panel);
  }

  function popPeekView(panel) {
    var prev = st.peekStack.pop();
    if (!prev) return;
    st.peekLocs = prev.locs;
    st.peekActive = prev.active;
    renderPeekView(panel);
  }

  function showPeek(locs, active) {
    hidePeek();
    var panel = document.createElement('div');
    panel.className = 'lsp-peek';
    panel.setAttribute('role', 'dialog');
    panel.setAttribute('aria-label', 'Definition preview');
    panel.innerHTML =
      '<div class="lsp-peek-header">' +
      '<button type="button" class="lsp-peek-back" aria-label="Back to previous definition" hidden>&larr;</button>' +
      '<span class="lsp-peek-title"></span>' +
      '<span class="lsp-peek-actions"></span>' +
      '<button type="button" class="lsp-peek-close" aria-label="Close definition preview">&times;</button>' +
      '</div><div class="lsp-peek-tabs" hidden></div><div class="lsp-peek-body"></div>' +
      '<div class="lsp-peek-hint">' + esc(st.peekHintText) + '</div>';
    document.body.appendChild(panel);
    st.peek = panel;
    st.peekStack = [];
    st.peekLocs = locs;
    st.peekActive = active;
    renderPeekView(panel);
    panel.querySelector('.lsp-peek-close').addEventListener('click', hidePeek);
    panel.querySelector('.lsp-peek-back').addEventListener('click', function () {
      popPeekView(panel);
    });
    panel.addEventListener('click', function (e) {
      var tab = e.target.closest('.lsp-peek-tab');
      if (tab) {
        st.peekActive = parseInt(tab.dataset.idx, 10);
        renderPeekView(panel);
        return;
      }
      if (e.metaKey || e.ctrlKey) chainedJumpFromPeek(panel, e);
    });
    document.addEventListener('keydown', onPeekKeydown, true);
  }

  function onGlobalMousedown(e) {
    if (st.peek && !st.peek.contains(e.target)) hidePeek();
    if (st.tooltip && !st.tooltip.hidden && !st.tooltip.contains(e.target)) hideTooltip();
  }

  function onScroll() {
    if (st) {
      clearTimeout(st.hoverTimer);
      hideTooltip();
    }
  }

  // init wires the document-level listeners. Idempotent; call once after
  // /api/config confirms lsp_available.
  function init(opts) {
    if (st) return;
    st = {
      renderMarkdown: opts.renderMarkdown,
      jumpToLocation: opts.jumpToLocation,
      toast: opts.toast || function () {},
      defHintText: opts.defHintText || '⌘/Ctrl+Click: go to definition',
      notFoundText: opts.notFoundText || 'No definition found',
      errorText: opts.errorText || 'Language server request failed',
      loadingText: opts.loadingText || 'Loading documentation… (first request warms up gopls)',
      openFullText: opts.openFullText || 'Open full file ↗',
      noPreviewText: opts.noPreviewText || 'No preview available',
      truncatedText: opts.truncatedText || 'Large file — showing an excerpt around the definition',
      peekHintText: opts.peekHintText || '⌘/Ctrl+Click: follow definition · Esc: back / close',
      peekStack: [],
      peekLocs: null,
      peekActive: 0,
      tooltip: null,
      peek: null,
      hoverTimer: 0,
      hoverKey: null,
      inflight: null,
      failures: 0,
      disabled: false,
    };
    document.addEventListener('mousemove', onMouseMove);
    document.addEventListener('click', onClick, true);
    document.addEventListener('mousedown', onGlobalMousedown, true);
    document.addEventListener('scroll', onScroll, true);
  }

  var api = {
    init: init,
    textOffsetIn: textOffsetIn,
    findHunkForLine: findHunkForLine,
    findGapForLine: findGapForLine,
  };
  if (typeof window !== 'undefined') {
    window.crit = window.crit || {};
    window.crit.lsp = api;
  }
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  }
})();
