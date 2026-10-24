/*! Maintenance Manager for Envoy® client. MIT licence.
 * https://github.com/prosperity-solutions/maintenance-manager
 *
 * <script src="/static/maintenance-manager.js" data-dismiss="click"
 *   data-delay="8" data-poll="60" data-locale="de-CH" defer></script>
 *
 * Polls /__maintenance-manager/status while the tab is visible and shows a
 * banner (banner, interstitial) or a full-screen overlay (block). Styling
 * hooks: --mm-bg, --mm-fg, --mm-accent, --mm-font, --mm-z on the host
 * element #maintenance-manager. No dependencies, no build step.
 */
(function () {
  "use strict";

  var PATH = "/__maintenance-manager/status";
  var HEADER = "X-Maintenance-Manager";
  var MAX_POLL = 300; // s: under the 15 min arming lead time
  var ACTIONS = { none: 1, banner: 1, interstitial: 1, block: 1 };
  var script = typeof document !== "undefined" ? document.currentScript : null;

  // num reads seconds within [min, max], or the default. A tiny poll would
  // flood ext_authz; a huge one overflows setTimeout and fires at once.
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

  // parseStatus turns a response into a status, or null when it is not ours
  // (no header: the route is not armed, the app answered).
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

  // dismissKey is where a dismissed banner is remembered: per window and
  // phase, and per end time, so a postponed or extended window shows again.
  function dismissKey(s) {
    return "mm:" + s.window + ":" + s.phase + (s.until ? ":" + s.until.getTime() : "");
  }

  // reloadDelay: when a block ends, reload after 0–30 s, so not every tab
  // hits the freshly restarted app at once.
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
  var seq = 0;
  var blocked = false;
  var root = null;
  var shown = "";

  var CSS =
    ":host{all:initial}" +
    ".b,.o{font:15px/1.4 var(--mm-font,system-ui,sans-serif);color:var(--mm-fg,#fff);background:var(--mm-bg,#1f2937);z-index:var(--mm-z,2147483647);position:fixed;left:0;right:0}" +
    ".b{top:0;display:flex;gap:1em;align-items:center;padding:.6em 1em;box-shadow:0 2px 8px #0004}" +
    ".b div{flex:1}.b button{all:unset;cursor:pointer;font-size:1.4em;padding:0 .3em}" +
    ".b button:focus-visible{outline:2px solid var(--mm-accent,#fbbf24)}" +
    ".o{top:0;bottom:0;display:flex;align-items:center;justify-content:center;text-align:center;padding:2em}" +
    ".o div{max-width:36em}.o h1{font-size:1.8em;margin:0 0 .5em}" +
    "strong{color:var(--mm-accent,#fbbf24)}p{margin:.2em 0}";

  function el(tag, text) {
    var e = document.createElement(tag);
    if (text) e.textContent = text;
    return e;
  }

  // host creates the shadow root once. Styles go in as a constructed sheet
  // where supported, which a CSP style-src does not govern; a <style>
  // element is the fallback.
  function host() {
    if (!root) {
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

  // clear empties the shadow root (keeping a fallback <style>) without
  // innerHTML, which a Trusted Types CSP refuses.
  function clear() {
    clearTimeout(autoTimer);
    if (root) {
      var keep = root.firstChild && root.firstChild.tagName === "STYLE" ? root.firstChild : null;
      while (root.lastChild && root.lastChild !== keep) root.removeChild(root.lastChild);
    }
    if (document.body) document.body.inert = false;
    shown = "";
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
    if (key === shown) return;
    clear();
    shown = key;
    var r = host();
    var box = el("div");
    if (s.action === "block") {
      blocked = true;
      var o = el("section");
      o.className = "o";
      o.setAttribute("role", "alertdialog");
      o.setAttribute("aria-modal", "true");
      o.setAttribute("aria-labelledby", "mm-title");
      o.tabIndex = -1;
      body(s, box, "h1");
      o.appendChild(box);
      r.appendChild(o);
      if (document.body) document.body.inert = true; // the page behind is out of reach
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
      store(k);
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

  // leaveBlock: a block has ended (into nothing, or into a banner phase), so
  // the app may be back: reload, spread over 0–30 s.
  function leaveBlock() {
    if (!blocked) return;
    blocked = false;
    setTimeout(function () { location.reload(); }, reloadDelay(Math.random()));
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
    fetch(PATH + "?locale=" + encodeURIComponent(cfg.locale), { redirect: "manual", cache: "no-store", credentials: "same-origin" })
      .then(function (res) {
        var ours = res.type !== "opaqueredirect" && res.headers.has(HEADER);
        return (ours ? res.text() : Promise.resolve("")).then(function (t) {
          return { ours: ours, s: parseStatus(ours, res.status, t) };
        });
      })
      .then(function (x) {
        if (mine !== seq) return; // a newer poll answers
        interval = nextInterval(interval, x.ours, cfg.poll);
        apply(x.s);
      }, function () {
        // No answer at all (offline, a reset): keep what is shown, so a block
        // does not reload into an error page; ask again less often.
        if (mine === seq) interval = nextInterval(interval, false, cfg.poll);
      })
      .catch(function () { /* a rendering error must not stop polling */ })
      .then(function () {
        if (mine === seq) schedule();
      });
  }

  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible") poll();
    else clearTimeout(timer);
  });
  poll();
})();
