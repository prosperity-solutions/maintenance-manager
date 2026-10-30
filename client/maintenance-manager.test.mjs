// Tests for the client's pure functions (HANDOVER §9, §13.1):
// node --test 'client/*.test.mjs'
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { gzipSync } from "node:zlib";

const mm = createRequire(import.meta.url)("./maintenance-manager.js");

test("config defaults and attributes", () => {
  assert.deepEqual(mm.parseConfig({}, "de-CH"), { dismiss: "click", frame: "on", delay: 8, poll: 60, locale: "de-CH" });
  assert.deepEqual(mm.parseConfig({ dismiss: "auto", frame: "off", delay: "3", poll: "30", locale: "fr" }, "de"),
    { dismiss: "auto", frame: "off", delay: 3, poll: 30, locale: "fr" });
  // Nonsense falls back to the defaults instead of polling in a tight loop
  // (or overflowing setTimeout, which fires at once).
  assert.deepEqual(mm.parseConfig({ dismiss: "never", frame: "no", delay: "-1", poll: "0" }, undefined),
    { dismiss: "click", frame: "on", delay: 8, poll: 60, locale: "en" });
  assert.equal(mm.parseConfig({ poll: "0.01" }).poll, 60);
  assert.equal(mm.parseConfig({ poll: "3e6" }).poll, 60);
  assert.equal(mm.parseConfig({ poll: "5" }).poll, 5);
  assert.equal(mm.parseConfig({ delay: "1e9" }).delay, 8);
  assert.equal(mm.parseConfig({ poll: "abc" }).poll, 60);
  assert.equal(mm.parseConfig(undefined).poll, 60);
});

test("back-off doubles from data-poll, caps at 300 s, resets on ours", () => {
  let i = 60;
  const seen = [];
  for (let k = 0; k < 5; k++) {
    i = mm.nextInterval(i, false, 60);
    seen.push(i);
  }
  assert.deepEqual(seen, [120, 240, 300, 300, 300]);
  assert.equal(mm.nextInterval(300, true, 60), 60);
  // A data-poll above the cap is kept, never shortened.
  assert.equal(mm.nextInterval(600, false, 600), 600);
  // The 300 s cap stays under the 15 min arming lead time.
  assert.ok(mm.nextInterval(1e9, false, 60) < 15 * 60);
});

test("jitter stays within ±20 %", () => {
  assert.equal(mm.jitter(60, 0), 48);
  assert.ok(Math.abs(mm.jitter(60, 0.5) - 60) < 1e-9);
  for (let k = 0; k < 1000; k++) {
    const j = mm.jitter(60, Math.random());
    assert.ok(j >= 48 && j < 72, String(j));
  }
});

test("status parsing", () => {
  const body = JSON.stringify({ action: "block", window: "w", phase: "down", title: "Back soon", message: "Upgrade",
    until: "2026-10-30T22:30:00Z", locale: "de" });
  const s = mm.parseStatus(true, 200, body);
  assert.equal(s.action, "block");
  assert.equal(s.until.toISOString(), "2026-10-30T22:30:00.000Z");
  assert.equal(s.locale, "de");
  // Not ours (no header): nothing active, whatever the body says.
  assert.equal(mm.parseStatus(false, 200, body), null);
  assert.equal(mm.parseStatus(true, 404, body), null);
  assert.equal(mm.parseStatus(true, 200, "<html>"), null);
  assert.equal(mm.parseStatus(true, 200, "null"), null);
  assert.equal(mm.parseStatus(true, 200, JSON.stringify({ action: "explode" })), null);
  const none = mm.parseStatus(true, 200, JSON.stringify({ action: "none", window: "", phase: "", title: "", message: "", until: null, locale: "en" }));
  assert.equal(none.action, "none");
  assert.equal(none.until, null);
  assert.equal(mm.parseStatus(true, 200, JSON.stringify({ action: "banner", until: "not a date" })).until, null);
  // Fields are strings, so the DOM gets text, never markup or objects.
  assert.equal(mm.parseStatus(true, 200, JSON.stringify({ action: "banner", title: { x: 1 } })).title, "[object Object]");
});

