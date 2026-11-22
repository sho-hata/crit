// crit-view.js — /view?path=X&line=N: one whole repo file on Pierre with
// crit-lsp wired up, read-only. Opened from the peek popup's "Open full file".
//
// Dependencies (window.crit.* namespaces read): shared, pierreAdapter,
// pierreView, pierreRuntime, pierreDOM, themeBoost, themePalette, palettes,
// codeHighlight, lsp, commentHtml; window.PierreDiffs via critPierreReady.
(function () {
  'use strict';

  function parseViewParams(search) {
    const q = new URLSearchParams(search || '');
    const line = parseInt(q.get('line') || '', 10);
    return { path: q.get('path') || '', line: line > 0 ? line : 0 };
  }

  function lineCount(content) {
    const c = content || '';
    if (!c) return 0;
    return c.split('\n').length - (c.endsWith('\n') ? 1 : 0);
  }

  const JUMP_FLASH_MS = 1600;

  function boot() {
    const shared = window.crit.shared;
    const adapter = window.crit.pierreAdapter;
    const pierreDOM = window.crit.pierreDOM;
    const container = document.getElementById('filesContainer');
    const getSetting = shared.getSetting;

    let P = null;
    let view = null;
    let decorations = null;
    let workers = null;
    let file = null;           // { path, content, fileType, status, viewMode, collapsed, lazy }
    let widget = null;         // { line, el } — crit-lsp's references panel
    let flashTimer = null;
    let lspReady = false;

    shared.applyThemeFromCookie();
    shared.applyDisplayAttributes();

    const pathChip = document.createElement('span');
    pathChip.className = 'header-chip crit-view-path';
    pathChip.id = 'viewPath';
    document.querySelector('.header-left').appendChild(pathChip);

    function setPath(path) {
      pathChip.textContent = path;
      pathChip.title = path;
      document.title = (path ? path + ' · ' : '') + 'Crit';
    }

    function showMessage(text) {
      if (view) { view.destroy(); view = null; }
      container.innerHTML = '';
      const el = document.createElement('div');
      el.className = 'crit-view-message';
      el.textContent = text;
      container.appendChild(el);
    }

    function settings() {
      const raw = shared.getCookie('crit-settings');
      try { return raw ? JSON.parse(raw) || {} : {}; } catch { return {}; }
    }

    function displayOptions() {
      return Object.assign(adapter.displayOptions(getSetting), {
        theme: window.crit.themeBoost.themes(P,
          window.crit.themePalette.pair(settings(), window.crit.palettes),
          getSetting('boostContrast', 'off') === 'on'),
      });
    }

    function workerPool() {
      if (!workers) {
        workers = window.crit.pierreRuntime.createWorkerController({
          create: function () {
            const display = displayOptions();
            return P.getOrCreateWorkerPoolSingleton({
              poolOptions: {
                workerFactory: function () { return new Worker('pierre/pierre-worker.js', { type: 'module' }); },
                poolSize: Math.max(1, Math.min(4, (navigator.hardwareConcurrency || 4) - 1)),
              },
              highlighterOptions: {
                theme: display.theme,
                lineDiffType: display.lineDiffType,
                preferredHighlighter: 'shiki-js',
              },
            });
          },
          onFailure: function (error) {
            console.warn('Highlight workers unavailable; rendering without workers.', error);
            window.crit.codeHighlight.configure({ pool: poolReady });
            requestAnimationFrame(function () {
              if (view) { view.destroy(); view = null; }
              if (file) ensureView().setFiles([file]);
            });
          },
        });
      }
      return workers.get();
    }

    function poolReady() {
      return window.critPierreReady.then(function (loaded) { return loaded ? workerPool() : null; });
    }

    function languageForPath(path) {
      return adapter.languageOverride(path) ||
        (typeof P.getFiletypeFromFileName === 'function' ? P.getFiletypeFromFileName(path) : null);
    }

    function buildHeader(path) {
      const header = document.createElement('div');
      header.className = 'crit-view-file-header';
      const name = document.createElement('span');
      name.className = 'crit-view-file-name';
      name.textContent = path;
      header.appendChild(name);
      if (file && file.path === path) {
        const count = document.createElement('span');
        count.className = 'crit-view-file-lines';
        count.textContent = lineCount(file.content) + ' lines';
        header.appendChild(count);
      }
      return header;
    }

    function annotationsFor(f) {
      if (!widget || !file || f.path !== file.path) return [];
      return [{ side: 'additions', lineNumber: widget.line, metadata: { kind: 'lsp-widget', id: f.path } }];
    }

    function ensureView() {
      if (view) return view;
      // Makes #filesContainer the scroll pane CodeView scrolls (style.css).
      document.body.classList.add('pierre-review');
      container.innerHTML = '';
      decorations = decorations || pierreDOM.createDecorations();
      const display = displayOptions();
      view = window.crit.pierreView.createPierreView(Object.assign({
        pierre: P,
        root: container,
        workerPool: workerPool(),
        diffStyle: 'unified',
        themeType: adapter.themeTypeFor(getSetting('theme', 'system')),
      }, display, {
        layout: { gap: 0, paddingTop: 0, paddingBottom: 80 },
        itemMetrics: { paddingTop: 0, paddingBottom: 1 },
        annotations: annotationsFor,
        isFileView: function () { return true; },
        loadFile: function () { return Promise.resolve(file); },
        buildHeader: buildHeader,
        buildAnnotation: function (kind) {
          return kind === 'lsp-widget' && widget ? widget.el : null;
        },
        onPostRender: function (path, node, phase) {
          const host = pierreDOM.hostFor(node);
          if (!host) return;
          if (phase === 'unmount') { decorations.unmount(host); return; }
          host.toggleAttribute('data-crit-file-view', true);
          decorations.mount(host, path, []);
        },
        unsafeCSS: pierreDOM.unsafeCSS,
        onGutterUtilityClick: function () {},
        onLineNumberClick: function () {},
      }));
      view.setRenderOptions({ enableGutterUtility: false });
      return view;
    }

    function flashLine(line) {
      if (!decorations || !file) return;
      const rows = pierreDOM.pierreLineSelectors(file, [{ start: line, end: line, old: false }], false, true, []);
      clearTimeout(flashTimer);
      decorations.setRangeCSS(rows.length ? rows.join(',\n') + ' { animation: lsp-jump-flash ' + JUMP_FLASH_MS + 'ms ease-out; }' : '');
      flashTimer = setTimeout(function () { if (decorations) decorations.setRangeCSS(''); }, JUMP_FLASH_MS);
    }

    function rowFor(line) {
      const host = container.querySelector('diffs-container');
      const root = host && host.shadowRoot;
      return root ? root.querySelector('code[data-code] [data-content] > [data-line="' + line + '"]') : null;
    }

    // Right after setFiles the item is still measuring, so a scroll can land
    // short of a far line: re-issue it until the row is on screen.
    function reveal(line) {
      if (!view || !file || !line || line > lineCount(file.content)) return Promise.resolve(false);
      const v = view;
      const path = file.path;
      return new Promise(function (resolve) {
        let frames = 0;
        function onScreen() {
          const row = rowFor(line);
          if (!row) return false;
          const r = row.getBoundingClientRect();
          return r.height > 0 && r.top >= 0 && r.bottom <= window.innerHeight;
        }
        function attempt() {
          if (view !== v || !file || file.path !== path) { resolve(false); return; }
          v.scrollToLine(path, line, '', 'center').then(function () {
            requestAnimationFrame(function () {
              if (onScreen() || ++frames >= 30) {
                v.setSelectedLine(path, line, '');
                flashLine(line);
                resolve(true);
              } else {
                attempt();
              }
            });
          });
        }
        attempt();
      });
    }

    function setURL(path, line, push) {
      const url = window.crit.lsp.viewURL(path, line);
      if (push) history.pushState({ path: path, line: line }, '', url);
      else history.replaceState({ path: path, line: line }, '', url);
    }

    let loadSeq = 0;
    function show(path, line, push) {
      const seq = ++loadSeq;
      return fetch('/api/file?path=' + encodeURIComponent(path))
        .then(function (r) {
          if (r.status === 404) throw new Error('File not found: ' + path);
          if (!r.ok) throw new Error('Could not load ' + path + ' (HTTP ' + r.status + ')');
          return r.json();
        })
        .then(function (snap) {
          if (seq !== loadSeq) return false;
          widget = null;
          file = {
            path: snap.path || path, content: snap.content || '', fileType: snap.file_type || 'code',
            status: snap.status || '', viewMode: 'document', collapsed: false, lazy: false,
          };
          setPath(file.path);
          setURL(file.path, line, push);
          ensureView().setFiles([file]);
          return reveal(line).then(function () { return true; });
        })
        .catch(function (err) {
          if (seq !== loadSeq) return false;
          if (file) shared.showToast(err.message, { kind: 'error' }); else showMessage(err.message);
          return false;
        });
    }

    function jumpToLocation(loc) {
      if (!loc || !loc.in_repo) return Promise.resolve(false);
      if (file && loc.path === file.path) {
        setURL(file.path, loc.line, true);
        return reveal(loc.line);
      }
      return show(loc.path, loc.line, true);
    }

    function mountLineWidget(path, line, el) {
      if (!view || !file || path !== file.path || line < 1 || line > lineCount(file.content)) return Promise.resolve(false);
      unmountLineWidget();
      widget = { line: line, el: el };
      view.refreshFile(file, ['lsp-widget:' + path]);
      return new Promise(function (resolve) {
        let frames = 0;
        (function poll() {
          if (el.isConnected) { resolve(true); return; }
          if (++frames >= 60 || !widget || widget.el !== el) {
            unmountLineWidget(el);
            resolve(false);
            return;
          }
          requestAnimationFrame(poll);
        })();
      });
    }

    function unmountLineWidget(el) {
      if (!widget || (el && widget.el !== el)) return;
      widget = null;
      if (view && file) view.refreshFile(file, ['lsp-widget:' + file.path]);
    }

    function initLSP(config) {
      if (lspReady || !config.lsp_available || !window.crit.lsp) return;
      lspReady = true;
      window.crit.lsp.init({
        canJump: function (loc) { return !!loc.in_repo; },
        jumpToLocation: jumpToLocation,
        mountLineWidget: mountLineWidget,
        unmountLineWidget: unmountLineWidget,
        languageForPath: languageForPath,
        toast: function (message) { shared.showToast(message); },
        extensions: config.lsp_extensions,
      });
    }

    const params = parseViewParams(window.location.search);
    setPath(params.path);
    if (!params.path) {
      showMessage('No file given. Open /view?path=<repo-relative path>[&line=N].');
      return;
    }

    window.addEventListener('popstate', function () {
      const p = parseViewParams(window.location.search);
      if (!p.path) return;
      if (!file || p.path !== file.path) { show(p.path, p.line, false); return; }
      if (p.line) { reveal(p.line); return; }
      if (view) {
        view.setSelectedLine(null);
        view.scrollToFile(file.path, 'start');
      }
    });

    Promise.all([shared.waitForSession(), window.critPierreReady])
      .then(function (results) {
        P = results[1];
        if (!P) throw new Error('The code renderer failed to load.');
        window.crit.codeHighlight.configure({ pool: poolReady });
        return fetch('/api/config').then(function (r) { return r.ok ? r.json() : {}; }).catch(function () { return {}; });
      })
      .then(function (config) {
        return show(params.path, params.line, false).then(function (shown) {
          if (shown) initLSP(config);
        });
      })
      .catch(function (err) {
        showMessage(err && err.message ? err.message : String(err));
      });
  }

  const api = { parseViewParams: parseViewParams, lineCount: lineCount, boot: boot };
  if (typeof window !== 'undefined') {
    window.crit = window.crit || {};
    window.crit.view = api;
    if (document.getElementById('filesContainer') && window.location.pathname === '/view') boot();
  }
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  }
})();
