// Runs the client in a small fake browser (node:vm + stubs): polling,
// rendering, dismissal, back-off, failures and the reload after a block.
// node --test 'client/*.test.mjs'
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const src = readFileSync(new URL("./maintenance-manager.js", import.meta.url), "utf8");

function node(tag) {
  return {
    tagName: tag.toUpperCase(), children: [], attrs: {}, listeners: {}, textContent: "", className: "", id: "",
    get firstChild() { return this.children[0] ?? null; },
    get lastChild() { return this.children.at(-1) ?? null; },
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) { this.children = this.children.filter((x) => x !== c); return c; },
    setAttribute(k, v) { this.attrs[k] = v; },
    addEventListener(t, f) { this.listeners[t] = f; },
    isConnected: true,
    attachShadow() { this.shadow = node("#shadow"); this.shadow.host = this; return this.shadow; },
    focus() { this.focused = true; },
    set innerHTML(v) { throw new Error("innerHTML is refused (Trusted Types)"); },
  };
}

function text(n) {
  return (n.textContent || "") + " " + n.children.map(text).join(" ");
}

function find(n, pred) {
  if (pred(n)) return n;
  for (const c of n.children) {
    const f = find(c, pred);
    if (f) return f;
  }
  return null;
}

// browser builds a fake page. Each fetch takes the next queued answer:
// {ours, status, body}, {redirect: true}, {reject: true} or {badBody: true}.
// win, when given, is the page's window (for self and top).
function browser(dataset, responses, store = new Map(), win = undefined) {
  let id = 0;
  const timers = new Map();
  const docListeners = {};
  const html = node("html");
  const docBody = node("body");
  const requests = [];
  let reloads = 0;
  const doc = {
    currentScript: { dataset },
    visibilityState: "visible",
    documentElement: html,
    body: docBody,
    createElement: node,
    addEventListener(t, f) { docListeners[t] = f; },
  };
  const ctx = {
    document: doc,
    navigator: { language: "en-GB" },
    localStorage: { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, v) },
    location: { reload: () => { reloads++; } },
    setTimeout: (f, ms) => { timers.set(++id, { f, ms }); return id; },
    clearTimeout: (t) => { timers.delete(t); },
    window: win,
    Math, JSON, Date, Number, String, Promise, Intl, isFinite, isNaN, encodeURIComponent, AbortController,
    fetch: (url, opts) => {
      requests.push({ url, opts });
      const r = responses.shift() ?? { ours: false, status: 404 };
      if (r.reject) return Promise.reject(new TypeError("Failed to fetch"));
      return Promise.resolve({
        type: r.redirect ? "opaqueredirect" : "basic",
        status: r.redirect ? 0 : (r.status ?? 200),
        headers: { has: (h) => !!r.ours && h === "X-Maintenance-Manager" },
        text: () => (r.badBody ? Promise.reject(new TypeError("network reset")) : Promise.resolve(JSON.stringify(r.body ?? {}))),
      });
    },
  };
  vm.createContext(ctx);
  vm.runInContext(src, ctx);
  // A real 5 ms pause: enough for the fake fetch's promise chain to settle.
  const flush = () => new Promise((r) => setTimeout(r, 5));
  // pollTimer is the pending poll (the client's function is named poll).
  const pollTimer = () => [...timers.entries()].filter(([, t]) => t.f.name === "poll").at(-1);
  return {
    doc, html, timers, store, requests, docListeners, flush, pollTimer,
    get reloads() { return reloads; },
    shadow: () => html.children[0]?.shadow,
    async tick() {
      const [k, t] = pollTimer();
      timers.delete(k);
      t.f();
      await flush();
    },
    reloadTimer: () => [...timers.values()].find((t) => t.f.toString().includes("location.reload")),
  };
}

const until = "2026-10-30T22:00:00Z";
const banner = { ours: true, body: { action: "banner", window: "w", phase: "warn", title: "Maintenance tonight", message: "From 22:00", until, locale: "en" } };
const block = { ours: true, body: { action: "block", window: "w", phase: "down", title: "Back soon", message: "<b>x</b>", until: null, locale: "en" } };
const none = { ours: true, body: { action: "none", window: "", phase: "", title: "", message: "", until: null, locale: "en" } };
const key = `mm:w:warn:${Date.parse(until)}`;

test("a banner shows with the right request, and a click dismisses it", async () => {
  const b = browser({ locale: "de-CH" }, [banner, banner]);
  await b.flush();
  assert.equal(b.requests[0].url, "/__maintenance-manager/status?locale=de-CH");
  const { signal, ...opts } = b.requests[0].opts;
  assert.deepEqual(opts, { redirect: "manual", cache: "no-store", credentials: "same-origin" });
  assert.ok(signal, "a hung request is aborted after 20 s");
  assert.match(text(b.shadow()), /Maintenance tonight/);
  assert.ok(find(b.shadow(), (n) => n.attrs.role === "status"));
  find(b.shadow(), (n) => n.tagName === "BUTTON").listeners.click();
  assert.equal(b.store.get(key), "1");
  assert.doesNotMatch(text(b.shadow()), /Maintenance tonight/);
});