test("dismissal keys are per window, phase and end time", () => {
  assert.equal(mm.dismissKey({ window: "db", phase: "warn", until: null }), "mm:db:warn");
  const at = new Date("2026-10-30T22:00:00Z");
  assert.equal(mm.dismissKey({ window: "db", phase: "warn", until: at }), "mm:db:warn:" + at.getTime());
  assert.notEqual(mm.dismissKey({ window: "db", phase: "warn", until: null }), mm.dismissKey({ window: "db", phase: "late", until: null }));
  // A moved downtime (an extended or postponed block) is shown again, even
  // when the phase's own end stays.
  const dt = new Date("2026-11-03T15:00:00Z"), end = new Date("2026-11-03T17:00:00Z");
  assert.equal(mm.dismissKey({ window: "db", phase: "warn", until: at, from: dt, to: end }),
    "mm:db:warn:" + at.getTime() + ":" + dt.getTime() + ":" + end.getTime());
  assert.notEqual(mm.dismissKey({ window: "db", phase: "warn", until: at, from: dt, to: end }),
    mm.dismissKey({ window: "db", phase: "warn", until: at, from: dt, to: new Date("2026-11-03T18:00:00Z") }));
  // A postponed window (same name, new end) is shown again.
  assert.notEqual(mm.dismissKey({ window: "db", phase: "warn", until: at }),
    mm.dismissKey({ window: "db", phase: "warn", until: new Date("2026-10-31T22:00:00Z") }));
});

test("the file stays under 4 KB gzipped (HANDOVER §9)", () => {
  const size = gzipSync(readFileSync(new URL("./maintenance-manager.js", import.meta.url))).length;
  assert.ok(size < 4096, `${size} bytes gzipped`);
});

test("reload delay is 0–30 s", () => {
  assert.equal(mm.reloadDelay(0), 0);
  assert.ok(mm.reloadDelay(0.999999) <= 30000);
  for (let k = 0; k < 1000; k++) {
    const d = mm.reloadDelay(Math.random());
    assert.ok(d >= 0 && d < 30000);
  }
});

test("the banner time is the downtime, from–to, and nothing else", () => {
  const st = (o) => mm.parseStatus(true, 200, JSON.stringify({ action: "banner", until: "2026-11-03T14:00:00Z", ...o }));
  const dt = { start: "2026-11-03T15:00:00Z", end: "2026-11-03T17:00:00Z" };
  const s = st({ downtime: dt });
  assert.equal(s.from.toISOString(), "2026-11-03T15:00:00.000Z");
  assert.equal(s.to.toISOString(), "2026-11-03T17:00:00.000Z");
  const f = new Intl.DateTimeFormat("en", { dateStyle: "medium", timeStyle: "short" });
  assert.equal(mm.when(s, "en"), f.formatRange(s.from, s.to));
  assert.match(mm.when(s, "en"), /–/);
  // Open end: from, and a dash.
  assert.equal(mm.when(st({ downtime: { start: dt.start, end: null } }), "en"), f.format(s.from) + " –");
  // No downtime (a banner-only window), no start yet, or a broken date: no
  // time at all; the phase's own end is never shown.
  assert.equal(mm.when(st({}), "en"), "");
  assert.equal(mm.when(st({ downtime: null }), "en"), "");
  assert.equal(mm.when(st({ downtime: { start: null, end: dt.end } }), "en"), "");
  assert.equal(mm.when(st({ downtime: { start: "soon", end: dt.end } }), "en"), "");
  // A locale Intl refuses: ISO, still from–to.
  assert.equal(mm.when(s, "not a locale!"), "2026-11-03T15:00:00.000Z – 2026-11-03T17:00:00.000Z");
});
