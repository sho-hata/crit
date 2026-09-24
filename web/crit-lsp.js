// crit-lsp.js — LSP hover, go-to-definition, and find-references for
// code-review mode.
//
// Talks to the local Go server's /api/lsp/* endpoints (which proxy a
// language server: gopls, typescript-language-server, pyright). Which file
// extensions are eligible comes from /api/config's lsp_extensions.
// Hover: rest the mouse over eligible code → documentation tooltip. Both
// renderings of a code file are covered: the diff view and file mode's
// document view (whole file, one .line-block per source line).
// Definition: Cmd/Ctrl+Click → jump within the review, or a peek popup when
// the target lives outside the visible diff / session / repo.
// References: Cmd/Ctrl+Shift+Click → side panel listing every reference,
// grouped by file; rows jump in-review or open the peek popup.
//
// Dependencies (window.crit.* namespaces read):
//   - crit.shared (escapeHTML)
//   - crit.lineBlocks (splitHighlightedCode — peek syntax highlighting)
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

  // findHunkForLine returns the index of the hunk whose new-side range
  // contains line (1-based), or -1.
  function findHunkForLine(hunks, line) {
    for (let i = 0; i < (hunks || []).length; i++) {
      const h = hunks[i];
      if (line >= h.NewStart && line < h.NewStart + h.NewCount) return i;
    }
    return -1;
  }

  // findGapForLine returns {prevIdx, nextIdx} when line (1-based, new side)
  // falls in the collapsed gap between two adjacent hunks, or null. Leading
  // and trailing gaps are not covered — callers fall back to the peek popup.
  function findGapForLine(hunks, line) {
    for (let i = 1; i < (hunks || []).length; i++) {
      const prevEnd = hunks[i - 1].NewStart + hunks[i - 1].NewCount;
      if (line >= prevEnd && line < hunks[i].NewStart) {
        return { prevIdx: i - 1, nextIdx: i };
      }
    }
    return null;
  }

  // groupLocationsByFile folds a flat reference-location list into per-file
  // groups, preserving order. Each item keeps its index into the flat list so
  // click handlers can address the original location.
  function groupLocationsByFile(locs) {
    const groups = [];
    const byPath = {};
    for (let i = 0; i < (locs || []).length; i++) {
      const loc = locs[i];
      let group = byPath[loc.display_path];
      if (!group) {
        group = { display_path: loc.display_path, items: [] };
        byPath[loc.display_path] = group;
        groups.push(group);
      }
      group.items.push({ loc: loc, idx: i });
    }
    return groups;
  }

  // makeExtensionMatcher builds a RegExp matching paths whose extension is
  // in exts (server-provided, lower-case, no dots). An empty or missing list
  // matches nothing: init only runs when the server reports lsp_available,
  // which guarantees a non-empty list, and guessing an extension here would
  // just offer hovers the server 4xxes — feeding the failure breaker instead
  // of surfacing the bug.
  function makeExtensionMatcher(exts) {
    var list = (exts || []).filter(function (e) {
      return typeof e === 'string' && /^[a-z0-9]+$/.test(e);
    });
    if (!list.length) return /(?!)/;
    return new RegExp('\\.(' + list.join('|') + ')$', 'i');
  }

  // hljsLanguageForPath maps a peek target's file extension to the highlight.js
  // grammar used to render it, or null when no grammar applies (assembly,
  // embed assets, …) — those render as escaped plain text.
  var HLJS_LANG_BY_EXT = {
    go: 'go',
    ts: 'typescript', mts: 'typescript', cts: 'typescript', tsx: 'typescript',
    js: 'javascript', mjs: 'javascript', cjs: 'javascript', jsx: 'javascript',
    py: 'python', pyi: 'python',
  };
  function hljsLanguageForPath(path) {
    var m = /\.([a-z0-9]+)$/i.exec(path || '');
    if (!m) return null;
    return HLJS_LANG_BY_EXT[m[1].toLowerCase()] || null;
  }

  // composeTooltip assembles the hover tooltip: the documentation, then an
  // optional one-line note, then the fixed key hint. The note is a quiet
  // footnote — same muted size as the hint — so it informs without competing
  // with the documentation the reviewer hovered for. esc escapes text.
  function composeTooltip(docHtml, noteText, hintText, esc) {
    return docHtml +
      (noteText ? '<div class="lsp-tooltip-note">' + esc(noteText) + '</div>' : '') +
      '<div class="lsp-tooltip-hint">' + esc(hintText) + '</div>';
  }

  // peekNoteHTML is the strip shown above a peeked file's source when that
  // file is the reviewer's own local copy of a package (the server marks such
  // targets local_env under range/PR focus): what they read may not be the
  // version the PR uses. '' for every other target.
  function peekNoteHTML(loc, noteText, esc) {
    if (!loc || !loc.local_env || !noteText) return '';
    return '<div class="lsp-peek-note">' + esc(noteText) + '</div>';
  }

  // refSnippet extracts the reference's own source line from its peek window,
  // or '' when the location carries no peek (file outside readable roots).
  function refSnippet(loc) {
    if (!loc.peek || !loc.peek_start) return '';
    const idx = loc.line - loc.peek_start;
    if (idx < 0 || idx >= loc.peek.length) return '';
    return loc.peek[idx];
  }

  // ===== Controller =====

  const HOVER_DELAY_MS = 350;
  const MAX_CONSECUTIVE_FAILURES = 3;
  // Cap for a server error rendered into a tooltip or toast.
  const MAX_ERROR_CHARS = 200;
  // After the breaker trips, allow another attempt this long after the last
  // failure (half-open): gopls warm-up on large repos can outlast the
  // server's retry window, and a permanent disable would outlive the outage.
  const DISABLE_RETRY_MS = 30000;

  let st = null; // controller state; null until init()

  function esc(s) {
    return window.crit.shared.escapeHTML(s);
  }

  // eligibleLineEl walks up from an event target to the enclosing source line
  // of an LSP-covered file, returning {contentEl, path, line} or null. Both
  // renderings of a code file have to be recognized, or the feature silently
  // does nothing in whichever one is missing:
  //   - diff view (git mode, and any file whose viewMode is 'diff')
  //   - document view (code files in file mode: `crit some.ts`), which has no
  //     diff markup at all
  function eligibleLineEl(target) {
    if (!target || !target.closest) return null;
    return diffLineHit(target) || documentLineHit(target);
  }

  // diffLineHit resolves a position inside the dual-gutter diff renderer.
  // Unified view rows are .diff-line; split view sides are .diff-split-side —
  // both carry the same data-diff-* attributes via tagDiffLine. Only the new
  // side maps to the file the language server reads.
  function diffLineHit(target) {
    const contentEl = target.closest('.diff-content');
    if (!contentEl) return null;
    const lineEl = contentEl.closest('.diff-line, .diff-split-side');
    if (!lineEl) return null;
    if (lineEl.dataset.diffSide === 'old') return null;
    return lineHit(contentEl, lineEl.dataset.diffFilePath, lineEl.dataset.diffLineNum);
  }

  // documentLineHit resolves a position inside a code file's document view.
  // buildCodeLineBlocks emits one .line-block per source line, so the block's
  // start line is the position's line and .line-content holds exactly that
  // line's text — the same contract caretCharOffset needs from .diff-content.
  // The .code-document scope keeps markdown document/rendered-diff blocks
  // (same markup, multi-line ranges, rendered prose) out.
  function documentLineHit(target) {
    const contentEl = target.closest('.code-document .line-content');
    if (!contentEl) return null;
    const lineEl = contentEl.closest('.line-block');
    if (!lineEl) return null;
    if (lineEl.dataset.startLine !== lineEl.dataset.endLine) return null;
    return lineHit(contentEl, lineEl.dataset.filePath, lineEl.dataset.startLine);
  }

  // lineHit builds the hit object once the renderer-specific lookup has found
  // the file path and line number, dropping files no installed server covers.
  function lineHit(contentEl, path, lineNum) {
    if (!path || !st.extRe.test(path)) return null;
    const line = parseInt(lineNum, 10);
    if (!line) return null;
    return { contentEl: contentEl, path: path, line: line };
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

  // recordFailure counts one failed request and opens the breaker on the
  // third in a row, returning whether this call opened it. The breaker stops
  // hover from firing at all, so it has to announce itself: silently going
  // dead is indistinguishable from the feature never having worked.
  function recordFailure() {
    st.failures++;
    if (st.failures >= MAX_CONSECUTIVE_FAILURES && !st.disabled) {
      st.disabled = true;
      st.disabledAt = Date.now();
      hideTooltip();
      st.toast(st.disabledText);
      return true;
    }
    return false;
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

  function showTooltip(html, x, y, note) {
    const tip = ensureTooltip();
    tip.innerHTML = composeTooltip(html, note, st.defHintText, esc);
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

  // serverErrorText turns a failed LSP response into one line a reviewer can
  // act on. The Go handlers answer with a plain-text reason (a language server
  // that would not start, a workspace that could not be prepared) and that
  // reason is the whole diagnosis — dropping it is what made a broken language
  // server look like a flaky one.
  function serverErrorText(body, status) {
    const line = String(body || '').trim().split('\n')[0];
    if (!line) return 'Language server request failed (HTTP ' + status + ')';
    return line.length > MAX_ERROR_CHARS ? line.slice(0, MAX_ERROR_CHARS - 1) + '…' : line;
  }

  // responseError rejects with the server's own reason for a non-ok response.
  function responseError(r) {
    return r.text().then(function (body) {
      throw new Error(serverErrorText(body, r.status));
    });
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
    let loadingShown = false;
    const loadingTimer = setTimeout(function () {
      loadingShown = true;
      showTooltip('<div class="lsp-tooltip-loading">' + esc(st.loadingText) + '</div>', x, y);
    }, 400);
    const url = '/api/lsp/hover?path=' + encodeURIComponent(hit.path) +
      '&line=' + hit.line + '&char=' + char;
    fetch(url, { signal: ctl.signal })
      .then(function (r) {
        if (!r.ok) return responseError(r);
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
        showTooltip(st.renderMarkdown(data.contents), x, y, data.local_env ? st.localEnvText : '');
      })
      .catch(function (err) {
        clearTimeout(loadingTimer);
        if (err && err.name === 'AbortError') return;
        if (loadingShown) {
          // The reviewer watched a spinner sit there; swapping it for nothing
          // reads as "hover is broken". Show what the server actually said.
          showTooltip('<div class="lsp-tooltip-error">' + esc(err.message) + '</div>', x, y);
        } else {
          hideTooltip();
        }
        recordFailure();
      });
  }

  // ===== Definition jump / references =====

  // setBusy keeps the shared progress cursor accurate under overlapping
  // requests: a counter, not a boolean, so the first response to settle
  // cannot clear the cursor while another request is still in flight.
  function setBusy(on) {
    st.busyCount += on ? 1 : -1;
    document.documentElement.classList.toggle('lsp-busy', st.busyCount > 0);
  }

  // fetchLocations GETs a location-list LSP endpoint with the busy cursor,
  // response check, and breaker accounting in one place. onLocations handles
  // the non-empty outcome; errors toast and count toward the failure breaker.
  //
  // Each call bumps st.defSeq and the UI callbacks only run while this
  // request is still the newest one — a later click (or hidePeek, which also
  // bumps the sequence) invalidates responses still in flight, so a stale
  // result can never scroll the review or render into a closed peek.
  function fetchLocations(url, onLocations, emptyText) {
    const seq = ++st.defSeq;
    setBusy(true);
    fetch(url)
      .finally(function () {
        setBusy(false);
      })
      .then(function (r) {
        if (!r.ok) return responseError(r);
        return r.json();
      })
      .then(function (data) {
        recordSuccess();
        if (seq !== st.defSeq) return;
        const locs = data.locations || [];
        if (locs.length === 0) {
          st.toast(emptyText || st.notFoundText);
          return;
        }
        onLocations(locs, data);
      })
      .catch(function (err) {
        if (err && err.name === 'AbortError') return;
        const tripped = recordFailure(); // already toasted the breaker notice
        if (seq !== st.defSeq || tripped) return;
        st.toast(err.message || st.errorText);
      });
  }

  function onClick(e) {
    if (isDisabled()) return;
    if (!e.metaKey && !e.ctrlKey) return;
    const hit = eligibleLineEl(e.target);
    if (!hit) return;
    const char = caretCharOffset(hit.contentEl, e.clientX, e.clientY);
    if (char < 0) return;
    e.preventDefault();
    e.stopPropagation();
    clearTimeout(st.hoverTimer);
    hideTooltip();
    if (e.shiftKey) {
      requestReferences(hit, char);
      return;
    }
    const url = '/api/lsp/definition?path=' + encodeURIComponent(hit.path) +
      '&line=' + hit.line + '&char=' + char;
    fetchLocations(url, function (locs) {
      if (locs.length === 1) {
        resolveJump(locs[0]);
        return;
      }
      showPeek(locs, 0);
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
      // Invalidate chained-jump requests still in flight: their target
      // panel is gone.
      st.defSeq++;
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
    // ← steps back through the jump history (matches the on-screen back
    // button). Only when history exists — otherwise leave the key to the
    // browser for horizontal scrolling of wide peek lines.
    if (e.key === 'ArrowLeft' && st.peek && st.peekStack.length > 0) {
      e.preventDefault();
      e.stopPropagation();
      popPeekView(st.peek);
    }
  }

  // highlightPeek highlights all lines in ONE hljs pass — with the grammar
  // matching the peeked file's extension — and splits the result per line
  // with splitHighlightedCode (span state carries across lines), so
  // multi-line constructs — block comments, raw/template strings — keep
  // correct colors and a 2000-line peek costs one highlight call, not 2000.
  // Falls back to escaped plain text when no grammar covers the file or
  // hljs / the splitter is missing.
  function highlightPeek(lines, path) {
    const lineBlocks = window.crit.lineBlocks;
    const language = hljsLanguageForPath(path);
    if (language && window.hljs && lineBlocks && lineBlocks.splitHighlightedCode) {
      try {
        const html = window.hljs.highlight(lines.join('\n'), { language: language }).value;
        const split = lineBlocks.splitHighlightedCode(html);
        if (split.length === lines.length) return split;
      } catch (err) { /* fall through to escaped text */ }
    }
    return lines.map(esc);
  }

  // Maximum chained-jump history entries kept for the back button.
  const PEEK_HISTORY_MAX = 20;

  function renderPeekView(panel) {
    const locs = st.peekLocs;
    const active = st.peekActive;
    const loc = locs[active];

    const back = panel.querySelector('.lsp-peek-back');
    back.hidden = st.peekStack.length === 0;

    const title = panel.querySelector('.lsp-peek-title');
    title.textContent = loc.display_path + ':' + loc.line;

    const actions = panel.querySelector('.lsp-peek-actions');
    actions.innerHTML = '';
    if (loc.in_repo) {
      const open = document.createElement('a');
      open.className = 'lsp-peek-open';
      open.textContent = st.openFullText;
      open.href = '/files/' + loc.path.split('/').map(encodeURIComponent).join('/');
      open.target = '_blank';
      open.rel = 'noopener';
      actions.appendChild(open);
    }

    const tabsEl = panel.querySelector('.lsp-peek-tabs');
    if (locs.length > 1) {
      tabsEl.hidden = false;
      tabsEl.innerHTML = locs.map(function (l, i) {
        const cls = 'lsp-peek-tab' + (i === active ? ' active' : '');
        return '<button type="button" class="' + cls + '" data-idx="' + i + '">' +
          esc(l.display_path + ':' + l.line) + '</button>';
      }).join('');
    } else {
      tabsEl.hidden = true;
      tabsEl.innerHTML = '';
    }

    const body = panel.querySelector('.lsp-peek-body');
    if (!loc.peek || loc.peek.length === 0) {
      body.innerHTML = '<div class="lsp-peek-empty">' + esc(st.noPreviewText) + '</div>';
      return;
    }
    let html = peekNoteHTML(loc, st.localEnvPeekText, esc);
    if (loc.peek_truncated) {
      html += '<div class="lsp-peek-truncated">' + esc(st.truncatedText) + '</div>';
    }
    // Highlight once per location and cache: tab switches and history steps
    // re-render, but the peek content never changes.
    loc.hl = loc.hl || highlightPeek(loc.peek, loc.path);
    const codeLines = loc.hl;
    for (let i = 0; i < loc.peek.length; i++) {
      const lineNo = loc.peek_start + i;
      const cls = 'lsp-peek-line' + (lineNo === loc.line ? ' lsp-peek-target' : '');
      html += '<div class="' + cls + '" data-line="' + lineNo + '"><span class="lsp-peek-num">' + lineNo +
        '</span><span class="lsp-peek-code">' + codeLines[i] + '</span></div>';
    }
    body.innerHTML = html;
    const target = body.querySelector('.lsp-peek-target');
    if (target) target.scrollIntoView({ block: 'center' });
  }

  // chainedJumpFromPeek handles Cmd/Ctrl+Click on code inside the popup:
  // definition-from-definition. The server only accepts these positions for
  // files under repo root / GOROOT / GOMODCACHE — the same roots the peek
  // content itself came from.
  function chainedJumpFromPeek(panel, e) {
    const codeEl = e.target.closest('.lsp-peek-code');
    if (!codeEl) return;
    const from = st.peekLocs[st.peekActive];
    // Peeks can render targets outside LSP coverage (embed assets, runtime
    // assembly); the server only answers definition requests for covered
    // extensions, so don't send one — a guaranteed 4xx would just feed the
    // failure breaker.
    if (!from || !st.extRe.test(from.path)) return;
    const lineEl = codeEl.closest('.lsp-peek-line');
    if (!lineEl) return;
    const lineNo = parseInt(lineEl.dataset.line, 10);
    if (!lineNo) return;
    const char = caretCharOffset(codeEl, e.clientX, e.clientY);
    if (char < 0) return;
    e.preventDefault();
    e.stopPropagation();
    const url = '/api/lsp/definition?path=' + encodeURIComponent(from.path) +
      '&line=' + lineNo + '&char=' + char;
    fetchLocations(url, function (locs) {
      if (st.peek !== panel) return; // peek replaced while in flight
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
    const prev = st.peekStack.pop();
    if (!prev) return;
    st.peekLocs = prev.locs;
    st.peekActive = prev.active;
    renderPeekView(panel);
  }

  function showPeek(locs, active) {
    hidePeek();
    const panel = document.createElement('div');
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
      const tab = e.target.closest('.lsp-peek-tab');
      if (tab) {
        st.peekActive = parseInt(tab.dataset.idx, 10);
        renderPeekView(panel);
        return;
      }
      if (e.metaKey || e.ctrlKey) chainedJumpFromPeek(panel, e);
    });
    document.addEventListener('keydown', onPeekKeydown, true);
  }

  // ===== References panel =====

  function requestReferences(hit, char) {
    const url = '/api/lsp/references?path=' + encodeURIComponent(hit.path) +
      '&line=' + hit.line + '&char=' + char;
    fetchLocations(url, function (locs, data) {
      showRefs(locs, !!data.truncated);
    }, st.refsNotFoundText);
  }

  function hideRefs() {
    if (st.refs) {
      st.refs.remove();
      st.refs = null;
      st.refsLocs = null;
      document.removeEventListener('keydown', onRefsKeydown, true);
    }
  }

  function onRefsKeydown(e) {
    // The peek popup (opened from a row) owns Escape while it is visible.
    if (e.key === 'Escape' && !st.peek) {
      e.stopPropagation();
      hideRefs();
    }
  }

  // showRefs renders the references side panel: rows grouped by file, each
  // showing the reference's own source line. Rows inside the review jump to
  // the diff; everything else opens the peek popup.
  function showRefs(locs, truncated) {
    hideRefs();
    hidePeek();
    const panel = document.createElement('div');
    panel.className = 'lsp-refs';
    panel.setAttribute('role', 'dialog');
    panel.setAttribute('aria-label', 'References');
    const title = locs.length + ' ' + (locs.length === 1 ? st.refText : st.refsText) +
      (truncated ? ' ' + st.refsTruncatedText : '');
    let html = '<div class="lsp-peek-header">' +
      '<span class="lsp-peek-title">' + esc(title) + '</span>' +
      '<button type="button" class="lsp-peek-close" aria-label="Close references">&times;</button>' +
      '</div><div class="lsp-refs-body">';
    const groups = groupLocationsByFile(locs);
    for (let g = 0; g < groups.length; g++) {
      const group = groups[g];
      html += '<div class="lsp-refs-file">' + esc(group.display_path) +
        '<span class="lsp-refs-count">' + group.items.length + '</span></div>';
      for (let i = 0; i < group.items.length; i++) {
        const item = group.items[i];
        const snippet = refSnippet(item.loc);
        // Locations outside the readable roots carry no peek, so say so
        // rather than rendering a clickable row with an empty code cell.
        // Each row is an isolated line, so highlight it on its own —
        // batching unrelated lines would leak parser state between rows.
        const code = snippet
          ? '<span class="lsp-peek-code">' + highlightPeek([snippet], item.loc.path)[0] + '</span>'
          : '<span class="lsp-peek-code lsp-refs-nopreview">' + esc(st.noPreviewText) + '</span>';
        html += '<button type="button" class="lsp-refs-item" data-idx="' + item.idx + '">' +
          '<span class="lsp-peek-num">' + item.loc.line + '</span>' + code +
          '</button>';
      }
    }
    html += '</div><div class="lsp-peek-hint">' + esc(st.refsHintText) + '</div>';
    panel.innerHTML = html;
    document.body.appendChild(panel);
    st.refs = panel;
    st.refsLocs = locs;
    panel.querySelector('.lsp-peek-close').addEventListener('click', hideRefs);
    panel.addEventListener('click', function (e) {
      const row = e.target.closest('.lsp-refs-item');
      if (!row) return;
      const loc = st.refsLocs[parseInt(row.dataset.idx, 10)];
      if (!loc) return;
      const prev = panel.querySelector('.lsp-refs-item.active');
      if (prev) prev.classList.remove('active');
      row.classList.add('active');
      // In-review rows jump and keep the panel open so the user can walk the
      // list; everything else falls back to the peek popup.
      Promise.resolve(loc.in_session ? st.jumpToLocation(loc) : false)
        .then(function (handled) {
          if (!handled) showPeek([loc], 0);
        });
    });
    document.addEventListener('keydown', onRefsKeydown, true);
  }

  // Shift+Click asks for LSP references, but the browser reads it as a text
  // selection across the diff. Selection is the *mousedown* default action,
  // so it has to be suppressed here — by the time onClick runs the range is
  // already painted.
  function suppressChordSelection(e) {
    if (e.button !== 0 || !e.shiftKey) return;
    if (!e.metaKey && !e.ctrlKey) return;
    if (isDisabled()) return;
    if (!eligibleLineEl(e.target)) return;
    e.preventDefault();
  }

  function onGlobalMousedown(e) {
    suppressChordSelection(e);
    // A hover request armed just before the click must not resurrect the
    // tooltip the click is dismissing: hideTooltip only nulls st.hoverKey,
    // so the pending timer would fire past the dedup check.
    clearTimeout(st.hoverTimer);
    if (st.peek && !st.peek.contains(e.target)) hidePeek();
    if (st.refs && !st.refs.contains(e.target) && !(st.peek && st.peek.contains(e.target))) hideRefs();
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
      defHintText: opts.defHintText || '⌘/Ctrl+Click: go to definition · +Shift: find references',
      notFoundText: opts.notFoundText || 'No definition found',
      refsNotFoundText: opts.refsNotFoundText || 'No references found',
      refText: opts.refText || 'reference',
      refsText: opts.refsText || 'references',
      refsTruncatedText: opts.refsTruncatedText || '(list truncated)',
      refsHintText: opts.refsHintText || 'Click: jump / preview · Esc: close',
      errorText: opts.errorText || 'Language server request failed',
      disabledText: opts.disabledText ||
        'Language server unavailable — hover and go-to-definition paused for 30s',
      loadingText: opts.loadingText || 'Loading documentation… (first request warms up the language server)',
      extRe: makeExtensionMatcher(opts.extensions),
      openFullText: opts.openFullText || 'Open full file ↗',
      noPreviewText: opts.noPreviewText || 'No preview available',
      truncatedText: opts.truncatedText || 'Large file — showing an excerpt around the definition',
      localEnvText: opts.localEnvText ||
        'ⓘ Third-party types come from your local environment, not this PR\'s dependencies',
      localEnvPeekText: opts.localEnvPeekText ||
        'ⓘ Your local copy of this package — it may differ from the version this PR uses',
      peekHintText: opts.peekHintText || '⌘/Ctrl+Click: follow definition · Esc: back / close',
      peekStack: [],
      peekLocs: null,
      peekActive: 0,
      tooltip: null,
      peek: null,
      refs: null,
      refsLocs: null,
      hoverTimer: 0,
      hoverKey: null,
      inflight: null,
      failures: 0,
      disabled: false,
      disabledAt: 0,
      busyCount: 0,
      defSeq: 0,
    };
    document.addEventListener('mousemove', onMouseMove);
    document.addEventListener('click', onClick, true);
    document.addEventListener('mousedown', onGlobalMousedown, true);
    document.addEventListener('scroll', onScroll, true);
  }

  const api = {
    init: init,
    serverErrorText: serverErrorText,
    textOffsetIn: textOffsetIn,
    findHunkForLine: findHunkForLine,
    findGapForLine: findGapForLine,
    groupLocationsByFile: groupLocationsByFile,
    refSnippet: refSnippet,
    makeExtensionMatcher: makeExtensionMatcher,
    hljsLanguageForPath: hljsLanguageForPath,
    composeTooltip: composeTooltip,
    peekNoteHTML: peekNoteHTML,
  };
  if (typeof window !== 'undefined') {
    window.crit = window.crit || {};
    window.crit.lsp = api;
  }
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  }
})();
