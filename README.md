# Maintenance Manager for Envoy®

**Flip a switch, your apps show a maintenance page. Flip it back, they're live. Their routes never notice.**

Website: [maintenance-manager.oss.life.li](https://maintenance-manager.oss.life.li)

A self-contained Helm chart that puts web apps behind **maintenance pages,
interstitials and banners** on any Kubernetes® cluster running **Envoy Gateway
≥ 1.8**, without touching the apps or their HTTPRoutes.

- **Hands off your routes.** It only reads HTTPRoutes, so it works where a
  platform owns and regenerates them.
- **Zero overhead at rest.** No window armed, no extra hop: Envoy never calls it.
- **Fail-open.** If it is down, slow or misconfigured, traffic flows as if it
  was never there.
- **Plays nice with platform auth.** Its policy merges into Gateway-level
  SecurityPolicies instead of replacing them.
- **No database.** State lives in three custom resources.

It needs **Envoy Gateway ≥ 1.8** (tested on v1.8.3 and v1.9.1) with the Gateway
API it installs, and one install per cluster.

## How it works

You describe three things as custom resources, usually in the editor:

- a **MaintenanceService**: which HTTPRoutes make up an app (a label selector);
- a **MaintenancePage**: what users see (an HTML template, and per language a title, a message and the interstitial's button text; see [Your own page](#your-own-page));
- a **MaintenanceWindow**: when, and what happens, as phases: `banner` (pass
  through with a header the app can show), `interstitial` (a page once per
  session), `block` (503 with your page, or `application/problem+json` for
  APIs, plus `Retry-After`).

Fifteen minutes before a window starts, maintenance-manager attaches one
SecurityPolicy per app namespace to the matching routes, so Envoy asks it about
every request; after the window it removes it. Its policy merges with
Gateway-level policies your platform already has.

**When your platform already puts a policy on a route.** Envoy Gateway allows
one SecurityPolicy per route, and some platforms take that slot on every route
(for example with an IP allowlist). Such routes are armed through an
EnvoyExtensionPolicy instead, a different kind of policy that asks
maintenance-manager the same question in another protocol (ext_proc). The
editor shows them as armed "(extension policy)". Two differences: the
platform's own authentication on the route (OIDC, JWT, basic auth, an external
auth service, API keys) answers before maintenance-manager, so anonymous users
see its login, not your page, and an external auth service sees the bypass
header before it is removed; and maintenance-manager receives every request
header, not only the ones it reads. Only a route where the platform also holds the
extension slot is left alone, and shown as a conflict.

## Install

**By digest**, after verifying it was built here. Every
[release](https://github.com/prosperity-solutions/maintenance-manager/releases)
lists its digests with both commands ready to paste (tested with Helm 4):

```sh
gh attestation verify oci://ghcr.io/prosperity-solutions/charts/maintenance-manager@sha256:<chart digest> \
  --repo prosperity-solutions/maintenance-manager \
  --signer-workflow prosperity-solutions/maintenance-manager/.github/workflows/ci.yml \
  --source-ref refs/heads/main --deny-self-hosted-runners

helm install mm oci://ghcr.io/prosperity-solutions/charts/maintenance-manager@sha256:<chart digest> \
  -n maintenance-manager --create-namespace --set editor.password=<at least 12 characters>
```

The chart pins the image by digest; the same `gh attestation verify` with
`oci://ghcr.io/prosperity-solutions/maintenance-manager@sha256:<image digest>`
checks the image. More in [docs/security.md](docs/security.md).

**Uninstall:** `helm uninstall mm -n maintenance-manager`. A pre-delete hook
removes every SecurityPolicy, EnvoyExtensionPolicy and ReferenceGrant it
created; your windows, pages and services stay (the CRDs are kept). If the hook
fails, run the uninstall again; `--no-hooks` leaves the policies behind. Going
back to a version before the extension policy existed leaves those behind too:
`kubectl delete envoyextensionpolicy -A -l
maintenance-manager.oss.life.li/owner=<release namespace>` removes them.

### The 60-second drain

Envoy Gateway adds one filter per policy to the listener, so attaching or
removing one changes the listener, and Envoy drains it: open connections
close gracefully over 60 seconds and reconnect. For plain HTTP that is the whole
port; for HTTPS it is that host's TLS chain. Normal requests do not notice;
WebSockets and server-sent events reconnect. This happens when a window arms
(15 minutes before it starts, so not during maintenance) and when it disarms.
Pick quiet moments for the first install and test.

### Letting your team through

Set `bypass.token` (at least 16 characters) and send
`X-Maintenance-Manager-Bypass: <token>` with a browser header extension: your
requests pass as if no window were active. The header is stripped before your
app on armed routes. At rest Envoy never calls maintenance-manager, so it
passes through untouched: send it only during maintenance. There is no bypass
link and no bypass cookie, so the token never lands in URLs or logs. After
changing it, restart the pods (`kubectl -n maintenance-manager rollout restart
deploy/maintenance-manager`).

## The editor

A plain web editor ships with the chart: a dashboard (which windows are armed or
active, which routes, whether Envoy accepted the policy, and conflicts in red),
forms for services, pages and windows, a page preview, and **Start next phase
now**. It listens on port 8080 inside the cluster only:

```sh
kubectl -n <namespace> port-forward svc/maintenance-manager 8080
# open http://localhost:8080, user admin, the password from editor.password
# (or the editor-password key of editor.existingSecret)
```

Want a URL instead? Point an HTTPRoute of your own at the `maintenance-manager`
Service, port 8080, at path `/` (the editor's links are root-relative), and put
TLS and your own login (OIDC, an IP allowlist) in front of it. Keep that route
out of every MaintenanceService's selector, or a block phase blocks the editor
too.

If the editor is ever out of reach, stop a window from the command line:

```sh
kubectl -n <namespace> patch mmw <window> --type=merge -p '{"spec":{"enabled":false}}'
```

> **Treat the editor password like a deploy credential.** Whoever holds it can
> serve any HTML and JavaScript on every gated app's origin. It must be at least
> 12 characters (shorter, the editor stays off). Login attempts are limited to
> one per second per client address, so a guessing pod does not lock out your
> port-forward.

## Your own page

The built-in page is plain and works in every language you give it texts for.
For your own look, give the MaintenancePage an `html` template: a Go
`html/template` (not Handlebars) with `{{.Title}}`, `{{.Message}}`,
`{{.Locale}}`, `{{.Until}}` (shown in the visitor's own time zone),
`{{.Continue}}` (the interstitial's button text, `continueLabel` per language)
and `{{.Interstitial}}`, plus where it sits in the maintenance: `{{.Phase}}`,
`{{.Previous}}`, `{{.Next}}`, `{{.Downtime}}` ("maintenance from X to Y"),
`{{.Window}}` and `{{.ContinueAfter}}`. Title and message stay plain text:
the in-app banner and API errors use them too. An interstitial phase sets how
long its page waits before continuing by itself
(`interstitial: {autoContinueSeconds: 8}`, `0` = only by the button).

```html
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .Until}}<p>Back at {{.Until}}</p>{{end}}
{{if .Interstitial}}<a href="">{{.Continue}}</a>{{end}}
```

Keep it self-contained (CSS in `<style>`, images inline): during a block, the
app's own host serves nothing else. **[docs/pages.md](docs/pages.md)** has every
rule, a full branded example, and a brief you can paste into a coding agent to
have it write the page for you.

## Telling users in the app

Most apps want a banner before maintenance starts, not just a page during it.
Two ways, both in **[docs/apps.md](docs/apps.md)** with exact formats and a
brief to paste into a coding agent:

1. **The JS client** (below): one script tag, restyled with five CSS
   variables.
2. **Your own banner:** read `GET /__maintenance-manager/status`
   (`{"action":"banner","title":…,"message":…,"downtime":{"start":…,"end":…}}`) and draw it with
   your own components.

Add the client, a single dependency-free file (under 4 KB gzipped), to your
pages as a classic script tag (not bundled, not `type="module"`):

```html
<script src="/static/maintenance-manager.js"
  data-dismiss="click" data-delay="8" data-poll="60" data-locale="de-CH" defer></script>
```

- **Self-host it** (recommended, works with a strict CSP `script-src 'self'`):
  every release attaches `maintenance-manager.js`, with its checksum in
  `SHA256SUMS`.
- Or load it from jsDelivr, pinned to a release:
  `https://cdn.jsdelivr.net/gh/prosperity-solutions/maintenance-manager@vX.Y.Z/client/maintenance-manager.min.js`.

| attribute | values | default |
|---|---|---|
| `data-dismiss` | `auto` (after `data-delay` seconds) or `click` | `click` |
| `data-delay` | seconds | `8` |
| `data-poll` | seconds, 5–3600: a banner shows within about this long after its phase starts | `60` |
| `data-locale` | BCP 47 tag | the browser's language |
| `data-frame` | `off`: inside an iframe, do nothing (no polling, no banner, no block overlay), so a page framed by another page with the client shows one banner, not two. Only when the outer page is on the same host: otherwise the frame's own maintenance shows nowhere | `on` |

While the tab is visible, it asks `/__maintenance-manager/status` on your own
host. That path only answers while a window is armed; otherwise your app
answers it (usually a 404), the client shows nothing and polls less often (up
to every 5 minutes). Banner and interstitial phases show a banner at the top;
a block phase covers the page, and when it ends every open tab reloads within
30 seconds (spread out, so they do not all hit the app at once). It renders in
a Shadow DOM; restyle it with `--mm-bg`, `--mm-fg`, `--mm-accent`, `--mm-font`
and `--mm-z` on `#maintenance-manager`. Under `viewport-fit=cover` (iPhone
apps, Capacitor) the banner and the block page pad themselves by the safe-area
insets, so the notch and status bar never cover them; without it nothing
changes. Your app may answer the status path itself with a cheap `204` to save
the 404.

The status path sits at the root of your host, so it reports on the route that
serves `/`. If your host is split into path-scoped routes (say `/app` and
`/api` as separate HTTPRoutes), make sure the route serving `/` belongs to the
same MaintenanceService, or the banner never shows for the others. A dismissed
banner comes back when the phase's end or the downtime moves. The time the banner and
the overlay show is the downtime, from–to ("Nov 3, 2026, 3:00 – 5:00 PM"),
the same as `.Downtime` on the pages; with no block coming they show no time.
It works under a strict
CSP (`script-src 'self'`, Trusted Types): its styles are a constructed
stylesheet, not inline CSS.

## Operating it

- **Health:** `:8081/healthz`, and `:8081/readyz`, which turns ready once the
  caches have synced and the first snapshot is built.
- **Metrics** on `:8081/metrics` (Prometheus), all prefixed
  `maintenance_manager_`, with no per-host, per-route or per-window labels:
  `check_total{result}` (pass, banner, interstitial_page, interstitial_pass,
  block_html, block_problem, status, bypass, not_ready, error),
  `snapshot_builds_total`, `snapshot_build_seconds`, `snapshot_routes`,
  `snapshot_timestamp_seconds`, `controller_leader`,
  `controller_reconciles_total{result}`, `controller_writes_total`,
  `controller_armed_routes`, `controller_conflicts`, `editor_writes_total{verb}`
  and `editor_auth_total{result}`. Plus the Go runtime and controller-runtime
  metrics.
- **Profiling:** `--set pprof.addr=127.0.0.1:6060` serves `net/http/pprof`;
  reach it with `kubectl port-forward`. Off by default.
- **NetworkPolicy:** `networkPolicy.enabled=true` lets only Envoy Gateway's
  proxy pods in `networkPolicy.envoyNamespaces` reach authz on port 9001, and
  any pod of the namespaces you list reach the editor and metrics (`editorFrom`,
  `metricsFrom`). In Envoy Gateway's GatewayNamespace deploy mode, list the
  Gateways' namespaces: otherwise authz is unreachable, windows silently do
  nothing, and every request on an armed route waits the 100 ms ext_authz
  timeout before failing open. On most CNIs traffic from a pod's own node is
  always allowed (kubelet probes, hostNetwork pods), and `kubectl port-forward`
  still works.
- **Failure:** if maintenance-manager is down, slow or misconfigured, Envoy
  lets traffic through (fail-open). Disabling a window always works, from the
  editor or with `kubectl -n maintenance-manager patch mmw <window> --type=merge
  -p '{"spec":{"enabled":false}}'`.
- **Shutdown:** on SIGTERM a pod fails `/readyz`, keeps answering for 10 s,
  then stops gRPC gracefully (5 s cap), within its 30 s grace period.

How it is tested: [docs/testing.md](docs/testing.md). How it is released:
[docs/release.md](docs/release.md).

## Licence

MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

---

Envoy® and Kubernetes® are registered trademarks of The Linux Foundation. This
project is not affiliated with or endorsed by The Linux Foundation.
