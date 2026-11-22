'use strict';
(function (root, factory) {
  var api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else {
    root.crit = root.crit || {};
    root.crit.live = root.crit.live || {};
    root.crit.live.origin = api;
  }
})(typeof window !== 'undefined' ? window : globalThis, function () {
  function makeOriginGuard(opts) {
    var expectSource = opts.expectSource;
    var expectOrigin = opts.expectOrigin;
    return function check(ev) {
      if (ev.source !== expectSource) return false;
      if (ev.origin !== expectOrigin) return false;
      return true;
    };
  }
  // agentOrigins returns the origin pair the chrome uses with the agent frame:
  // `expect` is the event.origin inbound messages must carry, `target` is the
  // targetOrigin for outbound postMessage.
  //
  // Live mode: the agent runs on the proxy port, a real origin.
  // Preview mode: /preview-content is served with a CSP sandbox (no
  // allow-same-origin), so the document has an opaque origin. Its messages
  // arrive with origin "null", and an opaque origin can only be addressed
  // with "*". The source check in makeOriginGuard (event.source must be the
  // iframe's contentWindow) is what ties inbound messages to the frame.
  function agentOrigins(opts) {
    if (opts.isPreview) return { expect: 'null', target: '*' };
    var origin = 'http://' + (opts.proxyHost || 'localhost') + ':' + (opts.proxyPort || 0);
    return { expect: origin, target: origin };
  }
  return { makeOriginGuard, agentOrigins };
});
