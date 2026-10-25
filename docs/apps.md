# Telling users in the app

The maintenance page covers the time an app is down. Before that, users want
a warning **inside the app**: "maintenance tonight at 22:30". This guide shows
how an app learns about maintenance and shows it, in two ways:

1. **The JS client:** one script tag, a ready-made banner and block overlay.
   Restyle it with five CSS variables. Start here.
2. **Your own banner:** read the same data yourself and draw it with your
   app's own components, for when it must look fully native.

It is written so you can hand it to a coding agent: the formats are exact
(the JSON examples are checked by a test), and [the last
section](#a-brief-for-an-agent) is a prompt to paste.

## What the app can see

Nothing reaches your app until a window is **armed**: from the chart's
`armingLeadTime` (default 15 minutes) before a phase with a start time,
straight away for a phase without one or after **Start next phase now**,
until the window ends. Then, on every host whose routes belong to the
window's services:

| Phase action | Browser page loads (GET/HEAD with `Accept: text/html`) | `/__maintenance-manager/status` | Everything else (API calls, form POSTs, assets) |
|---|---|---|---|
| armed, not started yet | your app | `{"action":"none", …}` | your app |
| `banner` | your app, with the response header `X-Maintenance-Manager: window=…; phase=…; action=banner` | `{"action":"banner", …}` | your app, with the same header |
| `interstitial` | the maintenance page once per phase, browser and host (then your app, with the header) | `{"action":"interstitial", …}` | your app, with the header |
| `block` | the maintenance page, `503` | `{"action":"block", …}` | `503 application/problem+json`, with `Retry-After` when the end is known. A server-rendered form POST gets this JSON too, not the page. |
| no window armed | your app | **your app** (usually a `404`) | your app |

The header is a **response** header, added on the way out. Your backend never
sees it, and JavaScript can read it only on `fetch`/XHR responses, not on the
page load itself.

### `until` means "this phase ends"

`until` (in the JSON, and `.Until` on pages) is when the **current phase**
ends: the next phase's start, or the window's end. So in a warning `banner`
before a `block` it is when the downtime **starts**; in a `block` it is when
the app is back. Label it by `action` ("Maintenance starts at …" for
`banner`/`interstitial`, "Back at …" for `block`), or show it without a label.
It is `null` when unknown (the next phase has no start time yet).

### The status endpoint

`GET /__maintenance-manager/status` at the root of the app's own host: call it
**same-origin** only, it sends no CORS headers. While a window is armed it is
answered without touching your app. Optional `?locale=de` picks the language;
otherwise `Accept-Language` does, as for the page.

<!-- status-example:start -->
```json
{"action":"banner","window":"db-migration","phase":"warn","title":"We'll be right back","message":"Scheduled maintenance tonight.","until":"2026-10-30T22:30:00Z","locale":"en"}
```
<!-- status-example:end -->

| Field | Meaning |
|---|---|
| `action` | `none`, `banner`, `interstitial` or `block`. Treat anything else as `none`. |
| `window`, `phase` | names, for logs and for dismissal keys |
| `title`, `message` | the page's texts in the matched language. **Plain text: render them as text, never as HTML.** |
| `until` | when this phase ends (RFC 3339, UTC), or `null`; see above |
| `locale` | the language that matched |

Armed but not started, or with a valid bypass header:

<!-- status-none:start -->
```json
{"action":"none","window":"","phase":"","title":"","message":"","until":null,"locale":""}
```
<!-- status-none:end -->

It answers `200`, `Content-Type: application/json`, `Cache-Control: no-store`
and `X-Maintenance-Manager: 1`. **That header tells our answer apart from
your app's:** when no window is armed, your app answers this path. Treat
anything without it (a `404`, a login redirect, your SPA's `index.html`) as
"no maintenance". Your app may answer the path itself with a cheap `204`.
Keep it out of any service worker cache: `cache: "no-store"` does not get
past a service worker.

**One route per host answers it:** the route that serves `/`. If a host is
split into path-scoped routes (`/app`, `/api`), put the route serving `/` in
the same MaintenanceService.

### API errors during a block

<!-- problem-example:start -->
```json
{"type":"https://maintenance-manager.oss.life.li/problems/maintenance","title":"We'll be right back","status":503,"detail":"Scheduled maintenance tonight.","window":"db-migration","until":"2026-10-30T22:30:00Z"}
```
<!-- problem-example:end -->

Status `503`, `Content-Type: application/problem+json`, and `Retry-After` (an
HTTP date) when the end is known. `Retry-After` is the end of the whole run of
block phases, so it can be later than `until`. A client spots it by `type` and
shows "back at …" instead of a generic error; with no `Retry-After`, it polls
the status endpoint. More on the
[problem type page](https://maintenance-manager.oss.life.li/problems/maintenance/).

**Cross-origin APIs:** this 503 carries no CORS headers, and CORS preflights
are refused too, so a browser call to an API on another host fails as a
network error during a block. Treat a network error as "check the
same-origin status endpoint", not as a crash.

## Option 1: the JS client

One dependency-free file, under 4 KB gzipped. It polls the status endpoint
while the tab is visible (and at once when the tab becomes visible again),
shows a banner for `banner` and `interstitial`, covers the page for `block`,
and reloads every open tab 0–30 s after a block ends, but not while the app
still answers with server errors.

```html
<script src="/static/maintenance-manager.js"
  data-dismiss="click" data-poll="60" defer></script>
```

- **A classic `<script>` tag, nothing else.** Not bundled (in a CommonJS or
  webpack bundle it only exports helpers and never runs), not
  `type="module"` (its `data-*` settings would be ignored).
- **Get the file:** every release attaches `maintenance-manager.js` with its
  checksum in `SHA256SUMS`. Self-host it (works with `script-src 'self'` and
  Trusted Types). Or load it from jsDelivr, pinned to a release:
  `https://cdn.jsdelivr.net/gh/prosperity-solutions/maintenance-manager@vX.Y.Z/client/maintenance-manager.min.js`.
  During a block the app's own host serves nothing else, so open tabs keep
  the copy they loaded.
- **Settings** (attributes on the script tag):

  | attribute | values | default |
  |---|---|---|
  | `data-dismiss` | `click`, or `auto` (after `data-delay` seconds) | `click` |
  | `data-delay` | seconds | `8` |
  | `data-poll` | seconds, 5–3600. A banner shows within about this long (plus up to 20 % jitter) after its phase starts. With nothing armed it polls less often, up to every 5 minutes. | `60` |
  | `data-locale` | a language tag, e.g. `de-CH` | the browser's language |

- **Styling:** the banner lives in a Shadow DOM, so your CSS cannot break it
  and it cannot break yours. Five CSS variables pass through:

  ```css
  #maintenance-manager {
    --mm-bg: #c2410c;          /* background (default #1f2937) */
    --mm-fg: #ffffff;          /* text and the close button (default #fff) */
    --mm-accent: #fde68a;      /* the banner's title and the focus ring (default #fbbf24) */
    --mm-font: "Inter", system-ui, sans-serif;  /* default system-ui */
    --mm-z: 1000;              /* default 2147483647, the top; lower it to sit under e.g. a cookie dialog */
  }
  ```

  That is all, on purpose: the banner stays readable whatever the app's CSS
  does. For more, use option 2.
- A dismissed banner comes back in a new phase or when the end time moves.
- The texts come from the window's MaintenancePage, in the matched language,
  so the banner and the page say the same.

## Option 2: your own banner

Read the status endpoint yourself and render with your own components:

```js
// One poll. Returns the status, or {action: "none"} when no window is armed.
// Throws on a network error or timeout: keep what is shown and retry later.
async function maintenanceStatus(locale) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), 20000); // a hung request must not stop polling
  try {
    const res = await fetch("/__maintenance-manager/status" + (locale ? "?locale=" + encodeURIComponent(locale) : ""), {
      cache: "no-store", credentials: "same-origin",
      redirect: "manual",            // a login redirect from the app is not an answer
      signal: ctl.signal,
    });
    if (res.status === 200 && res.headers.get("X-Maintenance-Manager") === "1") {
      const s = await res.json();
      const known = ["none", "banner", "interstitial", "block"];
      return known.includes(s.action) ? s : { action: "none" };
    }
    // Not ours: no window is armed, or the app answered. appStatus tells a
    // block's end (2xx/4xx) from an app that is still starting (5xx).
    return { action: "none", appStatus: res.status };
  } finally {
    clearTimeout(timer);
  }
}
```

The loop around it:

- **Poll only while the tab is visible**, and poll **at once** on
  `visibilitychange` to visible: a hidden tab may have missed a block or its
  end.
- **Intervals:** 60 s while a banner shows; 15–30 s during a block; 1–5
  minutes when the answer is `none`.
- **On a network error or timeout:** keep what is shown, poll less often,
  and never reload while blocked.
- `banner` / `interstitial`: show `title` and `message` (as text) and, when
  `until` is not `null`, the time in the user's zone:
  `new Date(until).toLocaleString(locale, {dateStyle: "medium", timeStyle: "short"})`
  (a banner can run days ahead, so keep the date). Label it by action, see
  [`until`](#until-means-this-phase-ends). Remember a dismissal under
  `window + phase + until`, so a new phase or a moved end shows again.
- `block`: show a full-screen notice in the open tab. **Don't just reload into
  the maintenance page:** it does not reload itself when the block ends. When
  the status is no longer `block`, reload after a random 0–30 s delay, so not
  every tab hits the app at once, but **not while `appStatus` is a 5xx** (the
  app is still starting; reloading would show a gateway error).

**API clients and SPAs:** handle a `503` with the problem `type` above as
"maintenance", not a crash: show `detail`, retry after `Retry-After`, and
without one poll the status endpoint.

## Letting your team through

Off by default. With the chart's `bypass.token` set (at least 16
characters), requests with the header `x-maintenance-manager-bypass: <token>`
pass every phase, and the header is removed before your app sees it. The
status endpoint then answers `none`. Send it with a browser extension or your
API client, only during maintenance.

## A brief for an agent

Paste this into your coding agent in the app's repository, **together with
this file** (an agent without network access cannot open the link):

```text
Integrate maintenance notices from maintenance-manager into this app,
following docs/apps.md exactly (pasted below; also at
https://github.com/prosperity-solutions/maintenance-manager/blob/main/docs/apps.md).

Use <option 1: the JS client | option 2: our own banner component>.

Option 1: add the client as a classic <script src> tag (not bundled, not
type="module") to every HTML page, self-hosted from the release assets or
jsDelivr pinned to vX.Y.Z, with data-dismiss="click" and data-poll="60".
Style it only through --mm-bg, --mm-fg, --mm-accent (title and focus ring),
--mm-font and --mm-z on #maintenance-manager, using our design tokens.

Option 2: build a banner from GET /__maintenance-manager/status, same-origin,
cache: no-store, redirect: manual, a 20 s timeout via AbortController, and
excluded from any service worker cache. Trust the answer only when it is 200
with the response header "X-Maintenance-Manager: 1"; anything else means no
maintenance. Fields: action (none|banner|interstitial|block; anything else
is none), window, phase, title, message (plain text, never HTML), until
(RFC 3339 or null: the end of the CURRENT phase, so the start of the
downtime during a banner, the end of it during a block), locale.
Poll only while the tab is visible and at once when it becomes visible: 60 s
with a banner, 15-30 s during a block, 1-5 min when none. On network errors
keep the current state and back off. Banner/interstitial: show title,
message and until with date and time in the user's zone, labelled by action;
remember dismissals per window+phase+until. Block: show a full-screen notice;
when the status is no longer block, reload after a random 0-30 s delay, but
not while the status path answers a 5xx without that header.

Both options: treat HTTP 503 application/problem+json with type
https://maintenance-manager.oss.life.li/problems/maintenance as maintenance in
the API layer (show detail, retry after Retry-After, else poll status), and
treat network errors on cross-origin API calls during a block the same way.
Add a test for each action.
```