test("a dismissal survives a reload (a fresh page with the store filled)", async () => {
  const b = browser({}, [banner], new Map([[key, "1"]]));
  await b.flush();
  assert.doesNotMatch(text(b.shadow() ?? node("x")), /Maintenance tonight/);
});

test("a changed end time shows the banner again", async () => {
  const later = { ours: true, body: { ...banner.body, until: "2026-10-30T23:00:00Z" } };
  const b = browser({}, [later], new Map([[key, "1"]]));
  await b.flush();
  assert.match(text(b.shadow()), /Maintenance tonight/);
});

test("auto dismissal after data-delay, cancelled when the status changes first", async () => {
  const b = browser({ dismiss: "auto", delay: "3" }, [banner, block]);
  await b.flush();
  assert.ok(!find(b.shadow(), (n) => n.tagName === "BUTTON"), "no button in auto mode");
  assert.ok([...b.timers.values()].some((t) => t.ms === 3000));
  await b.tick(); // the block arrives before the 3 s are up
  assert.ok(![...b.timers.values()].some((t) => t.ms === 3000), "the auto-dismiss timer is cancelled");
  assert.ok(find(b.shadow(), (n) => n.attrs.role === "alertdialog"), "the block stays up");
});

test("a block overlays the page as text, takes focus, and its end reloads within 30 s", async () => {
  const b = browser({}, [block, none]);
  await b.flush();
  const overlay = find(b.shadow(), (n) => n.attrs.role === "alertdialog");
  assert.ok(overlay && overlay.focused && overlay.attrs["aria-labelledby"] === "mm-title");
  assert.equal(b.doc.body.inert, true, "the page behind is inert");
  assert.match(text(overlay), /<b>x<\/b>/, "the message is text, never markup");
  await b.tick();
  assert.equal(b.doc.body.inert, false);
  const reload = b.reloadTimer();
  assert.ok(reload && reload.ms >= 0 && reload.ms <= 30000);
  reload.f();
  assert.equal(b.reloads, 1);
});

test("a block followed by a banner phase also reloads", async () => {
  const b = browser({}, [block, banner]);
  await b.flush();
  await b.tick();
  assert.ok(b.reloadTimer(), "leaving block for a banner reloads the app");
  assert.match(text(b.shadow()), /Maintenance tonight/);
});

test("no answer at all keeps what is shown and backs off", async () => {
  const b = browser({ poll: "60" }, [block, { reject: true }, { badBody: true }]);
  await b.flush();
  await b.tick();
  assert.ok(find(b.shadow(), (n) => n.attrs.role === "alertdialog"), "offline: the block stays");
  assert.equal(b.reloadTimer(), undefined, "offline: no reload into an error page");
  const first = b.pollTimer()[1].ms;
  assert.ok(first >= 120000 * 0.8 && first <= 120000 * 1.2, String(first));
  await b.tick();
  assert.ok(b.pollTimer(), "a failed body read does not stop polling");
});

test("responses that are not ours back off, reset, and count as nothing active", async () => {
  const b = browser({ poll: "60" }, [banner, { ours: false, status: 404 }, { redirect: true }, { ours: false, status: 503 }, none]);
  await b.flush();
  await b.tick();
  assert.doesNotMatch(text(b.shadow()), /Maintenance tonight/, "a 404 from the app: nothing active");
  const ms = () => b.pollTimer()[1].ms;
  assert.ok(ms() >= 96000 && ms() <= 144000, String(ms()));
  await b.tick();
  assert.ok(ms() >= 192000 && ms() <= 288000, String(ms()));
  await b.tick();
  assert.ok(ms() >= 240000 && ms() <= 360000, String(ms()), "capped at 300 s");
  await b.tick();
  assert.ok(ms() >= 48000 && ms() <= 72000, String(ms()), "reset on ours");
});

test("becoming visible polls at once; a hidden tab does not poll", async () => {
  const b = browser({}, [none, banner]);
  await b.flush();
  const before = b.requests.length;
  b.doc.visibilityState = "hidden";
  b.docListeners.visibilitychange();
  assert.equal(b.pollTimer(), undefined, "hidden: no poll scheduled");
  b.doc.visibilityState = "visible";
  b.docListeners.visibilitychange();
  await b.flush();
  assert.equal(b.requests.length, before + 1);
  assert.match(text(b.shadow()), /Maintenance tonight/);
});

