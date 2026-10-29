/*! Maintenance Manager for Envoy® client, MIT. Usage and styling hooks:
 * https://github.com/prosperity-solutions/maintenance-manager#telling-users-in-the-app */
(function () {
  "use strict";

  var PATH = "/__maintenance-manager/status";
  var HEADER = "X-Maintenance-Manager";
  var MAX_POLL = 300; // s: under the 15 min arming lead time
  var ACTIONS = { none: 1, banner: 1, interstitial: 1, block: 1 };
  var script = typeof document !== "undefined" ? document.currentScript : null;

  // num: seconds within [min, max], else the default (a tiny poll floods
  // ext_authz; a huge one overflows setTimeout).
  function num(v, def, min, max) {
    var n = Number(v);
    return v != null && v !== "" && isFinite(n) && n >= min && n <= max ? n : def;
  }

  // parseConfig reads the data-* attributes (as in element.dataset).
  function parseConfig(ds, navLang) {
    ds = ds || {};
    return {
      dismiss: ds.dismiss === "auto" ? "auto" : "click",
      delay: num(ds.delay, 8, 1, 3600),
      poll: num(ds.poll, 60, 5, 3600),
      locale: ds.locale || navLang || "en"
    };
  }

  // nextInterval: back to base on a response that is ours; otherwise double,
  // from base, up to 300 s (or base, if that is longer).
  function nextInterval(current, ours, base) {
    if (ours) return base;
    return Math.min(Math.max(current, base) * 2, Math.max(MAX_POLL, base));
  }

  // jitter spreads an interval by ±20 %. r is in [0, 1).
  function jitter(seconds, r) {
    return seconds * (0.8 + 0.4 * r);
  }

  // parseStatus: a status, or null when not ours (the app answered).
  function parseStatus(ours, httpStatus, body) {
    if (!ours || httpStatus !== 200) return null;
    var s;
    try {
      s = typeof body === "string" ? JSON.parse(body) : body;
    } catch (e) {
      return null;
    }
    if (!s || typeof s !== "object" || !ACTIONS[s.action]) return null;
    var until = s.until ? new Date(s.until) : null;
    return {
      action: s.action,
      window: String(s.window || ""),
      phase: String(s.phase || ""),
      title: String(s.title || ""),
      message: String(s.message || ""),
      until: until && !isNaN(until.getTime()) ? until : null,
      locale: String(s.locale || "")
    };
  }

  // dismissKey: per window, phase and end, so a postponed window shows again.
  function dismissKey(s) {
    return "mm:" + s.window + ":" + s.phase + (s.until ? ":" + s.until.getTime() : "");
  }

  // reloadDelay: 0–30 s, so tabs do not all hit the restarted app at once.
  function reloadDelay(r) {
    return Math.floor(r * 30000);
  }

  function formatUntil(d, locale) {
    try {
      return new Intl.DateTimeFormat(locale || undefined, { dateStyle: "medium", timeStyle: "short" }).format(d);
    } catch (e) {
      return d.toISOString();
    }
  }

  var api = {
    parseConfig: parseConfig, nextInterval: nextInterval, jitter: jitter,
    parseStatus: parseStatus, dismissKey: dismissKey, reloadDelay: reloadDelay
  };
  if (typeof module === "object" && module.exports) {
    module.exports = api;
    return;
  }
  if (typeof document === "undefined") return;

  var cfg = parseConfig(script && script.dataset, navigator.language);
  var interval = cfg.poll;
  var timer = 0;
  var autoTimer = 0;
  var reloadTimer = 0;
  var inert = false;
  var seq = 0;
  var blocked = false;
  var root = null;
  var shown = "";

  var CSS =
    ":host{all:initial}" +
    ".b,.o{font:15px/1.4 var(--mm-font,system-ui,sans-serif);color:var(--mm-fg,#fff);background:var(--mm-bg,#1f2937);z-index:var(--mm-z,2147483647);position:fixed;left:0;right:0}" +
    ".b{top:0;display:flex;gap:1em;align-items:center;padding:.6em 1em;padding:calc(.6em + env(safe-area-inset-top,0px)) calc(1em + env(safe-area-inset-right,0px)) .6em calc(1em + env(safe-area-inset-left,0px));box-shadow:0 2px 8px #0004}" +
    ".b div{flex:1}.b button{all:unset;cursor:pointer;font-size:1.4em;padding:0 .3em}" +
    ".b button:focus-visible{outline:2px solid var(--mm-accent,#fbbf24)}" +
    ".o{top:0;bottom:0;display:flex;align-items:center;justify-content:center;text-align:center;padding:2em;padding:calc(2em + env(safe-area-inset-top,0px)) calc(2em + env(safe-area-inset-right,0px)) calc(2em + env(safe-area-inset-bottom,0px)) calc(2em + env(safe-area-inset-left,0px))}" +
    ".o div{max-width:36em}.o h1{font-size:1.8em;margin:0 0 .5em}" +
    "strong{color:var(--mm-accent,#fbbf24)}p{margin:.2em 0}";

  function el(tag, text) {
    var e = document.createElement(tag);
    if (text) e.textContent = text;
    return e;
  }

  // host: the shadow root. A constructed sheet passes CSP style-src; <style>
  // is the fallback.
  function host() {
    if (!root || !root.host.isConnected) { // a framework may replace the document
      var h = el("div");
      h.id = "maintenance-manager";
      root = h.attachShadow({ mode: "open" });
      try {
        var sheet = new CSSStyleSheet();
        sheet.replaceSync(CSS);
        root.adoptedStyleSheets = [sheet];
      } catch (e) {
        root.appendChild(el("style", CSS));
      }
      document.documentElement.appendChild(h);
    }
    return root;
  }

  // clear: no innerHTML (Trusted Types); keeps a fallback <style>.
  function clear() {
    clearTimeout(autoTimer);
    if (root) {
      var keep = root.firstChild && root.firstChild.tagName === "STYLE" ? root.firstChild : null;
      while (root.lastChild && root.lastChild !== keep) root.removeChild(root.lastChild);
    }
    setInert(false);
    shown = "";
  }

  // setInert: the page behind a block; only undoes what it did.
  function setInert(on) {
    if (document.body && on !== inert) {
      document.body.inert = on;
      inert = on;
    }
  }

  function store(key) {
    try {
      localStorage.setItem(key, "1");
    } catch (e) { /* storage off: dismissal lasts this page only */ }
  }

  function dismissed(key) {
    try {
      return localStorage.getItem(key) === "1";
    } catch (e) {
      return false;
    }
  }

  function body(s, box, heading) {
    var h = el(heading, s.title);
    h.id = "mm-title";
    box.appendChild(h);
    if (s.message) box.appendChild(el("p", s.message));
    if (s.until) box.appendChild(el("p", formatUntil(s.until, s.locale || cfg.locale)));
  }

  function render(s) {
    var key = s.action + "|" + dismissKey(s) + "|" + s.title + "|" + s.message;
    if (s.action === "block") setInert(true); // again: <body> may not have existed last time
    if (key === shown) return;
    clear();
    shown = key;
    var r = host();
    var box = el("div");
    if (s.action === "block") {
      blocked = true;
      clearTimeout(reloadTimer); // blocked again before the last one's reload
      var o = el("section");
      o.className = "o";
      o.setAttribute("role", "alertdialog");
      o.setAttribute("aria-modal", "true");
      o.setAttribute("aria-labelledby", "mm-title");
      o.tabIndex = -1;
      body(s, box, "h1");
      o.appendChild(box);
      r.appendChild(o);
      setInert(true);
      o.focus();
      return;
    }
    leaveBlock();
    var k = dismissKey(s);
    if (dismissed(k)) return;
    var b = el("section");
    b.className = "b";
    b.setAttribute("role", "status");
    body(s, box, "strong");
    b.appendChild(box);
    var close = function () {
      if (shown !== key) return; // something else is showing by now
      // Auto: remember it only if the banner was actually on screen.
      if (cfg.dismiss === "click" || document.visibilityState === "visible") store(k);
      clear();
      shown = key;
    };
    if (cfg.dismiss === "click") {
      var btn = el("button", "×");
      btn.setAttribute("aria-label", "Dismiss");
      btn.addEventListener("click", close);
      b.appendChild(btn);
    } else {
      autoTimer = setTimeout(close, cfg.delay * 1000);
    }
    r.appendChild(b);
  }

  // leaveBlock: a block ended (in nothing or a banner): reload.
  function leaveBlock() {
    if (!blocked) return;
    blocked = false;
    reloadTimer = setTimeout(function () { location.reload(); }, reloadDelay(Math.random()));
  }

  function apply(s) {
    if (s && s.action !== "none") {
      render(s);
      return;
    }
    clear();
    leaveBlock();
  }

  function schedule() {
    clearTimeout(timer);
    if (document.visibilityState !== "visible") return;
    timer = setTimeout(poll, jitter(interval, Math.random()) * 1000);
  }

  function poll() {
    if (document.visibilityState !== "visible") return;
    var mine = ++seq;
    var ctl = typeof AbortController === "function" ? new AbortController() : null;
    var stop = setTimeout(function () { if (ctl) ctl.abort(); }, 20000); // a hung request must not stop polling
    fetch(PATH + "?locale=" + encodeURIComponent(cfg.locale), { redirect: "manual", cache: "no-store", credentials: "same-origin", signal: ctl && ctl.signal })
      .then(function (res) {
        var ours = res.type !== "opaqueredirect" && res.headers.has(HEADER);
        // Blocked and a 5xx not from us: the app is not back yet. Keep the overlay.
        if (blocked && !ours && res.status >= 500) throw new Error("not back yet");
        return (ours ? res.text() : Promise.resolve("")).then(function (t) {
          return { ours: ours, s: parseStatus(ours, res.status, t) };
        });
      })
      .then(function (x) {
        if (mine !== seq) return; // a newer poll answers
        interval = nextInterval(interval, x.ours, cfg.poll);
        apply(x.s);
      }, function () {
        // No answer (offline): keep what is shown, ask less often.
        if (mine === seq) interval = nextInterval(interval, false, cfg.poll);
      })
      .catch(function () { /* a rendering error must not stop polling */ })
      .then(function () {
        clearTimeout(stop);
        if (mine === seq) schedule();
      });
  }

  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible") poll();
    else clearTimeout(timer);
  });
  poll();
})();
