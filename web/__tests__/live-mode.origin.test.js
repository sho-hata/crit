'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { makeOriginGuard, agentOrigins } = require('../live-mode.origin.js');

test('agentOrigins: live mode targets and expects the proxy origin', () => {
  assert.deepEqual(
    agentOrigins({ isPreview: false, proxyHost: '127.0.0.1', proxyPort: 54322 }),
    { expect: 'http://127.0.0.1:54322', target: 'http://127.0.0.1:54322' },
  );
});

test('agentOrigins: preview mode expects the sandboxed opaque origin', () => {
  // /preview-content is CSP-sandboxed without allow-same-origin, so the
  // agent posts with origin "null" and can only be addressed with "*".
  assert.deepEqual(agentOrigins({ isPreview: true }), { expect: 'null', target: '*' });
});

test('guard in preview mode still rejects a "null"-origin message from another window', () => {
  const g = makeOriginGuard({ expectSource: 'iframeWin', expectOrigin: 'null' });
  assert.equal(g({ source: 'iframeWin', origin: 'null' }), true);
  assert.equal(g({ source: 'otherSandboxedWin', origin: 'null' }), false);
  // The crit origin itself is no longer the preview's origin.
  assert.equal(g({ source: 'iframeWin', origin: 'http://127.0.0.1:3000' }), false);
});

test('guard accepts matching source and origin', () => {
  const g = makeOriginGuard({
    expectSource: 'iframeWin',
    expectOrigin: 'http://localhost:54322',
  });
  assert.equal(g({ source: 'iframeWin', origin: 'http://localhost:54322' }), true);
});

test('guard rejects mismatched source', () => {
  const g = makeOriginGuard({
    expectSource: 'iframeWin', expectOrigin: 'http://localhost:54322',
  });
  assert.equal(g({ source: 'attacker', origin: 'http://localhost:54322' }), false);
});

test('guard rejects mismatched origin', () => {
  const g = makeOriginGuard({
    expectSource: 'iframeWin', expectOrigin: 'http://localhost:54322',
  });
  assert.equal(g({ source: 'iframeWin', origin: 'https://evil.example' }), false);
});
