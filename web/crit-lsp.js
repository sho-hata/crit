// crit-lsp.js — LSP hover documentation for code-review mode.
//
// Talks to the local Go server's /api/lsp/* endpoints (which proxy gopls).
// Rest the mouse over Go code in a diff → documentation tooltip.
//
// Dependencies (window.crit.* namespaces read):
//   - crit.shared (escapeHTML)
// UI adapters are injected via init(opts) so pure logic stays testable:
//   - opts.renderMarkdown(text) -> safe HTML for hover markdown
(function () {
  'use strict';

  // ===== Pure helpers (exported for Node tests) =====

  // textOffsetIn returns the UTF-16 offset of (target, offsetInNode) within
  // root's textContent, or -1 when target is not inside root. Duck-typed
  // (nodeType/childNodes/textContent) so tests can pass fake nodes.
  // For text nodes (nodeType 3) offsetInNode is a character offset; for
  // elements it is a child index (caretPositionFromPoint can return either).
  function textOffsetIn(root, target, offsetInNode) {
    let total = 0;
    let found = false;
    function textLen(node) {
      return (node.textContent || '').length;
    }
    function walk(node) {
      if (found) return;
      if (node === target) {
        if (node.nodeType === 3) {
          total += offsetInNode;
        } else {
          const kids = node.childNodes || [];
          for (let i = 0; i < offsetInNode && i < kids.length; i++) total += textLen(kids[i]);
        }
        found = true;
        return;
      }
      if (node.nodeType === 3) {
        total += textLen(node);
        return;
      }
      const children = node.childNodes || [];
      for (let j = 0; j < children.length; j++) {
        walk(children[j]);
        if (found) return;
      }
    }
    walk(root);
    return found ? total : -1;
  }

  // ===== Controller =====

  const HOVER_DELAY_MS = 350;
  const MAX_CONSECUTIVE_FAILURES = 3;
  // After the breaker trips, allow another attempt this long after the last
  // failure (half-open): gopls warm-up on large repos can outlast the
  // server's retry window, and a permanent disable would outlive the outage.
  const DISABLE_RETRY_MS = 30000;

  let st = null; // controller state; null until init()

  function esc(s) {
    return window.crit.shared.escapeHTML(s);
  }

  // eligibleLineEl walks up from an event target to the enclosing new-side
  // .go diff line, returning {lineEl, contentEl} or null. Unified view rows
  // are .diff-line; split view sides are .diff-split-side — both carry the
  // same data-diff-* attributes via tagDiffLine.
  function eligibleLineEl(target) {
    if (!target || !target.closest) return null;
    const contentEl = target.closest('.diff-content');
    if (!contentEl) return null;
    const lineEl = contentEl.closest('.diff-line, .diff-split-side');
    if (!lineEl) return null;
    const path = lineEl.dataset.diffFilePath;
    if (!path || !/\.go$/.test(path)) return null;
    if (lineEl.dataset.diffSide === 'old') return null;
    const line = parseInt(lineEl.dataset.diffLineNum, 10);
    if (!line) return null;
    return { lineEl: lineEl, contentEl: contentEl, path: path, line: line };
  }

  // caretCharOffset computes the UTF-16 column under the pointer, or -1.
  function caretCharOffset(contentEl, x, y) {
    let node = null;
    let offset = 0;
    if (document.caretPositionFromPoint) {
      const pos = document.caretPositionFromPoint(x, y);
      if (pos) {
        node = pos.offsetNode;
        offset = pos.offset;
      }
    } else if (document.caretRangeFromPoint) {
      const range = document.caretRangeFromPoint(x, y);
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
      st.disabledAt = Date.now();
      hideTooltip();
    }
  }

  // isDisabled reports whether the failure breaker is open, letting one
  // attempt through (half-open) once DISABLE_RETRY_MS has passed; a further
  // failure re-trips it immediately.
  function isDisabled() {
    if (!st.disabled) return false;
    if (Date.now() - st.disabledAt < DISABLE_RETRY_MS) return true;
    st.disabled = false;
    st.failures = MAX_CONSECUTIVE_FAILURES - 1;
    return false;
  }

  function recordSuccess() {
    st.failures = 0;
  }

  // ===== Hover tooltip =====

  function ensureTooltip() {
    if (st.tooltip) return st.tooltip;
    const el = document.createElement('div');
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
    const tip = ensureTooltip();
    tip.innerHTML = html;
    tip.hidden = false;
    // Position after layout so we can clamp to the viewport: prefer above
    // the cursor, fall back to below.
    tip.style.left = '0px';
    tip.style.top = '0px';
    const rect = tip.getBoundingClientRect();
    let left = Math.min(x, window.innerWidth - rect.width - 8);
    if (left < 8) left = 8;
    let top = y - rect.height - 12;
    if (top < 8) top = y + 20;
    tip.style.left = left + 'px';
    tip.style.top = top + 'px';
  }

  function onMouseMove(e) {
    if (isDisabled()) return;
    const hit = eligibleLineEl(e.target);
    if (!hit) {
      // Moving onto the tooltip itself keeps it open (lets users select
      // text) — but drop any pending hover request from the last on-line
      // position so it cannot fire with stale coordinates.
      if (st.tooltip && !st.tooltip.hidden && st.tooltip.contains(e.target)) {
        clearTimeout(st.hoverTimer);
        return;
      }
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
    const char = caretCharOffset(hit.contentEl, x, y);
    if (char < 0) return;
    const key = hit.path + ':' + hit.line + ':' + char;
    if (st.hoverKey === key && st.tooltip && !st.tooltip.hidden) return;
    abortInflight();
    const ctl = new AbortController();
    st.inflight = ctl;
    // Slow-response indicator: the first request after gopls spawns can take
    // seconds (workspace load). Show a placeholder so the wait is visible.
    const loadingTimer = setTimeout(function () {
      showTooltip('<div class="lsp-tooltip-loading">' + esc(st.loadingText) + '</div>', x, y);
    }, 400);
    const url = '/api/lsp/hover?path=' + encodeURIComponent(hit.path) +
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

  function onGlobalMousedown(e) {
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
      loadingText: opts.loadingText || 'Loading documentation… (first request warms up gopls)',
      tooltip: null,
      hoverTimer: 0,
      hoverKey: null,
      inflight: null,
      failures: 0,
      disabled: false,
      disabledAt: 0,
    };
    document.addEventListener('mousemove', onMouseMove);
    document.addEventListener('mousedown', onGlobalMousedown, true);
    document.addEventListener('scroll', onScroll, true);
  }

  const api = {
    init: init,
    textOffsetIn: textOffsetIn,
  };
  if (typeof window !== 'undefined') {
    window.crit = window.crit || {};
    window.crit.lsp = api;
  }
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  }
})();
