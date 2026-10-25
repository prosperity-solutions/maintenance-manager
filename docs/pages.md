# Custom maintenance pages

What your visitors see during maintenance is a **MaintenancePage**: an
optional HTML template, plus texts per language. Leave the template empty and
you get the built-in page, which is fine for most teams. This guide is for
when you want your own look.

It is written so you can hand it to a coding agent as is: the rules are
complete, the example renders, and [the last section](#a-brief-for-an-agent)
is a prompt to paste.

## What a page is

```yaml
apiVersion: maintenance-manager.oss.life.li/v1alpha1
kind: MaintenancePage
metadata:
  name: shop-maintenance        # lowercase letters, digits, - and .
  namespace: maintenance-manager  # the release namespace
spec:
  fallbackLocale: en            # must be one of the locales below
  html: |                       # optional; empty = the built-in page
    ...your template...
  locales:                      # 1 to 32 languages
    en:
      title: "We'll be right back"        # optional, ≤ 256 characters
      message: "Scheduled maintenance."   # optional, ≤ 4096 characters
      continueLabel: "Continue to the shop"  # optional, one line, ≤ 64
    de:
      title: "Gleich wieder da"
      message: "Geplante Wartung."
```

Paste it into the editor's **Pages** form, or apply it with
`kubectl apply -f page.yaml`. The editor checks a page on save and has a
**Preview** for every language, as a block page and as an interstitial.
**After a `kubectl apply`, open the page in the editor:** the cluster accepts
a broken page without complaint, and the editor is where you see why.

**Which language a visitor gets:** their browser's `Accept-Language` entries
are tried in order of preference, each first exactly and then by its base
language (`de-AT` → `de`; a page with only `de-CH` also serves `de` and
`de-AT`). Case and `_` vs `-` don't matter. No match: `fallbackLocale`.

## Where the page shows

| Phase action | Who sees the page | Status |
|---|---|---|
| `block` | every browser request (GET or HEAD asking for `text/html`). Other requests get `application/problem+json`. | `503`, with `Retry-After` when the end is known |
| `interstitial` | a browser's first page view in this phase. The response sets a session cookie; the button (or a refresh after 8 s) reloads the page, and from then on that browser passes. The cookie is per host, so an app on several hosts shows the page once on each. | `200` |
| `banner` | nobody: the request passes, with an `X-Maintenance-Manager` response header; the JS client (through the status endpoint) or your front end shows a banner, see [docs/apps.md](apps.md) | — |

## The template

It is a **Go [`html/template`](https://pkg.go.dev/html/template)**, not
Handlebars or Jinja: fields are written `{{.Title}}`, and HTML escaping is
automatic.

### Fields

| Field | What it is |
|---|---|
| `{{.Title}}` | the language's `title`, escaped |
| `{{.Message}}` | the language's `message`, escaped. Line breaks collapse into spaces, as in any HTML, unless you style its element with `white-space:pre-line` |
| `{{.Locale}}` | the language tag that matched, e.g. `de` (use it for `<html lang>`) |
| `{{.Until}}` | when this phase ends: the next phase's start or the window's end. Empty when unknown. See [Times](#times). |
| `{{.Continue}}` | the button text: `continueLabel`, else a built-in one for the language (`Weiter` for `de`, `Continuer` for `fr`, …), else `Continue` |
| `{{.Interstitial}}` | `true` on the interstitial, `false` on the block page |

### Allowed syntax

A safe subset, checked when the page is saved:

- text and HTML, and `{{.Field}}`;
- `{{if …}} … {{else}} … {{end}}` and `{{with …}} … {{end}}`;
- the functions `eq ne lt le gt ge and or not len`, e.g.
  `{{if eq .Locale "de"}}…{{end}}`.

Not allowed: `range`, `template`, `define`, `block`, variables (`$x`), and every
other function (`printf`, `html`, `js`, `index`, …).

**What happens to a broken page:** the editor refuses to save it. If it
reaches the cluster another way (`kubectl apply`, GitOps), the whole page is
ignored: visitors get the built-in page with its built-in English texts, and
your titles, messages and button texts are lost too. The same happens when
`fallbackLocale` is not one of the locales. So check every page in the editor.

### The interstitial button

Keep it a link to the same address, with `{{.Continue}}` as its text, inside
`{{if .Interstitial}}`. Classes and other attributes are fine:

```html
{{if .Interstitial}}<a class="button" href="">{{.Continue}}</a>{{end}}
```

`href=""` reloads the page the visitor asked for; the cookie set with the page
lets them through. Style the `a` as you like, and set its text per language
with `continueLabel`.

### Times

`{{.Until}}` renders as `<time datetime="2026-10-30T22:30:00Z"
data-mm-local>22:30 UTC</time>`. When your page shows it in its text, a small
script (added once, at the end of the page) turns it into the visitor's own
time zone, written in the page's language (so keep
`<html lang="{{.Locale}}">`), with the date when it is not today: `23:30 MEZ`
on a German page in Berlin, `05:30 PM EST` on an English page in New York.
Without scripts it stays in UTC.

- Use it in element text: `<p>Back at {{.Until}}</p>`. In an attribute it is
  plain `22:30 UTC`; don't put it in `<title>`, where it shows as markup.
- Wrap it in `{{if .Until}}…{{end}}`: it is empty when the end is unknown.
- The editor's preview runs no scripts, so it shows the time in the editor's
  own time zone.

### Limits and gotchas

- **64 KiB** per rendered page (the time script does not count), and 64 KiB
  of template source. A language whose page renders larger gets the built-in
  page, with its own texts.
- **Self-contained.** During a block phase every request to the app's host is
  blocked, its images, CSS and fonts included. Put CSS in a `<style>` tag, and
  images inline (`data:` URIs or inline `<svg>`). Assets from another host load
  for visitors, but the editor's preview shows only inline images.
- **Fonts:** prefer a system font stack. A web font from another host works
  for visitors but not in the preview (it allows no fonts at all), and an
  embedded font file eats the 64 KiB quickly.
- **No JavaScript needed.** You may add your own scripts; nothing on our side
  sets a CSP for this page.
- **Dark mode:** the built-in page follows `prefers-color-scheme`; do the same.
- **Status page or contact:** a link to another host (your status page)
  works, because only the gated app's hosts are blocked.

## An example

A branded page, in two languages, with the button and the time. It is checked
by a test, so it renders as shown.

<!-- example:start -->
```html
<!doctype html>
<html lang="{{.Locale}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
:root{color-scheme:light dark;--bg:#f6f4ef;--fg:#1d1b16;--muted:#5c574c;--brand:#c2410c;--on-brand:#fff}
@media (prefers-color-scheme:dark){:root{--bg:#16140f;--fg:#f3efe6;--muted:#b9b2a3;--brand:#fb923c;--on-brand:#16140f}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;padding:1.5rem;background:var(--bg);color:var(--fg);font:1.05rem/1.6 system-ui,sans-serif}
main{max-width:32rem}
.logo{width:3rem;height:3rem;color:var(--brand)}
h1{font-size:1.8rem;line-height:1.2;margin:1rem 0 .5rem}
.until{color:var(--muted)}
.message{white-space:pre-line}
.button{display:inline-block;margin-top:1rem;padding:.7rem 1.4rem;border-radius:999px;background:var(--brand);color:var(--on-brand);font-weight:600;text-decoration:none}
footer{margin-top:2rem;font-size:.9rem;color:var(--muted)}
footer a{color:inherit}
</style>
</head>
<body>
<main>
<svg class="logo" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M12 2 2 21h20L12 2Zm0 6 5.5 11h-11L12 8Z"/></svg>
<h1>{{.Title}}</h1>
<p class="message">{{.Message}}</p>
{{if .Until}}<p class="until">{{if eq .Locale "de"}}Wieder da um{{else}}Back at{{end}} {{.Until}}</p>{{end}}
{{if .Interstitial}}<a class="button" href="">{{.Continue}}</a>{{end}}
<footer>{{if eq .Locale "de"}}Aktuelle Infos:{{else}}Live updates:{{end}} <a href="https://status.example.com">status.example.com</a></footer>
</main>
</body>
</html>
```
<!-- example:end -->

The built-in page is a good starting point too: the editor's page form shows
its source under **Start from the built-in page**, with a button that copies it
into the template field.

## A brief for an agent

Paste this into your coding agent, then add your brand (colours, logo as SVG,
tone):

```text
Write a MaintenancePage for maintenance-manager
(https://github.com/prosperity-solutions/maintenance-manager), following
docs/pages.md in that repository exactly.

Deliver one YAML file: apiVersion maintenance-manager.oss.life.li/v1alpha1,
kind MaintenancePage, with spec.fallbackLocale, spec.locales (title, message,
continueLabel per language) and spec.html.

Rules for spec.html:
- Go html/template, safe subset only: {{.Title}} {{.Message}} {{.Locale}}
  {{.Until}} {{.Continue}} {{.Interstitial}}, if/else, with, and the functions
  eq ne lt le gt ge and or not len. No range, template, define, block,
  variables or other functions.
- Self-contained: CSS in <style>, images as inline SVG or data: URIs, no files
  from the app's own host (they are blocked during maintenance).
- <html lang="{{.Locale}}">, a viewport meta tag, <meta name="robots"
  content="noindex">, light and dark mode via prefers-color-scheme.
- Show {{.Until}} only in element text and inside {{if .Until}}…{{end}}.
- The interstitial button is an <a href=""> with {{.Continue}} as its text,
  inside {{if .Interstitial}}…{{end}}; classes and styles are fine, keep
  href="".
- Fonts: a system font stack (no font files from the app's host; they are
  blocked during maintenance).
- Rendered size well under 64 KiB. Accessible: WCAG AA contrast in light
  and dark mode (button text included), readable sizes, works on a phone.
- A broken page is silently replaced by the built-in English page, so the
  YAML must be valid: fallbackLocale is one of the locales.

Languages: <list them>. Brand: <colours, logo SVG, tone of voice>.
```

Check the result in the editor's **Preview** (both actions, every language)
before a window uses it.
