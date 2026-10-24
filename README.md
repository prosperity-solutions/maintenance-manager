# Maintenance Manager for Envoy®

**Flip a switch, your apps show a maintenance page. Flip it back, they're live. Their routes never notice.**

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
- a **MaintenancePage**: what users see (an HTML template and texts per language);
- a **MaintenanceWindow**: when, and what happens, as phases: `banner` (pass
  through with a header the app can show), `interstitial` (a page once per
  session), `block` (503 with your page, or `application/problem+json` for
  APIs, plus `Retry-After`).

Fifteen minutes before a window starts, maintenance-manager attaches one
SecurityPolicy per app namespace to the matching routes, so Envoy asks it about
every request; after the window it removes it. Its policy merges with
Gateway-level policies your platform already has, and it steps aside (reported
as a conflict) where another auth policy targets the route.

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

**On Qovery:**

1. Organization settings → Helm repositories: add
   `oci://ghcr.io/prosperity-solutions/charts` (OCI, public).
2. Add a Helm service: chart `maintenance-manager`, the release's version,
   **Allow cluster-wide resources** on, and `editor.password` from a Qovery
   secret.
3. Deploy.

**Uninstall:** `helm uninstall mm -n maintenance-manager`. A pre-delete hook
removes every SecurityPolicy and ReferenceGrant it created; your windows, pages
and services stay (the CRDs are kept). If the hook fails, run the uninstall
again; `--no-hooks` leaves the policies behind.

### The 60-second drain

Envoy Gateway adds one filter per SecurityPolicy to the listener, so attaching
or removing one changes the listener, and Envoy drains it: open connections
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

## Telling users in the app

Most apps want a banner before maintenance starts, not just a page during it.
Add the client, a single dependency-free file (under 4 KB gzipped), to your
pages:

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
| `data-poll` | seconds, 5–3600; keep it under 900 so a banner shows before a window's 15-minute arming lead time runs out | `60` |
| `data-locale` | BCP 47 tag | the browser's language |

While the tab is visible, it asks `/__maintenance-manager/status` on your own
host. That path only answers while a window is armed; otherwise your app
answers it (usually a 404), the client shows nothing and polls less often (up
to every 5 minutes). Banner and interstitial phases show a banner at the top;
a block phase covers the page, and when it ends every open tab reloads within
30 seconds (spread out, so they do not all hit the app at once). It renders in
a Shadow DOM; restyle it with `--mm-bg`, `--mm-fg`, `--mm-accent`, `--mm-font`
and `--mm-z` on `#maintenance-manager`. Your app may answer the status path
itself with a cheap `204` to save the 404.

The status path sits at the root of your host, so it reports on the route that
serves `/`. If your host is split into path-scoped routes (say `/app` and
`/api` as separate HTTPRoutes), make sure the route serving `/` belongs to the
same MaintenanceService, or the banner never shows for the others. A dismissed
banner comes back when the window's end time changes. It works under a strict
CSP (`script-src 'self'`, Trusted Types): its styles are a constructed
stylesheet, not inline CSS.

## Operating it

- **Health:** `:8081/healthz`, and `:8081/readyz`, which turns ready once the
  caches have synced and the first snapshot is built.
- **Metrics** on `:8081/metrics` (Prometheus), all prefixed
  `maintenance_manager_`, with no per-host, per-route or per-window labels:
  `check_total{result}` (pass, banner, interstitial_page, interstitial_pass,
  block_html, block_problem, status, bypass, not_ready),
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
  Gateways' namespaces, or authz is unreachable and windows silently do nothing.
  Traffic from a pod's own node is always allowed (kubelet probes, hostNetwork
  pods), and `kubectl port-forward` still works.
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