test("during a block, a 5xx that is not ours keeps the overlay (the app is not back yet)", async () => {
  const b = browser({}, [block, { ours: false, status: 503 }, none]);
  await b.flush();
  await b.tick();
  assert.ok(find(b.shadow(), (n) => n.attrs.role === "alertdialog"), "Envoy's no-healthy-upstream must not replace the overlay");
  assert.equal(b.reloadTimer(), undefined);
  await b.tick();
  assert.ok(b.reloadTimer(), "the real end reloads");
});

test("a block followed by an already dismissed banner still reloads", async () => {
  const b = browser({}, [block, banner], new Map([[key, "1"]]));
  await b.flush();
  await b.tick();
  assert.ok(b.reloadTimer());
});

// constructedSheetPage runs the client on a page whose browser has
// CSSStyleSheet and returns its <html>.
function constructedSheetPage() {
  const ctxSheet = class { replaceSync(css) { this.css = css; } };
  const html = node("html");
  const ctx = vm.createContext({
    document: { currentScript: { dataset: {} }, visibilityState: "visible", documentElement: html, body: node("body"), createElement: node, addEventListener() {} },
    navigator: { language: "en" }, localStorage: { getItem: () => null, setItem() {} }, location: {},
    setTimeout: () => 0, clearTimeout() {}, CSSStyleSheet: ctxSheet, AbortController,
    Math, JSON, Date, Number, String, Promise, Intl, isFinite, isNaN, encodeURIComponent,
    fetch: () => Promise.resolve({ type: "basic", status: 200, headers: { has: () => true }, text: () => Promise.resolve(JSON.stringify(banner.body)) }),
  });
  vm.runInContext(src, ctx);
  return html;
}

test("constructed stylesheets are used where the browser has them", async () => {
  const b = browser({}, [banner]);
  // A second page with CSSStyleSheet available: no <style> element at all.
  const page = constructedSheetPage();
  await b.flush();
  const shadow = page.children[0].shadow;
  assert.equal(shadow.adoptedStyleSheets.length, 1);
  assert.ok(!find(shadow, (n) => n.tagName === "STYLE"), "no inline <style> for style-src to refuse");
});

test("banner and overlay pad by the safe-area insets", async () => {
  const b = browser({}, [banner]);
  const page = constructedSheetPage();
  await b.flush();
  const css = page.children[0].shadow.adoptedStyleSheets[0].css;
  // Every padding declaration of a rule, in order: a plain one first for
  // browsers without env(), then the safe-area one that wins elsewhere.
  // Under viewport-fit=cover it clears the notch; without it env() is 0.
  const paddings = (sel) => {
    const m = css.match(new RegExp("(?:^|})\\" + sel + "\\{([^}]*)\\}"));
    assert.ok(m, sel + " rule exists");
    return [...m[1].matchAll(/(?:^|;)padding[^:]*:([^;]+)/g)].map((d) => d[1]);
  };
  assert.deepEqual(paddings(".b"), [".6em 1em",
    "calc(.6em + env(safe-area-inset-top,0px)) calc(1em + env(safe-area-inset-right,0px)) .6em calc(1em + env(safe-area-inset-left,0px))"]);
  assert.deepEqual(paddings(".o"), ["2em",
    "calc(2em + env(safe-area-inset-top,0px)) calc(2em + env(safe-area-inset-right,0px)) calc(2em + env(safe-area-inset-bottom,0px)) calc(2em + env(safe-area-inset-left,0px))"]);
});

test('data-frame="off" stays silent inside an iframe, and only there', async () => {
  const framed = { self: {}, top: {} };
  const own = {};
  own.self = own;
  own.top = own;
  const throwing = { self: {}, get top() { throw new Error("SecurityError"); } };

  const silent = browser({ frame: "off" }, [block], new Map(), framed);
  await silent.flush();
  assert.equal(silent.requests.length, 0, "no polling");
  assert.equal(silent.html.children.length, 0, "no rendering");
  assert.equal(silent.timers.size, 0, "nothing scheduled");
  assert.equal(silent.docListeners.visibilitychange, undefined, "no wake-up on tab switch");

  const refused = browser({ frame: "off" }, [block], new Map(), throwing);
  await refused.flush();
  assert.equal(refused.requests.length, 0, "a refused comparison counts as framed");

  for (const [name, ds, win] of [["default, framed", {}, framed], ["off, top level", { frame: "off" }, own]]) {
    const b = browser(ds, [banner], new Map(), win);
    await b.flush();
    assert.equal(b.requests.length, 1, name + ": polls");
    assert.match(text(b.shadow()), /Maintenance tonight/, name + ": renders");
  }
});
