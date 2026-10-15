// crit-lsp.js — LSP hover, go-to-definition, and find-references for
// code-review mode.
//
// Talks to the server's /api/lsp/* endpoints (which proxy a language server);
// eligible file extensions come from /api/config's lsp_extensions.
// Hover: documentation tooltip over any code line Pierre renders — a diff
// (split or unified) or a whole file (files mode).
// Definition: Cmd/Ctrl+Click → jump within the review, or a peek popup when
// the target lives outside the visible diff / session / repo.
// References: Cmd/Ctrl+Shift+Click → inline widget under the clicked line
// (preview + list grouped by file); double-click / Enter jumps in-review.
//
// Code lines live in @pierre/diffs' shadow roots: a line is `[data-line]`
// inside the column's <code> (data-additions / data-deletions / data-unified
// / data-code), the host <diffs-container> names the file (data-crit-path).
// Events reach the document retargeted to the host, so lines are found on
// composedPath(); see lineHitFromPath and caretCharOffset.
//
// Dependencies (window.crit.* namespaces read):
//   - crit.shared (escapeHTML)
//   - crit.codeHighlight (Shiki via Pierre's worker pool — peek highlighting)
// UI adapters are injected via init(opts) so pure logic stays testable:
//   - opts.renderMarkdown(text) -> safe HTML for hover markdown
//   - opts.jumpToLocation(loc)  -> Promise<boolean>; true when the app
//     revealed the location inside the review UI
//   - opts.canJump(loc)         -> whether jumpToLocation is worth asking for
//     this location (default: loc.in_session; the file viewer says in_repo)
//   - opts.mountLineWidget(path, line, el) -> Promise<boolean>; attaches el
//     under that line (a Pierre annotation); false when the line is not shown
//   - opts.unmountLineWidget(el) -> detaches it again
//   - opts.languageForPath(path) -> Shiki grammar id for a file, or null
//   - opts.toast(message)       -> transient error/info notice
(function () {
  'use strict';

  // ===== Pure helpers (exported for Node tests) =====

  // UTF-16 offset of (target, offsetInNode) within root's text — the column
  // the language server expects. Duck-typed (nodeType/childNodes/textContent)
  // so tests can pass fake nodes. offsetInNode is a character offset for a
  // text node but a child index for an element — caretPositionFromPoint can
  // return either.
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

  // The code line under an event, from its composedPath() (innermost first):
  // the `[data-line]` row, then the <code> column it sits in, then the
  // <diffs-container> host whose data-crit-path names the file. Gutter cells
  // carry data-column-number instead of data-line, so they never match.
  // Only the new side maps to the file the language server reads: the
  // deletions column, and change-deletion rows of a unified diff, give null.
  // Duck-typed (tagName/dataset) so tests can pass plain objects.
  function lineHitFromPath(path) {
    let lineEl = null;
    let codeEl = null;
    let host = null;
    for (let i = 0; i < (path || []).length; i++) {
      const node = path[i];
      const ds = node && node.dataset;
      if (!ds) continue;
      if (!lineEl) {
        if (ds.line !== undefined) lineEl = node;
      } else if (!codeEl) {
        if (node.tagName === 'CODE') codeEl = node;
      } else if (ds.critPath !== undefined) {
        host = node;
        break;
      }
    }
    if (!lineEl || !codeEl || !host) return null;
    if (codeEl.dataset.deletions !== undefined) return null;
    if (codeEl.dataset.unified !== undefined && lineEl.dataset.lineType === 'change-deletion') return null;
    return { lineEl: lineEl, path: host.dataset.critPath, line: parseInt(lineEl.dataset.line, 10) || 0 };
  }

  function viewURL(path, line) {
    const url = '/view?path=' + encodeURIComponent(path);
    return line > 0 ? url + '&line=' + line : url;
  }

  function findHunkForLine(hunks, line) {
    for (let i = 0; i < (hunks || []).length; i++) {
      const h = hunks[i];
      if (line >= h.NewStart && line < h.NewStart + h.NewCount) return i;
    }
    return -1;
  }

  // Only gaps between two hunks count; for leading and trailing gaps callers
  // fall back to the peek popup.
  function findGapForLine(hunks, line) {
    for (let i = 1; i < (hunks || []).length; i++) {
      const prevEnd = hunks[i - 1].NewStart + hunks[i - 1].NewCount;
      if (line >= prevEnd && line < hunks[i].NewStart) {
        return { prevIdx: i - 1, nextIdx: i };
      }
    }
    return null;
  }

  // Each item keeps its index into the flat list so click handlers can
  // address the original location.
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

  // An empty list matches nothing rather than guessing: init only runs when
  // the server reports lsp_available (non-empty list), and a guessed
  // extension would offer hovers the server 4xxes — feeding the failure
  // breaker instead of surfacing the bug.
  function makeExtensionMatcher(exts) {
    var list = (exts || []).filter(function (e) {
      return typeof e === 'string' && /^[a-z0-9]+$/.test(e);
    });
    if (!list.length) return /(?!)/;
    return new RegExp('\\.(' + list.join('|') + ')$', 'i');
  }

  // The note is a quiet footnote (same muted size as the hint), not part of
  // the documentation.
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

  // A location has no peek when its file is outside the readable roots.
  function refSnippet(loc) {
    if (!loc.peek || !loc.peek_start) return '';
    const idx = loc.line - loc.peek_start;
    if (idx < 0 || idx >= loc.peek.length) return '';
    return loc.peek[idx];
  }

  const IDENT_CHAR = /[\p{L}\p{N}_$]/u;
  function wordAt(text, char) {
    if (!text || char < 0 || char > text.length) return '';
    let start = char;
    let end = char;
    while (start > 0 && IDENT_CHAR.test(text[start - 1])) start--;
    while (end < text.length && IDENT_CHAR.test(text[end])) end++;
    return text.slice(start, end);
  }

  // null unless word sits at character, so an aliased import or stale column
  // never highlights the wrong text.
  function symbolRange(lineText, character, word) {
    if (!word || typeof character !== 'number' || character < 0) return null;
    if ((lineText || '').substr(character, word.length) !== word) return null;
    return { start: character, end: character + word.length };
  }

  // ===== Controller =====

  const HOVER_DELAY_MS = 350;
  // The first request after a server spawns can take seconds (workspace
  // load); past this delay a placeholder makes the wait visible.
  const LOADING_DELAY_MS = 400;
  const MAX_CONSECUTIVE_FAILURES = 3;
  const MAX_ERROR_CHARS = 200;
  // After the breaker trips, allow another attempt this long after the last
  // failure (half-open): gopls warm-up on large repos can outlast the
  // server's retry window, and a permanent disable would outlive the outage.
  const DISABLE_RETRY_MS = 30000;

  let st = null; // null until init()

  function esc(s) {
    return window.crit.shared.escapeHTML(s);
  }

  // Every rendering of a code file is Pierre's: split and unified diffs and
  // the whole-file item of files mode all use the same line markup, so one
  // lookup covers them. Markdown documents never carry a covered extension.
  function eligibleLineHit(e) {
    const hit = lineHitFromPath(typeof e.composedPath === 'function' ? e.composedPath() : []);
    if (!hit || !hit.line || !st.extRe.test(hit.path)) return null;
    return hit;
  }

  // The composed path of whatever is under (x, y) right now, innermost
  // first, descending through open shadow roots the way an event's
  // composedPath() would: Pierre re-mounts rows while the pointer rests
  // (virtualization, hydration), so a hit captured on mousemove can be stale
  // by the time the hover timer fires.
  function composedPathAt(x, y) {
    let el = document.elementFromPoint(x, y);
    while (el && el.shadowRoot) {
      const inner = el.shadowRoot.elementFromPoint(x, y);
      if (!inner || inner === el) break;
      el = inner;
    }
    const path = [];
    for (let n = el; n; n = n.parentNode || n.host) path.push(n);
    return path;
  }

  // UTF-16 column under (x, y) within lineEl, or -1.
  //
  // The line is inside Pierre's shadow root. Without the shadowRoots option
  // caretPositionFromPoint retargets its answer to the <diffs-container>
  // host, and caretRangeFromPoint always does (measured in Chrome 155 and
  // Safari 26.4; both honour the option), so the text node under the cursor
  // is only reachable by naming the root. Light-DOM callers (the peek popup)
  // pass no option. Browsers without the API fall back to measuring.
  function caretCharOffset(lineEl, x, y) {
    const root = lineEl.getRootNode();
    const shadow = root && root.host ? root : null;
    if (typeof document.caretPositionFromPoint === 'function') {
      let pos = null;
      try {
        pos = document.caretPositionFromPoint(x, y, shadow ? { shadowRoots: [shadow] } : undefined);
      } catch (err) {
        pos = null;
      }
      if (pos && pos.offsetNode) {
        const offset = textOffsetIn(lineEl, pos.offsetNode, pos.offset);
        if (offset >= 0) return offset;
      }
    }
    return measuredCharOffset(lineEl, shadow || document, x, y);
  }

  // Geometric fallback: hit-test the token span through the shadow root
  // (elementFromPoint works on a ShadowRoot everywhere), then measure that
  // span's characters one Range at a time until one reaches x. Returns the
  // nearer boundary, like a caret would; -1 past the end of the text.
  function measuredCharOffset(lineEl, scope, x, y) {
    let el = typeof scope.elementFromPoint === 'function' ? scope.elementFromPoint(x, y) : null;
    if (!el || !lineEl.contains(el)) el = lineEl;
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    const range = document.createRange();
    let node;
    while ((node = walker.nextNode())) {
      for (let i = 0; i < node.nodeValue.length; i++) {
        range.setStart(node, i);
        range.setEnd(node, i + 1);
        const r = range.getBoundingClientRect();
        if (r.width === 0 || x > r.right) continue;
        return textOffsetIn(lineEl, node, x < (r.left + r.right) / 2 ? i : i + 1);
      }
    }
    return -1;
  }

  // Returns whether this call opened the breaker. The breaker stops hover
  // from firing at all, so it has to announce itself: silently going dead is
  // indistinguishable from the feature never having worked.
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

  // Half-open: once DISABLE_RETRY_MS has passed one attempt goes through, and
  // a further failure re-trips the breaker immediately.
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
    if (st.tooltip) st.tooltip.hidden = true;
    st.hoverKey = null;
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
    // Measure after layout to clamp to the viewport; prefer above the cursor.
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
    // A pressed button is a drag (gutter range, text selection): never pop a
    // tooltip into its path.
    if (e.buttons) return;
    const hit = eligibleLineHit(e);
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

  // The Go handlers answer with a plain-text reason (a language server that
  // would not start, a workspace that could not be prepared) and that reason
  // is the whole diagnosis — without it a broken server looks like a flaky one.
  function serverErrorText(body, status) {
    const line = String(body || '').trim().split('\n')[0];
    if (!line) return 'Language server request failed (HTTP ' + status + ')';
    return line.length > MAX_ERROR_CHARS ? line.slice(0, MAX_ERROR_CHARS - 1) + '…' : line;
  }

  function responseError(r) {
    return r.text().then(function (body) {
      const err = new Error(serverErrorText(body, r.status));
      err.status = r.status;
      throw err;
    });
  }

  function requestHover(armed, x, y) {
    // The row can have been unmounted by virtualization during the delay;
    // ask again what is under the pointer now.
    const hit = armed.lineEl.isConnected ? armed : eligibleLineHit({
      composedPath: function () { return composedPathAt(x, y); },
    });
    if (!hit) return;
    const char = caretCharOffset(hit.lineEl, x, y);
    if (char < 0) return;
    const key = hit.path + ':' + hit.line + ':' + char;
    if (st.hoverKey === key && st.tooltip && !st.tooltip.hidden) return;
    abortInflight();
    const ctl = new AbortController();
    st.inflight = ctl;
    let loadingShown = false;
    const loadingTimer = setTimeout(function () {
      loadingShown = true;
      showTooltip('<div class="lsp-tooltip-loading">' + esc(st.loadingText) + '</div>', x, y);
    }, LOADING_DELAY_MS);
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

  // UI callbacks only run while this request is still the newest one: a
  // later click (or hidePeek, which also bumps st.defSeq) invalidates
  // responses still in flight, so a stale result can never scroll the review
  // or render into a closed peek.
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
        // 504 = the server is alive but still indexing (a cold gopls takes
        // 20-30s on references to a widely imported name). Counting it would
        // let a couple of clicks switch hover off for a healthy server.
        const tripped = err && err.status === 504 ? false : recordFailure(); // already toasted the breaker notice
        if (seq !== st.defSeq || tripped) return;
        st.toast(err.message || st.errorText);
      });
  }

  function onClick(e) {
    if (isDisabled()) return;
    if (!e.metaKey && !e.ctrlKey) return;
    const hit = eligibleLineHit(e);
    if (!hit) return;
    const char = caretCharOffset(hit.lineEl, e.clientX, e.clientY);
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

  // The peek fallback works because the server attaches a peek whenever the
  // file is readable.
  function resolveJump(loc) {
    Promise.resolve(st.canJump(loc) ? st.jumpToLocation(loc) : false)
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

  // Peek and reference snippets are highlighted by the Shiki worker pool the
  // diffs use (crit-code-highlight), one tokenization per location: span
  // state carries across lines, so block comments and raw strings keep
  // their colours, and a 2000-line peek costs one request, not 2000. The
  // grammar is Pierre's own choice for the file name (opts.languageForPath).
  // Tokenization is asynchronous: `html` is ready to render now (cached
  // tokens, or escaped text), and `ready` resolves with the highlighted lines
  // once they exist — null when the first render was already final.
  function highlightLines(lines, path) {
    const ch = window.crit.codeHighlight;
    const lang = st.languageForPath ? st.languageForPath(path) : null;
    const plain = lines.map(esc);
    if (!ch || !lang || lang === 'text') return { html: plain, ready: null };
    const code = lines.join('\n');
    const cached = ch.lines(code, lang);
    if (cached && cached.length === lines.length) return { html: cached, ready: null };
    const ready = ch.prime([{ code: code, lang: lang }]).then(function (results) {
      const out = results && results[0];
      return out && out.length === lines.length ? out : null;
    }, function () {
      return null;
    });
    return { html: plain, ready: ready };
  }

  // loc.hl caches a location's highlighted peek lines: tab switches and
  // history steps re-render, but the peek content never changes. The first
  // call renders plain text and re-runs `rerender` when the tokens arrive,
  // provided that location is still the one on screen.
  function peekLines(loc, isCurrent, rerender) {
    if (loc.hl) return loc.hl;
    const hl = highlightLines(loc.peek, loc.path);
    loc.hl = hl.html;
    if (hl.ready) {
      hl.ready.then(function (out) {
        if (!out) return;
        loc.hl = out;
        if (isCurrent()) rerender();
      });
    }
    return loc.hl;
  }

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
      open.href = viewURL(loc.path, loc.line);
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
    const codeLines = peekLines(loc, function () {
      return st.peek === panel && st.peekLocs[st.peekActive] === loc;
    }, function () {
      renderPeekView(panel);
    });
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

  // The server accepts these absolute positions only under the same roots
  // the peek content itself came from.
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
      if (locs.length === 1 && st.canJump(locs[0])) {
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

  // ===== References widget =====

  function requestReferences(hit, char) {
    const url = '/api/lsp/references?path=' + encodeURIComponent(hit.path) +
      '&line=' + hit.line + '&char=' + char;
    const word = wordAt(hit.lineEl.textContent, char);
    fetchLocations(url, function (locs, data) {
      showRefs(locs, !!data.truncated, { path: hit.path, line: hit.line }, word);
    }, st.refsNotFoundText);
  }

  function hideRefs() {
    if (st.refs) {
      const panel = st.refs;
      st.refs = null;
      st.refsLocs = null;
      if (panel.classList.contains('lsp-refs-floating') || !st.unmountLineWidget) panel.remove();
      else st.unmountLineWidget(panel);
      document.removeEventListener('keydown', onRefsKeydown, true);
    }
  }

  function onRefsKeydown(e) {
    const panel = st.refs;
    if (!panel || !panel.isConnected) {
      hideRefs();
      return;
    }
    // The peek popup owns Escape while it is visible.
    if (e.key === 'Escape' && !st.peek) {
      e.stopPropagation();
      hideRefs();
      return;
    }
    // Leave keys to the review's shortcuts once focus moves elsewhere.
    if (!panel.contains(document.activeElement)) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      e.stopPropagation();
      const next = st.refsActive + (e.key === 'ArrowDown' ? 1 : -1);
      if (next >= 0 && next < st.refsLocs.length) selectRef(panel, next);
    } else if (e.key === 'Enter') {
      e.preventDefault();
      e.stopPropagation();
      openRef(st.refsActive);
    }
  }

  function markSymbol(el, range) {
    if (!range) return;
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    const r = document.createRange();
    let pos = 0;
    let started = false;
    let node;
    while ((node = walker.nextNode())) {
      const len = node.nodeValue.length;
      if (!started && range.start <= pos + len) {
        r.setStart(node, range.start - pos);
        started = true;
      }
      if (started && range.end <= pos + len) {
        r.setEnd(node, range.end - pos);
        const mark = document.createElement('mark');
        mark.className = 'lsp-refs-match';
        mark.appendChild(r.extractContents());
        r.insertNode(mark);
        return;
      }
      pos += len;
    }
  }

  function renderRefPreview(panel, loc) {
    const title = panel.querySelector('.lsp-refs-where');
    title.textContent = loc.display_path + ':' + loc.line;
    const actions = panel.querySelector('.lsp-peek-actions');
    actions.innerHTML = '';
    if (loc.in_repo) {
      const open = document.createElement('a');
      open.className = 'lsp-peek-open';
      open.textContent = st.openFullText;
      open.href = viewURL(loc.path, loc.line);
      open.target = '_blank';
      open.rel = 'noopener';
      actions.appendChild(open);
    }
    const body = panel.querySelector('.lsp-refs-preview');
    if (!loc.peek || loc.peek.length === 0) {
      body.innerHTML = '<div class="lsp-peek-empty">' + esc(st.noPreviewText) + '</div>';
      return;
    }
    const codeLines = peekLines(loc, function () {
      return st.refs === panel && st.refsLocs[st.refsActive] === loc;
    }, function () {
      renderRefPreview(panel, loc);
    });
    let html = peekNoteHTML(loc, st.localEnvPeekText, esc);
    for (let i = 0; i < loc.peek.length; i++) {
      const lineNo = loc.peek_start + i;
      const cls = 'lsp-peek-line' + (lineNo === loc.line ? ' lsp-peek-target' : '');
      html += '<div class="' + cls + '"><span class="lsp-peek-num">' + lineNo +
        '</span><span class="lsp-peek-code">' + codeLines[i] + '</span></div>';
    }
    body.innerHTML = html;
    const target = body.querySelector('.lsp-peek-target');
    if (target) {
      markSymbol(target.querySelector('.lsp-peek-code'),
        symbolRange(refSnippet(loc), loc.character, st.refsWord));
      // Scroll the pane only — scrollIntoView would also move the page.
      body.scrollTop = target.offsetTop - (body.clientHeight - target.offsetHeight) / 2;
    }
  }

  function selectRef(panel, idx) {
    st.refsActive = idx;
    const prev = panel.querySelector('.lsp-refs-item.active');
    if (prev) prev.classList.remove('active');
    const row = panel.querySelector('.lsp-refs-item[data-idx="' + idx + '"]');
    if (row) {
      row.classList.add('active');
      row.scrollIntoView({ block: 'nearest' });
    }
    renderRefPreview(panel, st.refsLocs[idx]);
  }

  // Targets outside the review are already shown in the preview pane.
  function openRef(idx) {
    const loc = st.refsLocs && st.refsLocs[idx];
    if (!loc || !st.canJump(loc)) return;
    Promise.resolve(st.jumpToLocation(loc)).then(function (handled) {
      if (!handled) return;
      hideRefs();
      // Removing the widget shifts the target when it sat above it; the
      // second jump only re-centres (the hunk is expanded, the line loaded).
      return st.jumpToLocation(loc);
    });
  }

  // Each row is an isolated line, so it is highlighted on its own (batching
  // unrelated lines would leak parser state between rows), and re-filled
  // when its tokens arrive. Rows are trimmed; the symbol column shifts by
  // the dropped indent.
  function fillRefRows(panel, locs, word) {
    const rows = panel.querySelectorAll('.lsp-refs-item');
    for (let i = 0; i < rows.length; i++) {
      const idx = parseInt(rows[i].dataset.idx, 10);
      const loc = locs[idx];
      const snippet = refSnippet(loc);
      if (!snippet) continue;
      const trimmed = snippet.trim();
      const range = symbolRange(trimmed, loc.character - (snippet.length - snippet.replace(/^\s+/, '').length), word);
      const fill = function (html) {
        const code = panel.querySelector('.lsp-refs-item[data-idx="' + idx + '"] .lsp-peek-code');
        if (!code) return;
        code.innerHTML = html;
        markSymbol(code, range);
      };
      const hl = highlightLines([trimmed], loc.path);
      fill(hl.html[0]);
      if (hl.ready) {
        hl.ready.then(function (out) {
          if (out && st.refs === panel) fill(out[0]);
        });
      }
    }
  }

  function showRefs(locs, truncated, at, word) {
    hideRefs();
    hidePeek();
    const panel = document.createElement('div');
    panel.className = 'lsp-refs';
    panel.setAttribute('role', 'region');
    panel.setAttribute('aria-label', 'References');
    const count = locs.length + ' ' + (locs.length === 1 ? st.refText : st.refsText) +
      (truncated ? ' ' + st.refsTruncatedText : '');
    let html = '<div class="lsp-peek-header">' +
      (word ? '<span class="lsp-refs-symbol">' + esc(word) + '</span>' : '') +
      '<span class="lsp-refs-count-total">' + esc(count) + '</span>' +
      '<span class="lsp-peek-title lsp-refs-where"></span>' +
      '<span class="lsp-peek-actions"></span>' +
      '<button type="button" class="lsp-peek-close" aria-label="Close references">&times;</button>' +
      '</div><div class="lsp-refs-main"><div class="lsp-refs-preview"></div>' +
      '<div class="lsp-refs-list" role="listbox" tabindex="0" aria-label="References">';
    const groups = groupLocationsByFile(locs);
    for (let g = 0; g < groups.length; g++) {
      const group = groups[g];
      html += '<div class="lsp-refs-file" title="' + esc(group.display_path) + '">' +
        '<span class="lsp-refs-file-name">' + esc(group.display_path) + '</span>' +
        '<span class="lsp-refs-count">' + group.items.length + '</span></div>';
      for (let i = 0; i < group.items.length; i++) {
        const item = group.items[i];
        const code = refSnippet(item.loc)
          ? '<span class="lsp-peek-code"></span>'
          : '<span class="lsp-peek-code lsp-refs-nopreview">' + esc(st.noPreviewText) + '</span>';
        html += '<div class="lsp-refs-item" role="option" data-idx="' + item.idx + '">' +
          '<span class="lsp-peek-num">' + item.loc.line + '</span>' + code + '</div>';
      }
    }
    html += '</div></div><div class="lsp-peek-hint">' + esc(st.refsHintText) + '</div>';
    panel.innerHTML = html;
    fillRefRows(panel, locs, word);

    st.refs = panel;
    st.refsLocs = locs;
    st.refsWord = word;
    selectRef(panel, 0);
    // The widget sits under the clicked line as a Pierre annotation, which
    // mounts on the next render; float it when that line is not rendered.
    Promise.resolve(st.mountLineWidget ? st.mountLineWidget(at.path, at.line, panel) : false)
      .then(function (mounted) {
        if (st.refs !== panel) return;
        if (!mounted) {
          panel.classList.add('lsp-refs-floating');
          document.body.appendChild(panel);
        }
        panel.scrollIntoView({ block: 'nearest', behavior: 'instant' });
        panel.querySelector('.lsp-refs-list').focus({ preventScroll: true });
      });

    panel.querySelector('.lsp-peek-close').addEventListener('click', hideRefs);
    panel.addEventListener('click', function (e) {
      const row = e.target.closest('.lsp-refs-item');
      if (row) selectRef(panel, parseInt(row.dataset.idx, 10));
    });
    panel.addEventListener('dblclick', function (e) {
      const row = e.target.closest('.lsp-refs-item');
      if (row) openRef(parseInt(row.dataset.idx, 10));
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
    if (!eligibleLineHit(e)) return;
    e.preventDefault();
  }

  function onGlobalMousedown(e) {
    suppressChordSelection(e);
    // A hover armed just before the click must not resurrect the tooltip the
    // click is dismissing.
    clearTimeout(st.hoverTimer);
    if (st.peek && !st.peek.contains(e.target)) hidePeek();
    if (st.tooltip && !st.tooltip.hidden && !st.tooltip.contains(e.target)) hideTooltip();
  }

  function onScroll() {
    clearTimeout(st.hoverTimer);
    hideTooltip();
  }

  // Idempotent; call once after /api/config confirms lsp_available.
  function init(opts) {
    if (st) return;
    st = {
      renderMarkdown: opts.renderMarkdown,
      jumpToLocation: opts.jumpToLocation,
      canJump: opts.canJump || function (loc) { return !!loc.in_session; },
      mountLineWidget: opts.mountLineWidget || null,
      unmountLineWidget: opts.unmountLineWidget || null,
      languageForPath: opts.languageForPath || null,
      toast: opts.toast || function () {},
      defHintText: opts.defHintText || '⌘/Ctrl+Click: go to definition · +Shift: find references',
      notFoundText: opts.notFoundText || 'No definition found',
      refsNotFoundText: opts.refsNotFoundText || 'No references found',
      refText: opts.refText || 'reference',
      refsText: opts.refsText || 'references',
      refsTruncatedText: opts.refsTruncatedText || '(list truncated)',
      refsHintText: opts.refsHintText || 'Click / ↑↓: preview · Double-click / Enter: jump · Esc: close',
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
      refsActive: 0,
      refsWord: '',
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
    lineHitFromPath: lineHitFromPath,
    findHunkForLine: findHunkForLine,
    findGapForLine: findGapForLine,
    groupLocationsByFile: groupLocationsByFile,
    refSnippet: refSnippet,
    wordAt: wordAt,
    symbolRange: symbolRange,
    makeExtensionMatcher: makeExtensionMatcher,
    viewURL: viewURL,
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
